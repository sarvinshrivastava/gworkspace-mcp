// What the gateway knows about Google, from its own Drive calls, the boot
// probe and (in the TS service) the Workspace child's exit. Feeds /healthz
// (a field, never the status code), the [drive]/[alert] log lines, and the
// push notices. In-memory: a restart resets it to unknown.
// Port of src/drive/health.ts.
package drive

import (
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/gwerrors"
)

type State string

const (
	StateUnknown  State = "unknown"
	StateOK       State = "ok"
	StateFailing  State = "failing"
	StateRejected State = "rejected"
)

type BrokenKind string

const (
	BrokenRejected     BrokenKind = "rejected"
	BrokenFailing      BrokenKind = "failing"
	BrokenProbe        BrokenKind = "probe"
	BrokenUpstreamDown BrokenKind = "upstream_down"
)

type Notice struct {
	Title   string
	Message string
}

type ProbeResult struct {
	OK       bool
	Rejected bool
	Detail   string
}

var safeCategoryRE = regexp.MustCompile(`^[A-Za-z0-9_ .-]{1,40}$`)

func safeCategory(category string) string {
	if safeCategoryRE.MatchString(category) {
		return category
	}
	return "unknown"
}

type Health struct {
	now         func() int64
	threshold   int
	minSpanMs   int64
	logWindowMs int64

	mu                    sync.Mutex
	broadcast             func(kind BrokenKind, notice Notice)
	state                 State
	since                 *int64
	failures              int
	firstFailureAt        int64
	rejectedAt            int64
	rejectedCategory      string
	lastTransientCategory string
	probeState            string
	probeAt               int64
	downAt                *int64
	alerted               map[BrokenKind]bool
	lastLogged            map[string]int64
}

func NewHealth() *Health {
	return &Health{
		now:         func() int64 { return time.Now().UnixMilli() },
		threshold:   5,
		minSpanMs:   60_000,
		logWindowMs: 60_000,
		state:       StateUnknown,
		rejectedAt:  -1 << 62,
		probeState:  "unknown",
		alerted:     map[BrokenKind]bool{},
		lastLogged:  map[string]int64{},
	}
}

func (h *Health) SetBroadcast(fn func(BrokenKind, Notice)) {
	h.mu.Lock()
	h.broadcast = fn
	h.mu.Unlock()
}

func (h *Health) broadcastOf() func(BrokenKind, Notice) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.broadcast
}

func iso(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano) }

// Ok records a Drive answer that started at startedAt. A stale one never
// clears a rejection.
func (h *Health) Ok(startedAt int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state == StateRejected && startedAt <= h.rejectedAt {
		return
	}
	h.failures = 0
	if h.state == StateOK {
		return
	}
	h.state = StateOK
	now := h.now()
	h.since = &now
	if h.alerted[BrokenFailing] || h.alerted[BrokenRejected] {
		fmt.Println("[recovered] Google Drive answers again")
	}
	h.alerted[BrokenFailing] = false
	h.alerted[BrokenRejected] = false
}

// Failure records one transient or rejected failure with its safe category.
func (h *Health) Failure(kind string, category, detail string) {
	h.mu.Lock()
	now := h.now()
	h.failures++
	var sendKind BrokenKind
	var notice *Notice
	if kind == "rejected" {
		if h.state != StateRejected {
			h.state = StateRejected
			h.since = &now
		}
		h.rejectedAt = now
		h.rejectedCategory = category
		if !h.alerted[BrokenRejected] {
			h.alerted[BrokenRejected] = true
			fmt.Printf("[alert] Google access rejected (%s): %s\n", safeCategory(category), gwerrors.RedactDetail(detail, 300))
		}
		n := h.rejectedNoticeLocked()
		sendKind, notice = BrokenRejected, &n
	} else {
		h.lastTransientCategory = category
		if h.state != StateRejected {
			if h.state != StateFailing {
				h.state = StateFailing
				h.since = &now
				h.failures = 1
				h.firstFailureAt = now
			}
			if h.failures >= h.threshold && now-h.firstFailureAt >= h.minSpanMs {
				if !h.alerted[BrokenFailing] {
					h.alerted[BrokenFailing] = true
					fmt.Printf("[alert] Google Drive failing: %d consecutive failures (%s): %s\n",
						h.failures, safeCategory(category), gwerrors.RedactDetail(detail, 300))
				}
				n := h.failingNoticeLocked()
				sendKind, notice = BrokenFailing, &n
			}
		}
	}
	h.mu.Unlock()
	if notice != nil {
		h.send(sendKind, *notice)
	}
}

// Probe records the boot credential check. A probe naming invalid_grant is a
// rejection, not a probe notice.
func (h *Health) Probe(result ProbeResult) {
	h.mu.Lock()
	h.probeAt = h.now()
	if result.OK {
		h.probeState = "ok"
		h.mu.Unlock()
		return
	}
	h.probeState = "failed"
	if result.Rejected {
		detail := result.Detail
		h.mu.Unlock()
		h.Failure("rejected", "invalid_grant", detail)
		return
	}
	if !h.alerted[BrokenProbe] {
		h.alerted[BrokenProbe] = true
		fmt.Printf("[alert] boot probe of the Google credential failed: %s\n", gwerrors.RedactDetail(result.Detail, 300))
	}
	notice := Notice{
		Title:   "gworkspace-mcp: Google credential check failed at boot",
		Message: fmt.Sprintf("The boot credential check failed. Workspace tools may fail. Since %s.", iso(h.probeAt)),
	}
	h.mu.Unlock()
	h.send(BrokenProbe, notice)
}

// UpstreamDown: in the TS service, the Workspace child exited. In the Go port
// there is no child; retained for the health contract
// decision — a stuck Google call breaker could reuse it. Not wired yet.
func (h *Health) UpstreamDown() {
	h.mu.Lock()
	now := h.now()
	if h.downAt == nil {
		h.downAt = &now
	}
	if !h.alerted[BrokenUpstreamDown] {
		h.alerted[BrokenUpstreamDown] = true
		fmt.Println("[alert] the Workspace server process is gone; tool calls fail until the service restarts")
	}
	notice := Notice{
		Title:   "gworkspace-mcp: Workspace server exited",
		Message: fmt.Sprintf("The Google Workspace server process exited. Tool calls fail until the service restarts. Since %s.", iso(*h.downAt)),
	}
	h.mu.Unlock()
	h.send(BrokenUpstreamDown, notice)
}

// LogDrive: one line per reason and category per window; the denial itself is
// never suppressed.
func (h *Health) LogDrive(tool, reason, category string, detail *string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	key := reason + " " + category
	if last, ok := h.lastLogged[key]; ok && now-last < h.logWindowMs {
		return
	}
	if len(h.lastLogged) > 100 {
		h.lastLogged = map[string]int64{}
	}
	h.lastLogged[key] = now
	d := "-"
	if detail != nil {
		d = gwerrors.RedactDetail(*detail, 300)
	}
	fmt.Printf("[drive] %s %s: %s\n", tool, reason, d)
}

type Snapshot struct {
	State               State   `json:"state"`
	Since               *string `json:"since"`
	ConsecutiveFailures int     `json:"consecutiveFailures"`
	Probe               string  `json:"probe"`
}

func (h *Health) SnapshotOf() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	var since *string
	if h.since != nil {
		s := iso(*h.since)
		since = &s
	}
	return Snapshot{
		State:               h.state,
		Since:               since,
		ConsecutiveFailures: h.failures,
		Probe:               h.probeState,
	}
}

func (h *Health) rejectedNoticeLocked() Notice {
	since := h.now()
	if h.since != nil {
		since = *h.since
	}
	return Notice{
		Title: "gworkspace-mcp: Google access rejected",
		Message: fmt.Sprintf(
			"Google access rejected (%s). Workspace tools will fail until the owner re-consents and replaces GOOGLE_TOKEN_JSON, or enables the API. Since %s.",
			safeCategory(h.rejectedCategory), iso(since)),
	}
}

func (h *Health) failingNoticeLocked() Notice {
	since := h.now()
	if h.since != nil {
		since = *h.since
	}
	return Notice{
		Title: "gworkspace-mcp: Google Drive failing",
		Message: fmt.Sprintf(
			"%d consecutive Drive failures since %s. Last: %s. Usually transient.",
			h.failures, iso(since), safeCategory(h.lastTransientCategory)),
	}
}

// send: a broken sender must never reach a tool call.
func (h *Health) send(kind BrokenKind, notice Notice) {
	if b := h.broadcastOf(); b != nil {
		func() {
			defer func() { _ = recover() }()
			b(kind, notice)
		}()
	}
}
