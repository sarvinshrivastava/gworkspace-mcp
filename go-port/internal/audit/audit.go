// The audit trail: one row per tool call, per refused credential and per
// dashboard change. Resource IDs only — never argument values or result text,
// which are document contents.
//
// Fail-open by design: a full disk must not turn into "every tool call fails".
// Port of src/audit.ts.
package audit

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/policy"
)

const DayMs = 24 * 60 * 60 * 1000
const authDedupeMs = 60_000

type Audit struct {
	Errors atomic.Int64

	mu              sync.Mutex
	store           *policy.PolicyStore
	now             func() int64
	lastAuthFailure map[string]int64
}

func New(store *policy.PolicyStore) *Audit {
	return &Audit{
		store:           store,
		now:             func() int64 { return time.Now().UnixMilli() },
		lastAuthFailure: map[string]int64{},
	}
}

func (a *Audit) write(row policy.AuditRow) {
	// Fail-open by design: a full disk must not turn into "every tool call
	// fails". A write that fails is logged and counted, and /healthz reports
	// the count.
	if err := a.store.InsertAudit(row); err != nil {
		n := a.Errors.Add(1)
		fmt.Printf("[audit] could not write a row (%d so far): %v\n", n, err)
	}
}

func (a *Audit) Tool(agentSlug, tool, decision, reason string, resources []string) {
	a.write(policy.AuditRow{
		Ts: a.now(), Kind: "tool", Agent: &agentSlug, Tool: &tool,
		Decision: decision, Reason: reason, Resources: resources,
	})
}

// AuthFailure: at most one row per source and reason per minute.
func (a *Audit) AuthFailure(source, reason string) {
	if reason == "" {
		reason = "bad_bearer"
	}
	key := source + "" + reason
	a.mu.Lock()
	at := a.now()
	if last, ok := a.lastAuthFailure[key]; ok && at-last < authDedupeMs {
		a.mu.Unlock()
		return
	}
	if len(a.lastAuthFailure) > 1000 {
		a.lastAuthFailure = map[string]int64{}
	}
	a.lastAuthFailure[key] = at
	a.mu.Unlock()
	a.write(policy.AuditRow{Ts: at, Kind: "auth", Decision: "deny", Reason: reason, Source: source})
}

// Admin files a dashboard change under the agent it touched.
func (a *Audit) Admin(action string, agent *string, resources []string, source string) {
	a.write(policy.AuditRow{
		Ts: a.now(), Kind: "admin", Agent: agent,
		Decision: "mutate", Reason: action, Resources: resources, Source: source,
	})
}

// AdminDeny records a refused dashboard request. Never deduplicated.
func (a *Audit) AdminDeny(reason, source string) {
	a.write(policy.AuditRow{Ts: a.now(), Kind: "auth", Decision: "deny", Reason: reason, Source: source})
}

// Prune removes rows older than retentionDays; 0 when the prune failed
// (counted in Errors).
func (a *Audit) Prune(retentionDays int) int {
	removed, err := a.store.PruneAudit(a.now() - int64(retentionDays)*DayMs)
	if err != nil {
		a.Errors.Add(1)
		fmt.Printf("[audit] prune failed: %v\n", err)
		return 0
	}
	if removed > 0 {
		fmt.Printf("[audit] pruned %d rows older than %d days\n", removed, retentionDays)
	}
	return int(removed)
}
