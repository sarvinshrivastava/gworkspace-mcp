package tools

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// Calendar tools tests — same shape as the listDriveFiles tests.

func withCalendarBase(t *testing.T, base string) {
	t.Helper()
	old := CalendarBase
	CalendarBase = base
	t.Cleanup(func() { CalendarBase = old })
}

var isoMillisRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

func TestListEvents_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"nextPageToken":"tok9","items":[{"id":"ev1","status":"confirmed","summary":"Standup","description":"Daily","location":"Room 1","start":{"dateTime":"2026-04-10T09:00:00+05:30","timeZone":"Asia/Kolkata"},"end":{"dateTime":"2026-04-10T09:15:00+05:30","timeZone":"Asia/Kolkata"},"attendees":[{"email":"a@b.co","responseStatus":"accepted"},{"email":"c@d.co","responseStatus":"declined","optional":true}],"organizer":{"email":"me@x.co","self":true},"htmlLink":"https://cal.example/ev1","recurringEventId":"rec1"},{"id":"ev2","status":"cancelled","start":{"date":"2026-04-11"},"end":{"date":"2026-04-12"}}]}`)
	})
	withCalendarBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := ListEvents(context.Background(), g, mustArgs(t, `{}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if req.Method != http.MethodGet {
		t.Fatalf("method = %q", req.Method)
	}
	if req.Path != "/calendars/primary/events" { // zod default calendarId
		t.Fatalf("path = %q", req.Path)
	}
	q, _ := url.ParseQuery(req.Query)
	if _, ok := q["q"]; ok { // googleapis omits undefined
		t.Fatalf("q should be omitted, query = %q", req.Query)
	}
	if got := q.Get("timeMin"); !isoMillisRe.MatchString(got) {
		t.Fatalf("timeMin = %q (want new Date().toISOString() shape)", got)
	}
	if _, ok := q["timeMax"]; ok {
		t.Fatalf("timeMax should be omitted, query = %q", req.Query)
	}
	if got := q.Get("maxResults"); got != "25" { // zod default
		t.Fatalf("maxResults = %q", got)
	}
	if got := q.Get("singleEvents"); got != "true" { // zod default
		t.Fatalf("singleEvents = %q", got)
	}
	if got := q.Get("orderBy"); got != "startTime" { // singleEvents → startTime
		t.Fatalf("orderBy = %q", got)
	}

	want := `{
  "events": [
    {
      "id": "ev1",
      "status": "confirmed",
      "summary": "Standup",
      "description": "Daily",
      "location": "Room 1",
      "start": {
        "dateTime": "2026-04-10T09:00:00+05:30",
        "timeZone": "Asia/Kolkata"
      },
      "end": {
        "dateTime": "2026-04-10T09:15:00+05:30",
        "timeZone": "Asia/Kolkata"
      },
      "attendees": [
        {
          "email": "a@b.co",
          "responseStatus": "accepted",
          "optional": false
        },
        {
          "email": "c@d.co",
          "responseStatus": "declined",
          "optional": true
        }
      ],
      "organizer": "me@x.co",
      "htmlLink": "https://cal.example/ev1",
      "recurringEventId": "rec1"
    },
    {
      "id": "ev2",
      "status": "cancelled",
      "summary": null,
      "description": null,
      "location": null,
      "start": {
        "date": "2026-04-11"
      },
      "end": {
        "date": "2026-04-12"
      },
      "attendees": [],
      "organizer": null,
      "htmlLink": null,
      "recurringEventId": null
    }
  ],
  "count": 2,
  "nextPageToken": "tok9"
}`
	if out != want {
		t.Fatalf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestListEvents_Params(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"items":[]}`)
	})
	withCalendarBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := ListEvents(context.Background(), g, mustArgs(t,
		`{"calendarId":"team@group.calendar.google.com","q":"","timeMin":"2026-04-01T00:00:00Z","timeMax":"2026-05-01T00:00:00Z","maxResults":5,"singleEvents":false}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if req.Path != "/calendars/team@group.calendar.google.com/events" { // recorded path is URL-decoded
		t.Fatalf("path = %q", req.Path)
	}
	q, _ := url.ParseQuery(req.Query)
	if _, ok := q["q"]; !ok { // "" is defined: googleapis keeps it
		t.Fatalf("q should be present (empty), query = %q", req.Query)
	}
	if got := q.Get("timeMin"); got != "2026-04-01T00:00:00Z" { // passthrough, not re-parsed
		t.Fatalf("timeMin = %q", got)
	}
	if got := q.Get("timeMax"); got != "2026-05-01T00:00:00Z" {
		t.Fatalf("timeMax = %q", got)
	}
	if got := q.Get("maxResults"); got != "5" {
		t.Fatalf("maxResults = %q", got)
	}
	if got := q.Get("singleEvents"); got != "false" {
		t.Fatalf("singleEvents = %q", got)
	}
	if _, ok := q["orderBy"]; ok { // undefined when singleEvents is false
		t.Fatalf("orderBy should be omitted, query = %q", req.Query)
	}
	want := "{\n  \"events\": [],\n  \"count\": 0,\n  \"nextPageToken\": null\n}"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestListEvents_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"items":[]}`)
	})
	withCalendarBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	ctx := context.Background()

	cases := []struct {
		name, args, wantMsg string
	}{
		{"unknown key", `{"bogus":1}`, `Unrecognized key: "bogus"`},
		{"null calendarId", `{"calendarId":null}`, `Expected string, received null: "calendarId"`},
		{"null timeMin", `{"timeMin":null}`, `Expected string, received null: "timeMin"`},
		{"maxResults zero", `{"maxResults":0}`, `Number must be greater than or equal to 1: "maxResults"`},
		{"maxResults over", `{"maxResults":2501}`, `Number must be less than or equal to 2500: "maxResults"`},
		{"maxResults fractional", `{"maxResults":2.5}`, `Expected integer for "maxResults"`},
		{"maxResults null", `{"maxResults":null}`, `Expected number, received null: "maxResults"`},
		{"singleEvents wrong type", `{"singleEvents":"yes"}`, `Expected boolean for "singleEvents"`},
		{"singleEvents null", `{"singleEvents":null}`, `Expected boolean, received null: "singleEvents"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListEvents(ctx, g, mustArgs(t, tc.args))
			ue, ok := err.(*UserError)
			if !ok {
				t.Fatalf("err = %v (not a UserError)", err)
			}
			if ue.Msg != tc.wantMsg {
				t.Fatalf("msg = %q, want %q", ue.Msg, tc.wantMsg)
			}
		})
	}
}

func TestListEvents_HTTPBranches(t *testing.T) {
	t.Run("404 calendar", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 404, `{"error":{"message":"Not Found"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := ListEvents(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"calendarId":"gone@group.calendar.google.com"}`))
		ue := err.(*UserError)
		if ue.Msg != "Calendar not found (ID: gone@group.calendar.google.com)." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("403 scope", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"Insufficient Permission"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := ListEvents(context.Background(), fg.googleFor(newFakeTokenSource()), nil)
		ue := err.(*UserError)
		if ue.Msg != "Permission denied. Confirm the calendar.events scope was granted during consent." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("500 passthrough", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 500, `{"error":{"message":"Backend exploded"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := ListEvents(context.Background(), fg.googleFor(newFakeTokenSource()), nil)
		ue := err.(*UserError)
		if ue.Msg != "Failed to list events: Backend exploded" {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("401 refresh once", func(t *testing.T) {
		tok := newFakeTokenSource()
		calls := 0
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				cannedJSON(w, 401, `{"error":{"message":"Unauthorized"}}`)
				return
			}
			cannedJSON(w, 200, `{"items":[]}`)
		})
		withCalendarBase(t, fg.server.URL)
		out, err := ListEvents(context.Background(), fg.googleFor(tok), nil)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if calls != 2 || tok.invalidated != 1 {
			t.Fatalf("calls=%d invalidated=%d", calls, tok.invalidated)
		}
		if !strings.Contains(out, `"count": 0`) {
			t.Fatalf("out = %s", out)
		}
	})
}

func TestCreateEvent_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"ev9","summary":"Lunch","start":{"dateTime":"2026-04-15T12:00:00+05:30"},"end":{"dateTime":"2026-04-15T13:00:00+05:30","timeZone":"Asia/Kolkata"},"htmlLink":"https://cal.example/ev9","attendees":[{"email":"sam@x.co"},{"email":"j@x.co","optional":true}]}`)
	})
	withCalendarBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := CreateEvent(context.Background(), g, mustArgs(t,
		`{"summary":"Lunch","description":"With Sam","location":"Cafe","start":{"dateTime":"2026-04-15T12:00:00+05:30"},"end":{"dateTime":"2026-04-15T13:00:00+05:30","timeZone":"Asia/Kolkata"},"attendees":[{"email":"sam@x.co"},{"email":"j@x.co","optional":true}],"sendUpdates":"all"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if req.Method != http.MethodPost || req.Path != "/calendars/primary/events" {
		t.Fatalf("%s %s", req.Method, req.Path)
	}
	q, _ := url.ParseQuery(req.Query)
	if got := q.Get("sendUpdates"); got != "all" {
		t.Fatalf("sendUpdates = %q", got)
	}
	if _, ok := q["conferenceDataVersion"]; ok { // undefined when conferenceData false
		t.Fatalf("conferenceDataVersion should be omitted, query = %q", req.Query)
	}
	wantBody := `{"summary":"Lunch","description":"With Sam","location":"Cafe","start":{"dateTime":"2026-04-15T12:00:00+05:30"},"end":{"dateTime":"2026-04-15T13:00:00+05:30","timeZone":"Asia/Kolkata"},"attendees":[{"email":"sam@x.co"},{"email":"j@x.co","optional":true}]}`
	if req.Body != wantBody {
		t.Fatalf("body = %s\nwant %s", req.Body, wantBody)
	}

	want := `{
  "success": true,
  "id": "ev9",
  "summary": "Lunch",
  "start": {
    "dateTime": "2026-04-15T12:00:00+05:30"
  },
  "end": {
    "dateTime": "2026-04-15T13:00:00+05:30",
    "timeZone": "Asia/Kolkata"
  },
  "htmlLink": "https://cal.example/ev9",
  "hangoutLink": null,
  "attendees": 2,
  "message": "Event \"Lunch\" created."
}`
	if out != want {
		t.Fatalf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestCreateEvent_ConferenceData(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"evM","summary":"Sync","hangoutLink":"https://meet.example/xyz"}`)
	})
	withCalendarBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := CreateEvent(context.Background(), g, mustArgs(t,
		`{"summary":"Sync","start":{"dateTime":"2026-04-15T12:00:00Z"},"end":{"dateTime":"2026-04-15T12:30:00Z"},"conferenceData":true}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	q, _ := url.ParseQuery(req.Query)
	if got := q.Get("conferenceDataVersion"); got != "1" {
		t.Fatalf("conferenceDataVersion = %q", got)
	}
	if got := q.Get("sendUpdates"); got != "none" { // zod default
		t.Fatalf("sendUpdates = %q", got)
	}
	uuidRe := regexp.MustCompile(`"requestId":"mcp-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}"`)
	if !uuidRe.MatchString(req.Body) {
		t.Fatalf("requestId missing/not v4: %s", req.Body)
	}
	if !strings.Contains(req.Body, `"conferenceSolutionKey":{"type":"hangoutsMeet"}`) {
		t.Fatalf("conferenceSolutionKey missing: %s", req.Body)
	}
	if !strings.Contains(out, `"hangoutLink": "https://meet.example/xyz"`) {
		t.Fatalf("out = %s", out)
	}
}

func TestCreateEvent_AllDayAndStripping(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"evA","summary":"Offsite"}`)
	})
	withCalendarBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	// All-day event (date, not dateTime); unknown nested keys are stripped
	// like z.object (non-strict) does, not rejected.
	_, err := CreateEvent(context.Background(), g, mustArgs(t,
		`{"summary":"Offsite","start":{"date":"2026-04-16","surprise":1},"end":{"date":"2026-04-17"}}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	wantBody := `{"summary":"Offsite","start":{"date":"2026-04-16"},"end":{"date":"2026-04-17"}}`
	if got := fg.lastRequest().Body; got != wantBody {
		t.Fatalf("body = %s, want %s", got, wantBody)
	}
}

func TestCreateEvent_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"x"}`)
	})
	withCalendarBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	ctx := context.Background()
	dt := `"start":{"dateTime":"2026-04-15T12:00:00Z"},"end":{"dateTime":"2026-04-15T13:00:00Z"}`

	cases := []struct {
		name, args, wantMsg string
	}{
		{"missing summary", `{` + dt + `}`, `Required: "summary"`},
		{"null summary", `{"summary":null,` + dt + `}`, `Expected string, received null: "summary"`},
		{"missing start", `{"summary":"s","end":{"date":"2026-04-17"}}`, `Required: "start"`},
		{"missing end", `{"summary":"s","start":{"date":"2026-04-16"}}`, `Required: "end"`},
		{"null start", `{"summary":"s","start":null,"end":{"date":"2026-04-17"}}`, `Expected object, received null: "start"`},
		{"refine empty start", `{"summary":"s","start":{},"end":{"date":"2026-04-17"}}`, dcDateTimeRefineMsg},
		{"refine empty-string dateTime", `{"summary":"s","start":{"dateTime":""},"end":{"date":"2026-04-17"}}`, dcDateTimeRefineMsg},
		{"null nested dateTime", `{"summary":"s","start":{"dateTime":null},"end":{"date":"2026-04-17"}}`, `Expected string for "dateTime"`},
		{"null description", `{"summary":"s","description":null,` + dt + `}`, `Expected string, received null: "description"`},
		{"bad sendUpdates", `{"summary":"s",` + dt + `,"sendUpdates":"maybe"}`, `Invalid enum value for "sendUpdates": "maybe"`},
		{"null sendUpdates", `{"summary":"s",` + dt + `,"sendUpdates":null}`, `Expected string, received null: "sendUpdates"`},
		{"null attendees", `{"summary":"s",` + dt + `,"attendees":null}`, `Expected array, received null: "attendees"`},
		{"attendee unknown key", `{"summary":"s",` + dt + `,"attendees":[{"email":"a@b.co","role":"boss"}]}`, `Unrecognized key: "role"`},
		{"attendee missing email", `{"summary":"s",` + dt + `,"attendees":[{"optional":true}]}`, `Required: "email"`},
		{"attendee null optional", `{"summary":"s",` + dt + `,"attendees":[{"email":"a@b.co","optional":null}]}`, `Expected boolean for "optional"`},
		{"unknown top key", `{"summary":"s",` + dt + `,"bogus":1}`, `Unrecognized key: "bogus"`},
		{"null conferenceData", `{"summary":"s",` + dt + `,"conferenceData":null}`, `Expected boolean, received null: "conferenceData"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateEvent(ctx, g, mustArgs(t, tc.args))
			ue, ok := err.(*UserError)
			if !ok {
				t.Fatalf("err = %v (not a UserError)", err)
			}
			if ue.Msg != tc.wantMsg {
				t.Fatalf("msg = %q, want %q", ue.Msg, tc.wantMsg)
			}
		})
	}
}

func TestCreateEvent_HTTPBranches(t *testing.T) {
	args := `{"summary":"s","start":{"dateTime":"2026-04-15T12:00:00Z"},"end":{"dateTime":"2026-04-15T13:00:00Z"}}`
	t.Run("400 with message", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 400, `{"error":{"message":"Invalid datetime"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := CreateEvent(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, args))
		ue := err.(*UserError)
		want := "Calendar rejected the event: Invalid datetime. Check that start/end formats are valid RFC3339."
		if ue.Msg != want {
			t.Fatalf("msg = %q, want %q", ue.Msg, want)
		}
	})
	t.Run("400 without message falls back to Bad request", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 400, `{}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := CreateEvent(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, args))
		ue := err.(*UserError)
		want := "Calendar rejected the event: Bad request. Check that start/end formats are valid RFC3339."
		if ue.Msg != want {
			t.Fatalf("msg = %q, want %q", ue.Msg, want)
		}
	})
	t.Run("403 scope", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"Insufficient Permission"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := CreateEvent(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, args))
		ue := err.(*UserError)
		if ue.Msg != "Permission denied. Confirm the calendar.events scope was granted." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("404 calendar", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 404, `{"error":{"message":"Not Found"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := CreateEvent(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, args))
		ue := err.(*UserError)
		if ue.Msg != "Calendar not found (ID: primary)." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("500 passthrough", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 500, `{"error":{"message":"Backend exploded"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := CreateEvent(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, args))
		ue := err.(*UserError)
		if ue.Msg != "Failed to create event: Backend exploded" {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("401 refresh once", func(t *testing.T) {
		tok := newFakeTokenSource()
		calls := 0
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				cannedJSON(w, 401, `{"error":{"message":"Unauthorized"}}`)
				return
			}
			cannedJSON(w, 200, `{"id":"evR","summary":"s"}`)
		})
		withCalendarBase(t, fg.server.URL)
		out, err := CreateEvent(context.Background(), fg.googleFor(tok), mustArgs(t, args))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if calls != 2 || tok.invalidated != 1 {
			t.Fatalf("calls=%d invalidated=%d", calls, tok.invalidated)
		}
		if !strings.Contains(out, `"id": "evR"`) {
			t.Fatalf("out = %s", out)
		}
	})
}

func TestUpdateEvent_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"ev9","summary":"New Title","start":{"dateTime":"2026-04-15T14:00:00+05:30"},"end":{"dateTime":"2026-04-15T15:00:00+05:30"},"htmlLink":"https://cal.example/ev9","updated":"2026-04-01T10:00:00.000Z"}`)
	})
	withCalendarBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := UpdateEvent(context.Background(), g, mustArgs(t,
		`{"eventId":"ev9","summary":"New Title","start":{"dateTime":"2026-04-15T14:00:00+05:30"}}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if req.Method != http.MethodPatch || req.Path != "/calendars/primary/events/ev9" {
		t.Fatalf("%s %s", req.Method, req.Path)
	}
	q, _ := url.ParseQuery(req.Query)
	if got := q.Get("sendUpdates"); got != "none" { // zod default
		t.Fatalf("sendUpdates = %q", got)
	}
	// PATCH semantics: only the provided fields, in upstream assignment order.
	wantBody := `{"summary":"New Title","start":{"dateTime":"2026-04-15T14:00:00+05:30"}}`
	if req.Body != wantBody {
		t.Fatalf("body = %s, want %s", req.Body, wantBody)
	}

	want := `{
  "success": true,
  "id": "ev9",
  "summary": "New Title",
  "start": {
    "dateTime": "2026-04-15T14:00:00+05:30"
  },
  "end": {
    "dateTime": "2026-04-15T15:00:00+05:30"
  },
  "htmlLink": "https://cal.example/ev9",
  "updated": "2026-04-01T10:00:00.000Z",
  "message": "Event ev9 updated."
}`
	if out != want {
		t.Fatalf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestUpdateEvent_AttendeesReplace(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"ev9"}`)
	})
	withCalendarBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	// Empty array is a real value: full replace with nobody.
	_, err := UpdateEvent(context.Background(), g, mustArgs(t, `{"eventId":"ev9","attendees":[]}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if got := fg.lastRequest().Body; got != `{"attendees":[]}` {
		t.Fatalf("body = %s", got)
	}

	_, err = UpdateEvent(context.Background(), g, mustArgs(t, `{"eventId":"ev9","attendees":[{"email":"a@b.co","optional":false}],"sendUpdates":"externalOnly"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if got := req.Body; got != `{"attendees":[{"email":"a@b.co","optional":false}]}` {
		t.Fatalf("body = %s", got)
	}
	q, _ := url.ParseQuery(req.Query)
	if got := q.Get("sendUpdates"); got != "externalOnly" {
		t.Fatalf("sendUpdates = %q", got)
	}
}

func TestUpdateEvent_NoFields(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"ev9"}`)
	})
	withCalendarBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	want := "No fields provided to update. Pass at least one of summary, description, location, start, end, or attendees."

	// calendarId, eventId and sendUpdates are not body fields.
	for _, args := range []string{`{"eventId":"ev9"}`, `{"eventId":"ev9","calendarId":"primary","sendUpdates":"all"}`} {
		_, err := UpdateEvent(context.Background(), g, mustArgs(t, args))
		ue, ok := err.(*UserError)
		if !ok {
			t.Fatalf("err = %v (not a UserError)", err)
		}
		if ue.Msg != want {
			t.Fatalf("msg = %q, want %q", ue.Msg, want)
		}
	}
	if len(fg.requests) != 0 {
		t.Fatalf("no-fields update must not call Google; got %d requests", len(fg.requests))
	}
}

func TestUpdateEvent_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"ev9"}`)
	})
	withCalendarBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	ctx := context.Background()

	cases := []struct {
		name, args, wantMsg string
	}{
		{"missing eventId", `{"summary":"x"}`, `Required: "eventId"`},
		{"null eventId", `{"eventId":null,"summary":"x"}`, `Expected string, received null: "eventId"`},
		{"null summary", `{"eventId":"e","summary":null}`, `Expected string, received null: "summary"`},
		{"start refine", `{"eventId":"e","start":{}}`, dcDateTimeRefineMsg},
		{"unknown key", `{"eventId":"e","bogus":1}`, `Unrecognized key: "bogus"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UpdateEvent(ctx, g, mustArgs(t, tc.args))
			ue, ok := err.(*UserError)
			if !ok {
				t.Fatalf("err = %v (not a UserError)", err)
			}
			if ue.Msg != tc.wantMsg {
				t.Fatalf("msg = %q, want %q", ue.Msg, tc.wantMsg)
			}
		})
	}
}

func TestUpdateEvent_HTTPBranches(t *testing.T) {
	args := `{"eventId":"ev9","summary":"x"}`
	t.Run("404 event", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 404, `{"error":{"message":"Not Found"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := UpdateEvent(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, args))
		ue := err.(*UserError)
		if ue.Msg != "Event not found: ev9 on calendar primary." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("403 scope", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"Insufficient Permission"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := UpdateEvent(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, args))
		ue := err.(*UserError)
		if ue.Msg != "Permission denied. Confirm the calendar.events scope was granted." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("400 fallback Bad request", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 400, `{}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := UpdateEvent(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, args))
		ue := err.(*UserError)
		if ue.Msg != "Calendar rejected the update: Bad request." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("500 passthrough", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 500, `{"error":{"message":"Backend exploded"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := UpdateEvent(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, args))
		ue := err.(*UserError)
		if ue.Msg != "Failed to update event: Backend exploded" {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("401 refresh once", func(t *testing.T) {
		tok := newFakeTokenSource()
		calls := 0
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				cannedJSON(w, 401, `{"error":{"message":"Unauthorized"}}`)
				return
			}
			cannedJSON(w, 200, `{"id":"ev9","summary":"x"}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := UpdateEvent(context.Background(), fg.googleFor(tok), mustArgs(t, args))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if calls != 2 || tok.invalidated != 1 {
			t.Fatalf("calls=%d invalidated=%d", calls, tok.invalidated)
		}
	})
}

func TestQuickAddEvent_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"evQ","summary":"Lunch with Sarah","start":{"dateTime":"2026-04-16T12:00:00+05:30"},"end":{"dateTime":"2026-04-16T13:00:00+05:30"},"htmlLink":"https://cal.example/evQ"}`)
	})
	withCalendarBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := QuickAddEvent(context.Background(), g, mustArgs(t, `{"text":"Lunch with Sarah tomorrow at 12pm"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if req.Method != http.MethodPost || req.Path != "/calendars/primary/events/quickAdd" {
		t.Fatalf("%s %s", req.Method, req.Path)
	}
	if req.Body != "" {
		t.Fatalf("quickAdd sends no body, got %q", req.Body)
	}
	q, _ := url.ParseQuery(req.Query)
	if got := q.Get("text"); got != "Lunch with Sarah tomorrow at 12pm" {
		t.Fatalf("text = %q", got)
	}
	if got := q.Get("sendUpdates"); got != "none" {
		t.Fatalf("sendUpdates = %q", got)
	}

	want := `{
  "success": true,
  "id": "evQ",
  "summary": "Lunch with Sarah",
  "start": {
    "dateTime": "2026-04-16T12:00:00+05:30"
  },
  "end": {
    "dateTime": "2026-04-16T13:00:00+05:30"
  },
  "htmlLink": "https://cal.example/evQ",
  "message": "Event \"Lunch with Sarah\" created from \"Lunch with Sarah tomorrow at 12pm\"."
}`
	if out != want {
		t.Fatalf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestQuickAddEvent_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"x"}`)
	})
	withCalendarBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	ctx := context.Background()

	cases := []struct {
		name, args, wantMsg string
	}{
		{"missing text", `{}`, `Required: "text"`},
		{"null text", `{"text":null}`, `Expected string, received null: "text"`},
		{"bad sendUpdates", `{"text":"t","sendUpdates":"maybe"}`, `Invalid enum value for "sendUpdates": "maybe"`},
		{"unknown key", `{"text":"t","bogus":1}`, `Unrecognized key: "bogus"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := QuickAddEvent(ctx, g, mustArgs(t, tc.args))
			ue, ok := err.(*UserError)
			if !ok {
				t.Fatalf("err = %v (not a UserError)", err)
			}
			if ue.Msg != tc.wantMsg {
				t.Fatalf("msg = %q, want %q", ue.Msg, tc.wantMsg)
			}
		})
	}
}

func TestQuickAddEvent_HTTPBranches(t *testing.T) {
	t.Run("400 parse failure", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 400, `{"error":{"message":"Invalid Value"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := QuickAddEvent(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"text":"garbage"}`))
		ue := err.(*UserError)
		if ue.Msg != `Calendar could not parse "garbage" as an event. Try a clearer time format.` {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("404 calendar", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 404, `{"error":{"message":"Not Found"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := QuickAddEvent(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"text":"t"}`))
		ue := err.(*UserError)
		if ue.Msg != "Calendar not found (ID: primary)." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("403 scope", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"Insufficient Permission"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := QuickAddEvent(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"text":"t"}`))
		ue := err.(*UserError)
		if ue.Msg != "Permission denied. Confirm the calendar.events scope was granted." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("500 passthrough", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 500, `{"error":{"message":"Backend exploded"}}`)
		})
		withCalendarBase(t, fg.server.URL)
		_, err := QuickAddEvent(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"text":"t"}`))
		ue := err.(*UserError)
		if ue.Msg != "Failed to quick-add event: Backend exploded" {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("401 refresh once", func(t *testing.T) {
		tok := newFakeTokenSource()
		calls := 0
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				cannedJSON(w, 401, `{"error":{"message":"Unauthorized"}}`)
				return
			}
			cannedJSON(w, 200, `{"id":"evQ","summary":"s"}`)
		})
		withCalendarBase(t, fg.server.URL)
		out, err := QuickAddEvent(context.Background(), fg.googleFor(tok), mustArgs(t, `{"text":"t"}`))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if calls != 2 || tok.invalidated != 1 {
			t.Fatalf("calls=%d invalidated=%d", calls, tok.invalidated)
		}
		if !strings.Contains(out, `"id": "evQ"`) {
			t.Fatalf("out = %s", out)
		}
	})
}
