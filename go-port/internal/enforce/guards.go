// Argument guards that apply to EVERY agent, confined or not.
//
// The calendar tools are an outbound channel: an event can address arbitrary
// people, and a document containing injected instructions — one shared with
// this account by anyone — would otherwise turn a read tool and a calendar
// tool into mail-it-to-me.
// Port of src/enforce/guards.ts.
package enforce

import (
	"fmt"
	"regexp"
)

var IDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{10,128}$`)
var emailRE = regexp.MustCompile(`[^\s@<>()]+@[^\s@<>()]+\.[^\s@<>()]+`)

type GuardResult struct {
	Args    map[string]any
	Mutated bool
	Deny    string // "quickadd_guest" | "calendar_not_primary" when non-empty
}

// forcedNone mirrors TS value !== undefined && value !== "none". "undefined"
// is the ABSENT key; an explicit JSON null is present and not "none", so it
// counts as a mutation. Presence must come from the map, not the value.
func forcedNone(args map[string]any) bool {
	v, has := args["sendUpdates"]
	return has && v != "none"
}

// stripAttendees: createEvent and updateEvent. The event is still created; it
// just cannot address a stranger, and no invitation is sent.
func stripAttendees(args map[string]any) GuardResult {
	_, hasAttendees := args["attendees"]
	if hasAttendees {
		fmt.Println("[guard] dropped attendees from a calendar call")
	}
	out := map[string]any{}
	for k, v := range args {
		if k != "attendees" {
			out[k] = v
		}
	}
	out["sendUpdates"] = "none"
	return GuardResult{Args: out, Mutated: hasAttendees || forcedNone(args)}
}

// quickAddGuard: Google parses its free text, and "lunch with a@b.co" makes
// a@b.co a guest — the same channel as createEvent's attendees, through a
// field that cannot be stripped without changing the event.
func quickAddGuard(args map[string]any) GuardResult {
	if text, ok := args["text"].(string); ok && emailRE.MatchString(text) {
		return GuardResult{Deny: "quickadd_guest"}
	}
	out := map[string]any{}
	for k, v := range args {
		out[k] = v
	}
	out["sendUpdates"] = "none"
	return GuardResult{Args: out, Mutated: forcedNone(args)}
}

// primaryOnly: writes go to this account's own calendar only. Any other
// calendarId is a calendar someone may have shared with this account with
// edit rights — and an event written there, description and all, is theirs
// to read.
func primaryOnly(guard func(map[string]any) GuardResult) func(map[string]any) GuardResult {
	return func(args map[string]any) GuardResult {
		// TS: args.calendarId === undefined || === "primary". An explicit JSON
		// null is neither — it denies, exactly like any other non-primary id.
		cal, has := args["calendarId"]
		if !has || cal == "primary" {
			return guard(args)
		}
		return GuardResult{Deny: "calendar_not_primary"}
	}
}

var AllAgentGuards = map[string]func(map[string]any) GuardResult{
	"createEvent":   primaryOnly(stripAttendees),
	"updateEvent":   primaryOnly(stripAttendees),
	"quickAddEvent": primaryOnly(quickAddGuard),
}

// EventTimeOnlyKeys: updateEvent arguments that only move an event or address
// it. Anything else may carry text to the event's guests, so an allowlist
// fails closed.
var EventTimeOnlyKeys = map[string]bool{
	"calendarId":  true,
	"eventId":     true,
	"start":       true,
	"end":         true,
	"sendUpdates": true,
}
