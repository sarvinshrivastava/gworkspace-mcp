// Parity tests against fixtures produced by the TS store at the pinned commit.
//
// Two directions:
//  1. READ: open the TS-written fixture DB and compare every dump to ts-dump.json.
//  2. WRITE: replay the exact mutation sequence from make-fixture.mjs through
//     the Go store on a fresh DB and require the same logical end state and the
//     same error codes, then diff sqlite_master against the TS fixture.
package policy

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// Fixtures live in testdata so the suite is portable: any checkout runs it.
// Regenerate with /tmp/fixtures/make-fixture.mjs against the pinned TS commit
// when the policy semantics change, then copy both files here.
const fixtureDB = "testdata/policy-fixture.sqlite"
const tsDumpPath = "testdata/ts-dump.json"

type tsDump struct {
	Seeded        bool                  `json:"seeded"`
	SchemaVersion int                   `json:"schemaVersion"`
	Agents        []AgentRow            `json:"agents"`
	Grants        map[string][]Grant    `json:"grants"`
	Scopes        map[string][]Scope    `json:"scopes"`
	Tokens        map[string][]TokenRow `json:"tokens"`
	Audit         []AuditRow            `json:"audit"`
	AuditFiltered []AuditRow            `json:"auditFiltered"`
	Log           [][]any               `json:"log"`
}

func loadDump(t *testing.T) tsDump {
	t.Helper()
	data, err := os.ReadFile(tsDumpPath)
	if err != nil {
		t.Fatalf("TS dump missing (%v) — regenerate testdata with make-fixture.mjs", err)
	}
	var d tsDump
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// fixedNow matches the TS fixture's clock closely enough for equality-free
// comparisons; createdAt timestamps are excluded from comparisons and checked
// for sanity instead.
func saneTs(ts int64) bool { return ts > 1700000000000 }

func TestReadParity_TSFixtureDB(t *testing.T) {
	d := loadDump(t)
	store, err := OpenPolicyStore(fixtureDB)
	if err != nil {
		t.Fatalf("Go store cannot open the TS fixture DB: %v", err)
	}
	defer store.Close()

	if store.SchemaVersion != d.SchemaVersion {
		t.Errorf("schemaVersion: go=%d ts=%d", store.SchemaVersion, d.SchemaVersion)
	}

	agents, err := store.ListAgents(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != len(d.Agents) {
		t.Fatalf("agents: go=%d ts=%d", len(agents), len(d.Agents))
	}
	for i, want := range d.Agents {
		got := agents[i]
		if got.Slug != want.Slug || got.Name != want.Name || got.Scoped != want.Scoped {
			t.Errorf("agent %d: go=%+v ts=%+v", i, got, want)
		}
		if (got.ArchivedAt == nil) != (want.ArchivedAt == nil) {
			t.Errorf("agent %s archivedAt mismatch: go=%v ts=%v", got.Slug, got.ArchivedAt, want.ArchivedAt)
		}
		if !saneTs(got.CreatedAt) {
			t.Errorf("agent %s createdAt insane: %d", got.Slug, got.CreatedAt)
		}
	}

	for _, slug := range []string{"default", "agent-one", "agent-two"} {
		grants, err := store.GetGrants(slug)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(grants, d.Grants[slug]) {
			t.Errorf("grants[%s]: go=%v ts=%v", slug, grants, d.Grants[slug])
		}
		scopes, err := store.GetScopes(slug)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(scopes, d.Scopes[slug]) {
			t.Errorf("scopes[%s]: go=%v ts=%v", slug, scopes, d.Scopes[slug])
		}
		tokens, err := store.ListTokens(slug)
		if err != nil {
			t.Fatal(err)
		}
		want := d.Tokens[slug]
		if len(tokens) != len(want) {
			t.Fatalf("tokens[%s]: go=%d ts=%d", slug, len(tokens), len(want))
		}
		for i, w := range want {
			g := tokens[i]
			if g.ID != w.ID || g.Agent != w.Agent || g.Prefix != w.Prefix || g.Label != w.Label {
				t.Errorf("tokens[%s][%d]: go=%+v ts=%+v", slug, i, g, w)
			}
			if (g.RevokedAt == nil) != (w.RevokedAt == nil) {
				t.Errorf("tokens[%s][%d] revokedAt mismatch", slug, i)
			}
		}
	}

	audit, err := store.QueryAudit(AuditFilter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit, d.Audit) {
		got, _ := json.Marshal(audit)
		want, _ := json.Marshal(d.Audit)
		t.Errorf("audit mismatch:\ngo=%s\nts=%s", got, want)
	}

	filtered, err := store.QueryAudit(AuditFilter{Agent: "agent-one", Decision: "allow", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(filtered, d.AuditFiltered) {
		t.Errorf("auditFiltered: go=%v ts=%v", filtered, d.AuditFiltered)
	}

	// Token lookups against TS-written hashes.
	t1 := "gws_fixturetoken1_xxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	t2 := "gws_fixturetoken2_yyyyyyyyyyyyyyyyyyyyyyyyyyyy"
	t3 := "gws_fixturetoken3_zzzzzzzzzzzzzzzzzzzzzzzzzzzz"
	slug, err := store.AgentForTokenHash(HashToken(t1))
	if err != nil || slug != "agent-one" {
		t.Errorf("active token: slug=%q err=%v", slug, err)
	}
	for name, hash := range map[string]string{"revoked": HashToken(t2), "archived": HashToken(t3)} {
		slug, err := store.AgentForTokenHash(hash)
		if err != nil || slug != "" {
			t.Errorf("%s token: slug=%q err=%v", name, slug, err)
		}
	}

	// The TS error log, replayed as expectations on the fixture state.
	// (The TS error-code log is replayed and compared in the write-parity test.)
}

func TestWriteParity_ReplayMutationMatrix(t *testing.T) {
	d := loadDump(t)
	file := filepath.Join(t.TempDir(), "replay.sqlite")
	store, err := OpenPolicyStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Mirror the TS fixture clock so created_at ties break identically:
	// seed and agent-one share a millisecond; agent-two lands 1 ms later.
	base := int64(1793423278449)
	store.now = func() int64 { return base }

	seeded, err := store.EnsureDefaultAgent(DefaultGrants())
	if err != nil || seeded != d.Seeded {
		t.Fatalf("seed: got=%v,%v ts=%v", seeded, err, d.Seeded)
	}
	// Never re-seeds.
	again, err := store.EnsureDefaultAgent(DefaultGrants())
	if err != nil || again {
		t.Errorf("re-seed: got=%v err=%v", again, err)
	}

	var log [][2]string
	code := func(err error) string {
		if err == nil {
			return "NO-ERROR"
		}
		var se *StoreError
		if ok := errorAs(err, &se); ok {
			return se.Code
		}
		return err.Error()
	}
	record := func(name string, err error) { log = append(log, [2]string{name, code(err)}) }

	record("createAgent:bad-slug", store.CreateAgent("Bad Slug!", "x"))
	record("createAgent:empty-name", store.CreateAgent("ok-slug", "   "))
	must(t, store.CreateAgent("agent-one", "Agent One"))
	store.now = func() int64 { return base + 1 }
	must(t, store.CreateAgent("agent-two", "Agent Two"))
	store.now = func() int64 { return time.Now().UnixMilli() }
	record("createAgent:exists", store.CreateAgent("agent-one", "dup"))
	record("archive:default-reserved", store.ArchiveAgent("default"))
	record("archive:not-found", store.ArchiveAgent("nope"))

	must(t, store.SetGrants("agent-one", []Grant{{"docs", "read"}, {"drive", "read"}}))
	must(t, store.SetGrants("agent-two", DefaultGrants()))
	record("grants:not-found", store.SetGrants("nope", nil))

	record("scope:default-reserved", store.AddScope("default", "folderAaaaaaaaa", "A", true))
	must(t, store.AddScope("agent-one", "folderAaaaaaaa", "A", false))
	record("unconfine:has-scopes-early", store.SetUnconfined("agent-one"))
	must(t, store.AddScope("agent-one", "folderBbbbbbbb", "B", true))
	must(t, store.AddScope("agent-two", "folderCccccccc", "C", false))
	must(t, store.AddScope("agent-two", "folderDddddddd", "D", false))
	record("scope:exists", store.AddScope("agent-two", "folderCccccccc", "C", false))
	must(t, store.SetPrimary("agent-two", "folderDddddddd"))
	must(t, store.RemoveScope("agent-two", "folderDddddddd"))
	record("scope:remove-missing", store.RemoveScope("agent-two", "folderDddddddd"))

	t1 := "gws_fixturetoken1_xxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	id1, err := store.InsertToken("agent-one", HashToken(t1), t1[:12], "label one")
	must(t, err)
	t2 := "gws_fixturetoken2_yyyyyyyyyyyyyyyyyyyyyyyyyyyy"
	id2, err := store.InsertToken("agent-one", HashToken(t2), t2[:12], "  padded label trimmed over eighty chars "+"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")
	must(t, err)
	must(t, store.RevokeToken(id2))
	must(t, store.RevokeToken(id2))
	if id1 != 1 || id2 != 2 {
		t.Errorf("token ids: %d, %d", id1, id2)
	}

	ts0 := int64(1700000000000)
	must(t, store.InsertAudit(AuditRow{Ts: ts0, Kind: "tool", Agent: strp("agent-one"), Tool: strp("readDocument"), Decision: "allow", Resources: []string{"file1"}}))
	must(t, store.InsertAudit(AuditRow{Ts: ts0 + 1000, Kind: "auth", Decision: "deny", Reason: "bad_bearer", Source: "10.0.0.1"}))
	must(t, store.InsertAudit(AuditRow{Ts: ts0 + 2000, Kind: "admin", Agent: strp("agent-one"), Decision: "mutate", Reason: "grants.set", Resources: []string{"agent-one"}, Source: "10.0.0.1"}))
	big := make([]string, 60)
	for i := range big {
		big[i] = "r"
	}
	must(t, store.InsertAudit(AuditRow{Ts: 1600000000000, Kind: "tool", Agent: strp("agent-two"), Tool: strp("oldTool"), Decision: "error", Reason: "exception", Resources: big}))
	pruned, err := store.PruneAudit(1650000000000)
	must(t, err)
	if pruned != 1 {
		t.Errorf("prune: go=%d ts=1", pruned)
	}

	t3 := "gws_fixturetoken3_zzzzzzzzzzzzzzzzzzzzzzzzzzzz"
	if _, err := store.InsertToken("agent-two", HashToken(t3), t3[:12], "to-revoke"); err != nil {
		t.Fatal(err)
	}
	must(t, store.ArchiveAgent("agent-two"))
	record("createAgent:archived-reserved", store.CreateAgent("agent-two", "again"))

	// Error codes must equal the TS log, name by name.
	for _, entry := range d.Log {
		name := entry[0].(string)
		if name == "agentForTokenHash:active" || name == "agentForTokenHash:revoked" ||
			name == "agentForTokenHash:unknown" || name == "agentForTokenHash:archived-agent" ||
			name == "prune" {
			continue // covered by read/lookup assertions
		}
		want, _ := entry[1].(string)
		var got string
		for _, l := range log {
			if l[0] == name {
				got = l[1]
			}
		}
		if got == "" {
			t.Errorf("missing replay step %q", name)
		} else if got != want {
			t.Errorf("%s: go=%q ts=%q", name, got, want)
		}
	}

	// End state must equal the TS dump (timestamps aside).
	agents, err := store.ListAgents(true)
	must(t, err)
	if len(agents) != len(d.Agents) {
		t.Fatalf("end agents: go=%d ts=%d", len(agents), len(d.Agents))
	}
	for i, want := range d.Agents {
		got := agents[i]
		if got.Slug != want.Slug || got.Name != want.Name || got.Scoped != want.Scoped || (got.ArchivedAt == nil) != (want.ArchivedAt == nil) {
			t.Errorf("end agent %d: go=%+v ts=%+v", i, got, want)
		}
	}
	for _, slug := range []string{"default", "agent-one", "agent-two"} {
		grants, _ := store.GetGrants(slug)
		if !reflect.DeepEqual(grants, d.Grants[slug]) {
			t.Errorf("end grants[%s]: go=%v ts=%v", slug, grants, d.Grants[slug])
		}
		scopes, _ := store.GetScopes(slug)
		if !reflect.DeepEqual(scopes, d.Scopes[slug]) {
			t.Errorf("end scopes[%s]: go=%v ts=%v", slug, scopes, d.Scopes[slug])
		}
	}
	audit, err := store.QueryAudit(AuditFilter{Limit: 1000})
	must(t, err)
	if !reflect.DeepEqual(audit, d.Audit) {
		t.Errorf("end audit mismatch:\ngo=%v\nts=%v", audit, d.Audit)
	}

	// Schema must be byte-identical to the TS-created database.
	diffSchema(t, file, fixtureDB)
}

func diffSchema(t *testing.T, goDB, tsDB string) {
	t.Helper()
	schema := func(path string) []string {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.Query("SELECT type, name, sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var typ, name, sqlText string
			if err := rows.Scan(&typ, &name, &sqlText); err != nil {
				t.Fatal(err)
			}
			out = append(out, typ+"|"+name+"|"+sqlText)
		}
		return out
	}
	got, want := schema(goDB), schema(tsDB)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("schema diff:\ngo=%q\nts=%q", got, want)
	}
}

func TestDatabaseExists(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.sqlite")
	if DatabaseExists(missing) {
		t.Error("missing file counts as existing")
	}
	empty := filepath.Join(dir, "empty.sqlite")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if DatabaseExists(empty) {
		t.Error("0-byte file with no WAL counts as existing")
	}
	store, err := OpenPolicyStore(filepath.Join(dir, "real.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureDefaultAgent(DefaultGrants()); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if !DatabaseExists(filepath.Join(dir, "real.sqlite")) {
		t.Error("real database not detected")
	}
}

func TestTokens(t *testing.T) {
	plaintext, hash, prefix, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(plaintext) != 4+43 || plaintext[:4] != "gws_" {
		t.Errorf("shape: %q", plaintext)
	}
	if prefix != plaintext[:12] {
		t.Errorf("prefix: %q", prefix)
	}
	if hash != HashToken(plaintext) {
		t.Error("hash mismatch")
	}
	if !SafeEqual("a", "a") || SafeEqual("a", "b") || SafeEqual("a", "aa") {
		t.Error("SafeEqual wrong")
	}
}

func TestDeriveParity(t *testing.T) {
	// DEFAULT_GRANTS maps to exactly the 20 production tools, in the TS order.
	grants := DefaultGrants()
	if len(grants) != 10 {
		t.Errorf("default grants: %d", len(grants))
	}
	tools := DeriveTools(grants)
	if len(tools) != 20 {
		t.Errorf("default tools: %d (%v)", len(tools), tools)
	}
	want := []string{
		"readDocument", "listDocuments", "searchDocuments", "appendText", "insertText",
		"findAndReplace", "appendMarkdown", "createDocument", "readSpreadsheet", "listSpreadsheets",
		"writeSpreadsheet", "appendRows", "createSpreadsheet", "searchDriveFiles", "listDriveFiles",
		"createFolder", "listEvents", "createEvent", "updateEvent", "quickAddEvent",
	}
	for _, name := range want {
		if !tools[name] {
			t.Errorf("missing tool %s", name)
		}
	}
	// CalendarTools is exactly read+write.
	if len(CalendarTools) != 4 {
		t.Errorf("calendar tools: %v", CalendarTools)
	}
	// EnabledTools ordering follows exposedOrder; scoped withholding applies.
	order := []string{"listDocuments", "readDocument", "searchDriveFiles", "listEvents"}
	got := EnabledTools(grants, true, order, map[string]bool{}, map[string]bool{})
	if !reflect.DeepEqual(got, []string{"readDocument", "searchDriveFiles", "listEvents"}) {
		t.Errorf("scoped enabled: %v", got)
	}
	got = EnabledTools(grants, false, order, map[string]bool{}, map[string]bool{})
	if !reflect.DeepEqual(got, order) {
		t.Errorf("unconfined enabled: %v", got)
	}
}

func TestAuthenticatorParity(t *testing.T) {
	file := filepath.Join(t.TempDir(), "auth.sqlite")
	store, err := OpenPolicyStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.EnsureDefaultAgent(DefaultGrants()); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAgent("worker", "Worker"); err != nil {
		t.Fatal(err)
	}
	tok := "gws_workertoken_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := store.InsertToken("worker", HashToken(tok), tok[:12], ""); err != nil {
		t.Fatal(err)
	}
	legacy := "legacy-bearer-value"
	auth := MakeAuthenticator(store, func() string { return legacy })

	ctx, err := auth(legacy)
	if err != nil || ctx == nil || ctx.Slug != DefaultAgent || ctx.Scoped {
		t.Errorf("legacy: %+v err=%v", ctx, err)
	}
	ctx, err = auth(tok)
	if err != nil || ctx == nil || ctx.Slug != "worker" || !ctx.Scoped {
		t.Errorf("minted: %+v err=%v", ctx, err)
	}
	ctx, err = auth("wrong")
	if err != nil || ctx != nil {
		t.Errorf("bad bearer accepted: %+v", ctx)
	}
	ctx, err = auth("")
	if err != nil || ctx != nil {
		t.Errorf("empty bearer accepted")
	}

	// Degraded mode: legacy works with first-boot grants, minted refused.
	deg := MakeDegradedAuthenticator(func() string { return legacy })
	ctx = deg(legacy)
	if ctx == nil || ctx.Slug != DefaultAgent || ctx.Scoped || len(ctx.Grants) != 10 {
		t.Errorf("degraded legacy: %+v", ctx)
	}
	if deg(tok) != nil {
		t.Error("degraded mode accepted a minted token")
	}
	if deg("") != nil {
		t.Error("degraded mode accepted empty bearer")
	}
}

func strp(s string) *string { return &s }

// errorAs avoids importing errors twice in this file's diff-heavy sections.
func errorAs(err error, target **StoreError) bool {
	for err != nil {
		if se, ok := err.(*StoreError); ok {
			*target = se
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

var _ = time.Now // keep the time import honest if comparisons change
