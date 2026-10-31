// The MCP side: /healthz, POST /mcp and POST /internal/housekeeping.
// Port of src/gateway.ts — with the upstream child replaced by the in-process
// tools registry (Go-only, no Node sidecar).
package gateway

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/audit"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/enforce"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/housekeeping"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/policy"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/tools"
)

// ToolSchema is one entry of the pinned exposed-20 wire schema list.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

//go:embed schemas.json
var schemasJSON []byte

// LoadSchemas parses the pinned schemas and applies TOOL_ALLOWLIST semantics:
// the allowlist filters the exposed set once at boot, exactly as the TS
// upstream.ts boot decision. An empty allowlist denies everything by design.
func LoadSchemas(allowlist []string) ([]ToolSchema, error) {
	var all []ToolSchema
	if err := json.Unmarshal(schemasJSON, &all); err != nil {
		return nil, fmt.Errorf("pinned tool schemas: %w", err)
	}
	if len(all) != 20 {
		return nil, fmt.Errorf("pinned tool schemas: expected exactly 20, got %d", len(all))
	}
	// Deny-all on an empty allowlist, as in the TS: a service reachable by a
	// model reading documents other people can write must not expose
	// everything by default.
	if len(allowlist) == 0 {
		fmt.Println("[tools] TOOL_ALLOWLIST is empty — exposing NO tools. Set it to a comma-separated list of tool names.")
		return []ToolSchema{}, nil
	}
	allow := map[string]bool{}
	for _, name := range allowlist {
		allow[name] = true
	}
	known := map[string]bool{}
	out := make([]ToolSchema, 0, len(all))
	for _, t := range all {
		known[t.Name] = true
		if allow[t.Name] {
			out = append(out, t)
		}
	}
	// A typo in the allowlist otherwise removes a tool with no error, no
	// warning and no health signal.
	var missing []string
	for _, name := range allowlist {
		if !known[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		fmt.Printf("[tools] not found upstream, ignored: %s\n", strings.Join(missing, ", "))
	}
	return out, nil
}

// Deps is GatewayDeps. Getters, as in the TS: degraded mode swaps the
// authenticator, the store and audit arrive after boot starts.
type Deps struct {
	streamable *mcp.StreamableHTTPHandler

	Authenticate func() func(string) (*policy.AgentContext, error)
	Audit        func() *audit.Audit
	Pipeline     enforce.PipelineDeps
	Registry     map[string]tools.Handler
	GoogleTools  *tools.Google
	Ready        func() bool
	DBUp         func() bool
	AuditErrors  func() int64
	Housekeeping func() *housekeeping.Runner
	PlrToken     func() string
	Google       *drive.Health
	Schemas      []ToolSchema
}

type ctxKey struct{}

func withAgent(ctx context.Context, a *policy.AgentContext) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

func agentOf(ctx context.Context) *policy.AgentContext {
	a, _ := ctx.Value(ctxKey{}).(*policy.AgentContext)
	return a
}

// ToolsFor: this agent's enabled tools, in exposed (pinned) order. The
// withheld sets are empty in the Go port: the pinned schemas ARE the
// allowlist, so the TS tripwire's dynamic withholding has no upstream
// surprise left to catch. The two sets stay in the signature to keep
// EnabledTools identical to TS.
func (d *Deps) ToolsFor(agent *policy.AgentContext) []string {
	order := make([]string, len(d.Schemas))
	for i, t := range d.Schemas {
		order[i] = t.Name
	}
	return policy.EnabledTools(agent.Grants, agent.Scoped, order, map[string]bool{}, map[string]bool{})
}

// Handler builds the HTTP surface. Go 1.22 method patterns give the
// case-sensitive, strict routing the TS gets from its express Router.
func (d *Deps) Handler() http.Handler {
	// One stateless streamable handler; each request builds its agent's
	// server from the request context, as the TS built one McpServer per
	// request. Stateless = no Mcp-Session-Id, POST only.
	d.streamable = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return d.buildServer(agentOf(r.Context()))
	}, &mcp.StreamableHTTPOptions{Stateless: true})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", d.healthz)
	// One guard for every method under /internal/, as the TS mounted its
	// guard: a non-POST must meet the guard (not a mux 405), and every path
	// must share one rate-limit bucket.
	mux.HandleFunc("/internal/", d.withInternalGuard(d.internalRoutes))
	mux.HandleFunc("POST /mcp", d.mcpPost)
	mux.HandleFunc("/mcp", d.mcpMethodNotAllowed)
	return mux
}

// healthz: 200 once the server can serve tools, else 503 — autoheal reads
// this, so it tracks inability-to-serve only. Google state is a FIELD: a
// Google outage must not restart-loop the container. The upstream_down
// template is retired — there is no child process any more; the
// tools/upstreamTools fields both report the pinned exposed set so the
// dashboard shape is unchanged.)
func (d *Deps) healthz(w http.ResponseWriter, _ *http.Request) {
	healthy := d.Ready()
	payload := map[string]any{
		"status":        healthy,
		"tools":         len(d.Schemas),
		"upstreamTools": len(d.Schemas),
		"db":            d.DBUp(),
		"auditErrors":   d.AuditErrors(),
		"google":        d.Google.SnapshotOf(),
	}
	status := 200
	if !healthy {
		status = 503
	}
	raw, _ := json.Marshal(payload)
	writeJSON(w, status, string(raw))
}

func (d *Deps) withInternalGuard(next http.HandlerFunc) http.HandlerFunc {
	guard := newInternalGuard(d.PlrToken, func(source string) {
		if a := d.Audit(); a != nil {
			a.AuthFailure(source, "internal_bad_bearer")
		}
	})
	return func(w http.ResponseWriter, r *http.Request) {
		guard.ServeHTTP(w, r, next)
	}
}

// internalHousekeeping: pipeline-runner's daily call. Gated on the store
// only, not on ready(): a Google outage must not stop a prune and a backup.
func (d *Deps) internalHousekeeping(w http.ResponseWriter, _ *http.Request) {
	runner := d.Housekeeping()
	if runner == nil {
		writeJSON(w, 503, `{"error":"no_store"}`)
		return
	}
	res, err := runner.Run()
	if err != nil {
		// The detail stays in this log: the caller only needs the status.
		fmt.Printf("[db] housekeeping failed: %v\n", err)
		writeJSON(w, 500, `{"error":"housekeeping_failed"}`)
		return
	}
	raw, _ := json.Marshal(res)
	writeJSON(w, 200, string(raw))
}

// internalRoutes runs behind the guard. Only POST /internal/housekeeping
// exists; any other method or path under /internal ends here as a 404.
func (d *Deps) internalRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/internal/housekeeping" {
		d.internalHousekeeping(w, r)
		return
	}
	w.WriteHeader(404)
}

// mcpMethodNotAllowed: registered after the POST route, so POST still wins.
func (d *Deps) mcpMethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "POST")
	writeJSON(w, 405, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Method not allowed. This server is stateless; use POST."},"id":null}`)
}
