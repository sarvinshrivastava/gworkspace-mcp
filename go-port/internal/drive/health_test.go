package drive

import (
	"strings"
	"testing"
)

func newTestHealth() (*Health, *int64) {
	now := int64(1_700_000_000_000)
	h := NewHealth()
	h.now = func() int64 { return now }
	return h, &now
}

func TestRejectedTransitionAndNotice(t *testing.T) {
	h, _ := newTestHealth()
	var got []BrokenKind
	h.SetBroadcast(func(k BrokenKind, n Notice) { got = append(got, k) })
	h.Failure("rejected", "invalid_grant", "detail")
	if h.SnapshotOf().State != StateRejected {
		t.Fatalf("state = %s", h.SnapshotOf().State)
	}
	if len(got) != 1 || got[0] != BrokenRejected {
		t.Fatalf("broadcasts = %v", got)
	}
}

func TestTransientThresholdNeedsSpanAndCount(t *testing.T) {
	h, now := newTestHealth()
	var got []BrokenKind
	h.SetBroadcast(func(k BrokenKind, n Notice) { got = append(got, k) })
	for i := 0; i < 6; i++ {
		h.Failure("transient", "HTTP 500", "d")
	}
	if len(got) != 0 {
		t.Fatalf("6 fast failures inside the span must not alert: %v", got)
	}
	if h.SnapshotOf().State != StateFailing {
		t.Fatalf("state = %s", h.SnapshotOf().State)
	}
	*now += 61_000
	h.Failure("transient", "HTTP 500", "d")
	if len(got) != 1 || got[0] != BrokenFailing {
		t.Fatalf("after span want one failing broadcast: %v", got)
	}
}

func TestStaleOkNeverClearsRejection(t *testing.T) {
	h, now := newTestHealth()
	h.Failure("rejected", "invalid_grant", "d")
	rejectedAt := *now
	h.Ok(rejectedAt - 1000) // answer that started before the rejection
	if h.SnapshotOf().State != StateRejected {
		t.Fatalf("stale ok cleared the rejection")
	}
	h.Ok(rejectedAt + 1000)
	if h.SnapshotOf().State != StateOK {
		t.Fatalf("fresh ok must recover, state = %s", h.SnapshotOf().State)
	}
	if h.SnapshotOf().ConsecutiveFailures != 0 {
		t.Fatalf("failures not reset")
	}
}

func TestProbeRejectedFeedsFailure(t *testing.T) {
	h, _ := newTestHealth()
	var got []BrokenKind
	h.SetBroadcast(func(k BrokenKind, n Notice) { got = append(got, k) })
	h.Probe(ProbeResult{OK: false, Rejected: true, Detail: "invalid_grant"})
	if h.SnapshotOf().State != StateRejected || h.SnapshotOf().Probe != "failed" {
		t.Fatalf("snapshot = %+v", h.SnapshotOf())
	}
	if len(got) != 1 || got[0] != BrokenRejected {
		t.Fatalf("broadcasts = %v", got)
	}
}

func TestProbePlainFailureIsNotRejection(t *testing.T) {
	h, _ := newTestHealth()
	var got []BrokenKind
	h.SetBroadcast(func(k BrokenKind, n Notice) { got = append(got, k) })
	h.Probe(ProbeResult{OK: false, Rejected: false, Detail: "timeout"})
	if h.SnapshotOf().State != StateUnknown {
		t.Fatalf("plain probe failure must not move state: %s", h.SnapshotOf().State)
	}
	if len(got) != 1 || got[0] != BrokenProbe {
		t.Fatalf("broadcasts = %v", got)
	}
}

func TestRejectedNoticeMentionsRemediation(t *testing.T) {
	h, _ := newTestHealth()
	var n Notice
	h.SetBroadcast(func(k BrokenKind, notice Notice) { n = notice })
	h.Failure("rejected", "invalid_client", "d")
	if !strings.Contains(n.Message, "re-consents") || !strings.Contains(n.Title, "rejected") {
		t.Errorf("notice text drifted: %+v", n)
	}
}
