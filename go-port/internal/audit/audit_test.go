package audit

import (
	"path/filepath"
	"testing"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/policy"
)

func newTestAudit(t *testing.T) (*Audit, *policy.PolicyStore) {
	t.Helper()
	store, err := policy.OpenPolicyStore(filepath.Join(t.TempDir(), "p.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return New(store), store
}

func TestToolRowRoundTrip(t *testing.T) {
	a, store := newTestAudit(t)
	a.Tool("agent1", "listDocuments", "allow", "", []string{"id1", "id2"})
	rows, err := store.QueryAudit(policy.AuditFilter{Agent: "agent1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.Kind != "tool" || r.Decision != "allow" || r.Tool == nil || *r.Tool != "listDocuments" {
		t.Errorf("row mismatch: %+v", r)
	}
	if len(r.Resources) != 2 || r.Resources[0] != "id1" {
		t.Errorf("resources mismatch: %+v", r.Resources)
	}
}

func TestAuthFailureDedupe(t *testing.T) {
	a, store := newTestAudit(t)
	now := int64(1_700_000_000_000)
	a.now = func() int64 { return now }
	a.AuthFailure("mcp", "bad_bearer")
	a.AuthFailure("mcp", "bad_bearer") // same minute: dropped
	a.AuthFailure("mcp", "expired")    // different reason: kept
	rows, err := store.QueryAudit(policy.AuditFilter{Decision: "deny", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	now += 61_000 // a minute later the same failure files again
	a.AuthFailure("mcp", "bad_bearer")
	rows, _ = store.QueryAudit(policy.AuditFilter{Decision: "deny", Limit: 10})
	if len(rows) != 3 {
		t.Fatalf("after window want 3 rows, got %d", len(rows))
	}
}

func TestAuthFailureDefaultReason(t *testing.T) {
	a, store := newTestAudit(t)
	a.AuthFailure("mcp", "")
	rows, _ := store.QueryAudit(policy.AuditFilter{Decision: "deny", Limit: 10})
	if len(rows) != 1 || rows[0].Reason != "bad_bearer" {
		t.Fatalf("default reason: %+v", rows)
	}
}

func TestPrune(t *testing.T) {
	a, store := newTestAudit(t)
	now := int64(1_700_000_000_000)
	a.now = func() int64 { return now }
	old := now - 40*DayMs
	if err := store.InsertAudit(policy.AuditRow{Ts: old, Kind: "tool", Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	a.Tool("agent1", "x", "allow", "", nil) // fresh row at now
	if removed := a.Prune(30); removed != 1 {
		t.Fatalf("pruned %d, want 1", removed)
	}
	rows, _ := store.QueryAudit(policy.AuditFilter{Limit: 10})
	if len(rows) != 1 {
		t.Fatalf("after prune want 1 row, got %d", len(rows))
	}
}
