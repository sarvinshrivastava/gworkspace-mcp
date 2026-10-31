package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/audit"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/housekeeping"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/policy"
)

// pinnedNames is the full 20-tool allowlist, the shape prod passes.
func pinnedNames(t *testing.T) []string {
	t.Helper()
	var all []ToolSchema
	if err := json.Unmarshal(schemasJSON, &all); err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(all))
	for i, s := range all {
		names[i] = s.Name
	}
	return names
}

func testDeps(t *testing.T, agent *policy.AgentContext) *Deps {
	t.Helper()
	schemas, err := LoadSchemas(pinnedNames(t))
	if err != nil {
		t.Fatal(err)
	}
	return &Deps{
		Authenticate: func() func(string) (*policy.AgentContext, error) {
			return func(bearer string) (*policy.AgentContext, error) {
				if bearer == "good" {
					return agent, nil
				}
				return nil, nil
			}
		},
		Audit:       func() *audit.Audit { return nil },
		Ready:       func() bool { return true },
		DBUp:        func() bool { return true },
		AuditErrors: func() int64 { return 0 },
		Google:      drive.NewHealth(),
		PlrToken:    func() string { return "" },
		Schemas:     schemas,
	}
}

func do(t *testing.T, h http.Handler, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHealthz_ReadyAndShape(t *testing.T) {
	d := testDeps(t, nil)
	w := do(t, d.Handler(), "GET", "/healthz", "", "")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != true || body["tools"] != float64(20) || body["upstreamTools"] != float64(20) {
		t.Fatalf("body = %v", body)
	}
	if _, ok := body["google"]; !ok {
		t.Fatal("google field missing")
	}
	d.Ready = func() bool { return false }
	w = do(t, d.Handler(), "GET", "/healthz", "", "")
	if w.Code != 503 {
		t.Fatalf("not ready: status = %d", w.Code)
	}
}

// TS selectTools: an empty allowlist exposes NOTHING. config.envList returns
// nil for an unset, empty or whitespace-only TOOL_ALLOWLIST, so nil must deny
// too.
func TestLoadSchemas_EmptyAllowlistDeniesAll(t *testing.T) {
	for _, allow := range [][]string{nil, {}} {
		got, err := LoadSchemas(allow)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("LoadSchemas(%#v) exposed %d tools, want 0", allow, len(got))
		}
	}
}

func TestLoadSchemas_FiltersAndIgnoresUnknown(t *testing.T) {
	got, err := LoadSchemas([]string{"readDocument", "appendTableRows", "sendEmail"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "readDocument" {
		t.Fatalf("got %v", got)
	}
	if all, _ := LoadSchemas(pinnedNames(t)); len(all) != 20 {
		t.Fatalf("full allowlist exposed %d tools, want 20", len(all))
	}
}

func TestMcp_ReadyGate(t *testing.T) {
	d := testDeps(t, nil)
	d.Ready = func() bool { return false }
	w := do(t, d.Handler(), "POST", "/mcp", "good", `{}`)
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"starting"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestMcp_Auth(t *testing.T) {
	d := testDeps(t, &policy.AgentContext{Slug: "a", Scoped: false})
	w := do(t, d.Handler(), "POST", "/mcp", "wrong", `{}`)
	if w.Code != 401 || !strings.Contains(w.Body.String(), `"unauthorized"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// Bad bearers over two connections from one client are one audit row: the
// per-minute dedupe keys on the host, as the TS did, not on host:port.
func TestMcp_AuthFailureAuditKeysOnHost(t *testing.T) {
	store, err := policy.OpenPolicyStore(filepath.Join(t.TempDir(), "p.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	aud := audit.New(store)
	d := testDeps(t, nil)
	d.Audit = func() *audit.Audit { return aud }
	h := d.Handler()
	for _, addr := range []string{"172.18.0.5:40001", "172.18.0.5:40002"} {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
		r.RemoteAddr = addr
		r.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("%s: status %d", addr, w.Code)
		}
	}
	rows, err := store.QueryAudit(policy.AuditFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Source != "172.18.0.5" {
		t.Fatalf("want one row with source 172.18.0.5, got %+v", rows)
	}
}

func TestMcp_BatchRefused(t *testing.T) {
	d := testDeps(t, &policy.AgentContext{Slug: "a"})
	w := do(t, d.Handler(), "POST", "/mcp", "good", `[]`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "batch requests are not supported") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestMcp_ParseError(t *testing.T) {
	d := testDeps(t, &policy.AgentContext{Slug: "a"})
	w := do(t, d.Handler(), "POST", "/mcp", "good", `{not json`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `-32700`) || !strings.Contains(w.Body.String(), "Parse error") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestMcp_MethodNotAllowed(t *testing.T) {
	d := testDeps(t, nil)
	for _, m := range []string{"GET", "DELETE", "PUT"} {
		w := do(t, d.Handler(), m, "/mcp", "", "")
		if w.Code != 405 {
			t.Fatalf("%s: status = %d", m, w.Code)
		}
		if w.Header().Get("Allow") != "POST" {
			t.Fatalf("%s: Allow = %q", m, w.Header().Get("Allow"))
		}
		if !strings.Contains(w.Body.String(), "stateless; use POST") {
			t.Fatalf("%s: body = %s", m, w.Body)
		}
	}
}

func TestMcp_DisabledToolRefusalMatchesNodeSDK(t *testing.T) {
	// Legacy unconfined agent with no grants: every tool disabled. The
	// refusal must be the Node SDK's exact body, not the go-sdk's text.
	d := testDeps(t, &policy.AgentContext{Slug: "a", Scoped: false, Grants: nil})
	w := do(t, d.Handler(), "POST", "/mcp", "good",
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"readDocument","arguments":{}}}`)
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	// SSE-framed, as the TS streamable transport frames every /mcp answer.
	raw := w.Body.String()
	if !strings.HasPrefix(raw, "event: message\ndata: ") || !strings.HasSuffix(raw, "\n\n") {
		t.Fatalf("not SSE-framed: %q", raw)
	}
	var body struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(raw, "event: message\ndata: "), "\n\n")), &body); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, raw)
	}
	if body.ID != 7 || body.Error.Code != -32602 || body.Error.Message != "Tool readDocument not found" {
		t.Fatalf("body = %s", w.Body)
	}
}

func TestInternal_NotConfigured(t *testing.T) {
	d := testDeps(t, nil)
	w := do(t, d.Handler(), "POST", "/internal/housekeeping", "", "")
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"not_configured"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestInternal_ProxyHeader404(t *testing.T) {
	d := testDeps(t, nil)
	d.PlrToken = func() string { return strings.Repeat("x", 40) }
	r := httptest.NewRequest("POST", "/internal/housekeeping", nil)
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("proxied /internal: %d", w.Code)
	}
}

func TestInternal_RateLimitBeforeBearer(t *testing.T) {
	d := testDeps(t, nil)
	d.PlrToken = func() string { return strings.Repeat("x", 40) }
	h := d.Handler()
	var last *httptest.ResponseRecorder
	for i := 0; i < 11; i++ {
		last = do(t, h, "POST", "/internal/housekeeping", "wrong", "")
	}
	if last.Code != 429 {
		t.Fatalf("11th request: %d", last.Code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After missing")
	}
}

func TestInternal_WrongBearer401(t *testing.T) {
	d := testDeps(t, nil)
	d.PlrToken = func() string { return strings.Repeat("x", 40) }
	w := do(t, d.Handler(), "POST", "/internal/housekeeping", "wrong", "")
	if w.Code != 401 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// TS order for every method and path under /internal: guard first (401 with
// no or a bad bearer), then the router (404 for anything but POST
// /internal/housekeeping). One rate-limit bucket across all of it.
func TestInternal_GuardCoversEveryMethodAndPath(t *testing.T) {
	good := strings.Repeat("x", 40)
	d := testDeps(t, nil)
	d.PlrToken = func() string { return good }
	d.Housekeeping = func() *housekeeping.Runner { return nil } // degraded: no store
	h := d.Handler()
	cases := []struct {
		method, path, bearer string
		want                 int
	}{
		{"GET", "/internal/housekeeping", "", 401},
		{"GET", "/internal/housekeeping", good, 404},
		{"POST", "/internal/nope", good, 404},
		{"POST", "/internal/housekeeping/", good, 404},
		{"POST", "/internal/housekeeping", good, 503}, // no store in testDeps
	}
	for _, c := range cases {
		if w := do(t, h, c.method, c.path, c.bearer, ""); w.Code != c.want {
			t.Errorf("%s %s bearer=%t: %d, want %d", c.method, c.path, c.bearer != "", w.Code, c.want)
		}
	}
}

func TestInternal_OneRateLimitBucketAcrossPaths(t *testing.T) {
	d := testDeps(t, nil)
	d.PlrToken = func() string { return strings.Repeat("x", 40) }
	h := d.Handler()
	for i := 0; i < 5; i++ {
		do(t, h, "POST", "/internal/housekeeping", "wrong", "")
		do(t, h, "GET", "/internal/nope", "wrong", "")
	}
	if w := do(t, h, "POST", "/internal/housekeeping", "wrong", ""); w.Code != 429 {
		t.Fatalf("11th request across two paths: %d, want 429", w.Code)
	}
}

func TestUsablePlrToken(t *testing.T) {
	if UsablePlrToken("  short  ") != "" {
		t.Fatal("short token must be unusable")
	}
	tok := strings.Repeat("a", 32)
	if UsablePlrToken("  "+tok+" ") != tok {
		t.Fatal("32-char token with padding must be usable, trimmed")
	}
}

// TS internal-auth.ts:58 — proxy detection is header PRESENCE
// (req.headers[...] !== undefined), not a non-empty value.
func TestInternal_EmptyProxyHeaderStill404(t *testing.T) {
	d := testDeps(t, nil)
	d.PlrToken = func() string { return strings.Repeat("x", 40) }
	r := httptest.NewRequest("POST", "/internal/housekeeping", nil)
	r.Header["X-Forwarded-For"] = []string{""}
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("empty X-Forwarded-For: %d", w.Code)
	}
	r2 := httptest.NewRequest("POST", "/internal/housekeeping", nil)
	r2.Header["X-Real-Ip"] = []string{""}
	w2 := httptest.NewRecorder()
	d.Handler().ServeHTTP(w2, r2)
	if w2.Code != 404 {
		t.Fatalf("empty X-Real-IP: %d", w2.Code)
	}
}

// TS: (req.headers.authorization ?? "").replace(/^Bearer\s+/i, "") — \s+ is
// one or more of the JS whitespace class, case-insensitive "Bearer".
func TestBearerOf_TSRegexParity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Bearer tok", "tok"},
		{"bearer tok", "tok"},
		{"BEARER tok", "tok"},
		{"Bearer  tok", "tok"}, // multiple spaces
		{"Bearer\ttok", "tok"}, // tab is JS \s
		{"Bearer tok", "tok"},  // U+00A0 is JS \s
		{"Bearer", "Bearer"},   // no whitespace: replace is a no-op
		{"Bearertok", "Bearertok"},
		{"tok", "tok"},
		{"", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if c.in != "" {
			r.Header.Set("Authorization", c.in)
		}
		if got := bearerOf(r); got != c.want {
			t.Fatalf("bearerOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Boot contract: Ready means "can serve tool calls", never "DB healthy".
// The legacy degraded path (policy DB absent) still serves tools, so healthz
// must stay 200 with db:false — a 503 there would autoheal-loop the container.
func TestHealthz_DegradedDBStill200(t *testing.T) {
	d := testDeps(t, nil)
	d.DBUp = func() bool { return false }
	w := do(t, d.Handler(), "GET", "/healthz", "", "")
	if w.Code != 200 {
		t.Fatalf("degraded DB healthz = %d, want 200", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != true || body["db"] != false {
		t.Fatalf("body = %v", body)
	}
	// A Google outage likewise must not flap the container: status stays 200,
	// google state is a field.
	d.Google.Probe(drive.ProbeResult{Detail: "down"})
	w = do(t, d.Handler(), "GET", "/healthz", "", "")
	if w.Code != 200 {
		t.Fatalf("google-down healthz = %d, want 200", w.Code)
	}
}
