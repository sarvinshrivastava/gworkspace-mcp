package enforce

import (
	"encoding/json"
	"strings"
	"testing"
)

// Regression tests for the TS null-vs-undefined guard semantics: JSON null is
// a PRESENT value; only an absent key is undefined.

func args(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPrimaryOnly_NullCalendarIdDenies(t *testing.T) {
	// TS: args.calendarId === undefined || === "primary" — null is neither.
	for _, tool := range []string{"createEvent", "updateEvent", "quickAddEvent"} {
		res := AllAgentGuards[tool](args(t, `{"calendarId":null,"text":"standup"}`))
		if res.Deny != "calendar_not_primary" {
			t.Fatalf("%s calendarId:null deny = %q, want calendar_not_primary", tool, res.Deny)
		}
	}
}

func TestPrimaryOnly_AbsentAndPrimaryPass(t *testing.T) {
	res := AllAgentGuards["createEvent"](args(t, `{"summary":"x"}`))
	if res.Deny != "" {
		t.Fatalf("absent calendarId denied: %q", res.Deny)
	}
	res = AllAgentGuards["createEvent"](args(t, `{"calendarId":"primary","summary":"x"}`))
	if res.Deny != "" {
		t.Fatalf("primary calendarId denied: %q", res.Deny)
	}
}

func TestStripAttendees_NullSendUpdatesCountsAsMutation(t *testing.T) {
	// TS forcedNone: null !== undefined && null !== "none" → mutated.
	res := AllAgentGuards["createEvent"](args(t, `{"summary":"x","sendUpdates":null}`))
	if !res.Mutated {
		t.Fatal("sendUpdates:null must count as a mutation (TS presence semantics)")
	}
	if res.Args["sendUpdates"] != "none" {
		t.Fatalf("sendUpdates forced to none, got %v", res.Args["sendUpdates"])
	}
	// Absent sendUpdates with no attendees: no mutation.
	res = AllAgentGuards["createEvent"](args(t, `{"summary":"x"}`))
	if res.Mutated {
		t.Fatal("absent sendUpdates must not count as a mutation")
	}
	// Explicit "none": not a mutation.
	res = AllAgentGuards["createEvent"](args(t, `{"summary":"x","sendUpdates":"none"}`))
	if res.Mutated {
		t.Fatal(`sendUpdates:"none" must not count as a mutation`)
	}
}

func TestHasNonTimeKeys_NullCounts(t *testing.T) {
	// TS Object.keys includes null-valued keys and null !== undefined.
	if !hasNonTimeKeys(args(t, `{"eventId":"e","summary":null}`)) {
		t.Fatal("summary:null must count as a non-time key")
	}
	if hasNonTimeKeys(args(t, `{"eventId":"e","sendUpdates":null}`)) {
		t.Fatal("sendUpdates:null is a time-only key, must not count")
	}
	if hasNonTimeKeys(args(t, `{"eventId":"e"}`)) {
		t.Fatal("only time-only keys present")
	}
}

func TestSearchEnvelope_KeyOrderAndEscaping(t *testing.T) {
	// The wire text must be files,total,hasMore in that order with no HTML
	// escaping (JSON.stringify parity), regardless of Go's map ordering and
	// default encoder.
	var buf strings.Builder
	_ = buf
	out := searchEnvelope{
		Files: []searchFileOut{{
			Name:  "a<b>&c",
			Size:  nil,
			Owner: nil,
		}},
		Total:   1,
		HasMore: false,
	}
	text := stringifySearch(out)
	idx := strings.Index(text, `"files"`)
	if idx < 0 || !strings.Contains(text[idx:], `"total"`) || strings.Index(text, `"total"`) > strings.Index(text, `"hasMore"`) {
		t.Fatalf("key order wrong: %s", text)
	}
	if !strings.Contains(text, `"name": "a<b>&c"`) {
		t.Fatalf("HTML escaping leaked into output: %s", text)
	}
	if strings.Contains(text, `"id"`) {
		t.Fatalf("undefined id should be dropped, not emitted: %s", text)
	}
	if !strings.Contains(text, `"size": null`) || !strings.Contains(text, `"owner": null`) {
		t.Fatalf("null-coerced fields must always emit: %s", text)
	}
	if strings.HasSuffix(text, "\n") {
		t.Fatal("trailing newline: JSON.stringify adds none")
	}
}

func TestJsStr(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"abc", "abc"},
		{float64(12), "12"},
		{float64(1.5), "1.5"},
		{true, "true"},
	}
	for _, tc := range cases {
		if got := jsStr(tc.in); got != tc.want {
			t.Fatalf("jsStr(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
