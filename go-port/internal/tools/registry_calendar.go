package tools

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Calendar tools — upstream reference:
// @a-bonus/google-docs-mcp@1.11.3 dist/tools/calendar/{listEvents,createEvent,updateEvent,quickAddEvent}.js
//
// Local helpers carry a dc (drive+calendar) prefix so they cannot
// collide with helpers from the other tool groups. Shared ParseArgs /
// OptInt / OptEnum are used where their semantics already match zod; presence
// and null handling (zod: null is neither undefined nor a string) are done
// here on a second decode of the raw object, without touching shared files.

// dcMap re-decodes the raw args object for presence/null checks. ParseArgs
// has already validated the JSON and rejected unknown top-level keys.
func dcMap(raw json.RawMessage) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(raw) != 0 {
		_ = json.Unmarshal(raw, &m) // validated upstream of this call
	}
	return m
}

// dcRejectNull mirrors zod's invalid_type rejection of JSON null for a field
// whose schema type is typ (Go's json.Unmarshal would silently turn null
// into the zero value, which zod never accepts).
func dcRejectNull(m map[string]json.RawMessage, typ string, keys ...string) error {
	for _, k := range keys {
		if v, ok := m[k]; ok && string(v) == "null" {
			return &UserError{Msg: fmt.Sprintf("Expected %s, received null: %q", typ, k)}
		}
	}
	return nil
}

// dcStr reads an optional string with presence tracking ("" is a value,
// unlike absent — zod .optional() keeps it).
func dcStr(m map[string]json.RawMessage, key string) (string, bool, error) {
	v, ok := m[key]
	if !ok {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", true, &UserError{Msg: fmt.Sprintf("Expected string for %q", key)}
	}
	return s, true, nil
}

func dcReqString(m map[string]json.RawMessage, key string) (string, error) {
	s, ok, err := dcStr(m, key)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", &UserError{Msg: fmt.Sprintf("Required: %q", key)}
	}
	return s, nil
}

func dcBool(m map[string]json.RawMessage, key string) (bool, bool, error) {
	v, ok := m[key]
	if !ok {
		return false, false, nil
	}
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return false, true, &UserError{Msg: fmt.Sprintf("Expected boolean for %q", key)}
	}
	return b, true, nil
}

// dcJSStr renders a possibly-absent Google field the way a JS template
// literal does: absent key → "undefined", explicit null → "null", a string
// unquoted, anything else verbatim.
func dcJSStr(raw json.RawMessage) string {
	if raw == nil {
		return "undefined"
	}
	if string(raw) == "null" {
		return "null"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// dcErrMessageOr is error.message || fallback — the 400 branches substitute
// 'Bad request', not ErrMessage's 'Unknown error'.
func dcErrMessageOr(body []byte, fallback string) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return fallback
}

// dcUUID is node:crypto randomUUID(): a v4 UUID.
func dcUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// dcEventDateTime is the upstream eventDateTimeSchema: a (non-strict)
// z.object with a refine requiring dateTime or date. Unknown keys are
// stripped, exactly as z.object does; key order follows the schema shape.
type dcEventDateTime struct {
	DateTime *string `json:"dateTime,omitempty"`
	Date     *string `json:"date,omitempty"`
	TimeZone *string `json:"timeZone,omitempty"`
}

const dcDateTimeRefineMsg = "Provide either dateTime (timed event) or date (all-day event)."

// dcParseEventDateTime parses an optional start/end field. Absent → nil.
// The refine is JS-truthy: "" counts as missing.
func dcParseEventDateTime(m map[string]json.RawMessage, key string) (*dcEventDateTime, error) {
	v, ok := m[key]
	if !ok {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(v, &obj); err != nil {
		return nil, &UserError{Msg: fmt.Sprintf("Expected object for %q", key)}
	}
	out := &dcEventDateTime{}
	for _, k := range []string{"dateTime", "date", "timeZone"} {
		kv, ok := obj[k]
		if !ok {
			continue
		}
		if string(kv) == "null" {
			return nil, &UserError{Msg: fmt.Sprintf("Expected string for %q", k)}
		}
		var s string
		if err := json.Unmarshal(kv, &s); err != nil {
			return nil, &UserError{Msg: fmt.Sprintf("Expected string for %q", k)}
		}
		switch k {
		case "dateTime":
			out.DateTime = &s
		case "date":
			out.Date = &s
		case "timeZone":
			out.TimeZone = &s
		}
	}
	// .refine((v) => v.dateTime || v.date) — skipped by zod only when the
	// whole value is undefined; here it is present, so it always runs.
	if (out.DateTime == nil || *out.DateTime == "") && (out.Date == nil || *out.Date == "") {
		return nil, &UserError{Msg: dcDateTimeRefineMsg}
	}
	return out, nil
}

// dcAttendee is one attendee of createEvent/updateEvent: a strictObject with
// required email and optional optional. A provided-but-absent optional key
// stays omitted from the request body (undefined is dropped).
type dcAttendee struct {
	Email    string `json:"email"`
	Optional *bool  `json:"optional,omitempty"`
}

func dcParseAttendees(m map[string]json.RawMessage) ([]dcAttendee, bool, error) {
	v, ok := m["attendees"]
	if !ok {
		return nil, false, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(v, &items); err != nil {
		return nil, true, &UserError{Msg: `Expected array for "attendees"`}
	}
	out := make([]dcAttendee, 0, len(items))
	for _, item := range items {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			return nil, true, &UserError{Msg: `Expected object for "attendees"`}
		}
		for k := range obj {
			if k != "email" && k != "optional" {
				return nil, true, &UserError{Msg: fmt.Sprintf("Unrecognized key: %q", k)}
			}
		}
		emailRaw, ok := obj["email"]
		if !ok {
			return nil, true, &UserError{Msg: `Required: "email"`}
		}
		if string(emailRaw) == "null" {
			return nil, true, &UserError{Msg: `Expected string for "email"`}
		}
		var email string
		if err := json.Unmarshal(emailRaw, &email); err != nil {
			return nil, true, &UserError{Msg: `Expected string for "email"`}
		}
		att := dcAttendee{Email: email}
		if optRaw, ok := obj["optional"]; ok {
			if string(optRaw) == "null" {
				return nil, true, &UserError{Msg: `Expected boolean for "optional"`}
			}
			var b bool
			if err := json.Unmarshal(optRaw, &b); err != nil {
				return nil, true, &UserError{Msg: `Expected boolean for "optional"`}
			}
			att.Optional = &b
		}
		out = append(out, att)
	}
	return out, true, nil
}

// dcCalendarURL is /calendars/{calendarId} with the segment escaped the way
// googleapis escapes path params.
func dcCalendarURL(calendarID string) string {
	return JoinURL(CalendarBase+"/calendars", calendarID)
}

// --- Response shapes -------------------------------------------------------
// Fields the upstream copies bare (id, status) are dropped iff Google omits
// them: json.RawMessage + omitempty, so an explicit null survives. Fields the
// upstream coerces with ?? null are always emitted; a *string collapses
// absent and null to null exactly like ??. start/end pass through verbatim
// (raw preserves Google's key order, re-indented by Stringify).

type calEventOut struct {
	ID               json.RawMessage  `json:"id,omitempty"`
	Status           json.RawMessage  `json:"status,omitempty"`
	Summary          *string          `json:"summary"`
	Description      *string          `json:"description"`
	Location         *string          `json:"location"`
	Start            json.RawMessage  `json:"start"`
	End              json.RawMessage  `json:"end"`
	Attendees        []calAttendeeOut `json:"attendees"`
	Organizer        *string          `json:"organizer"`
	HTMLLink         *string          `json:"htmlLink"`
	RecurringEventID *string          `json:"recurringEventId"`
}

type calAttendeeOut struct {
	Email          json.RawMessage `json:"email,omitempty"`
	ResponseStatus json.RawMessage `json:"responseStatus,omitempty"`
	Optional       bool            `json:"optional"`
}

// listEvents — upstream: dist/tools/calendar/listEvents.js
func ListEvents(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "calendarId", "q", "timeMin", "timeMax", "maxResults", "singleEvents")
	if err != nil {
		return "", err
	}
	m := dcMap(raw)
	if err := dcRejectNull(m, "string", "calendarId", "q", "timeMin", "timeMax"); err != nil {
		return "", err
	}
	if err := dcRejectNull(m, "number", "maxResults"); err != nil {
		return "", err
	}
	if err := dcRejectNull(m, "boolean", "singleEvents"); err != nil {
		return "", err
	}

	calendarID, _, err := dcStr(m, "calendarId")
	if err != nil {
		return "", err
	}
	if _, ok := m["calendarId"]; !ok {
		calendarID = "primary" // zod .default('primary') applies on undefined only
	}
	q, qGiven, err := dcStr(m, "q")
	if err != nil {
		return "", err
	}
	timeMin, timeMinGiven, err := dcStr(m, "timeMin")
	if err != nil {
		return "", err
	}
	timeMax, timeMaxGiven, err := dcStr(m, "timeMax")
	if err != nil {
		return "", err
	}
	maxResults, err := a.OptInt("maxResults", 25, 1, 2500)
	if err != nil {
		return "", err
	}
	singleEvents := true // zod .default(true)
	if b, ok, berr := dcBool(m, "singleEvents"); berr != nil {
		return "", berr
	} else if ok {
		singleEvents = b
	}
	// args.timeMin ?? new Date().toISOString()
	if !timeMinGiven {
		timeMin = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	}

	pairs := []string{}
	if qGiven { // googleapis omits undefined, keeps ""
		pairs = append(pairs, "q", q)
	}
	pairs = append(pairs, "timeMin", timeMin)
	if timeMaxGiven {
		pairs = append(pairs, "timeMax", timeMax)
	}
	pairs = append(pairs,
		"maxResults", strconv.Itoa(maxResults),
		"singleEvents", strconv.FormatBool(singleEvents),
	)
	if singleEvents { // orderBy: args.singleEvents ? 'startTime' : undefined
		pairs = append(pairs, "orderBy", "startTime")
	}
	u := dcCalendarURL(calendarID) + "/events?" + Query(pairs...)

	body, status, err := g.Do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", &UserError{Msg: "Failed to list events: " + err.Error()}
	}
	if status == 404 {
		return "", &UserError{Msg: fmt.Sprintf("Calendar not found (ID: %s).", calendarID)}
	}
	if status == 403 {
		return "", &UserError{Msg: "Permission denied. Confirm the calendar.events scope was granted during consent."}
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to list events: " + ErrMessage(body, status)}
	}

	var page struct {
		Items []struct {
			ID          json.RawMessage `json:"id"`
			Status      json.RawMessage `json:"status"`
			Summary     *string         `json:"summary"`
			Description *string         `json:"description"`
			Location    *string         `json:"location"`
			Start       json.RawMessage `json:"start"`
			End         json.RawMessage `json:"end"`
			Attendees   []struct {
				Email          json.RawMessage `json:"email"`
				ResponseStatus json.RawMessage `json:"responseStatus"`
				Optional       *bool           `json:"optional"`
			} `json:"attendees"`
			Organizer *struct {
				Email *string `json:"email"`
			} `json:"organizer"`
			HTMLLink         *string `json:"htmlLink"`
			RecurringEventID *string `json:"recurringEventId"`
		} `json:"items"`
		NextPageToken *string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return "", &UserError{Msg: "Failed to list events: " + err.Error()}
	}

	events := make([]calEventOut, 0, len(page.Items))
	for _, ev := range page.Items {
		out := calEventOut{
			ID:               ev.ID,
			Status:           ev.Status,
			Summary:          ev.Summary,
			Description:      ev.Description,
			Location:         ev.Location,
			Start:            ev.Start,
			End:              ev.End,
			Attendees:        make([]calAttendeeOut, 0, len(ev.Attendees)),
			HTMLLink:         ev.HTMLLink,
			RecurringEventID: ev.RecurringEventID,
		}
		if ev.Organizer != nil {
			out.Organizer = ev.Organizer.Email
		}
		for _, att := range ev.Attendees {
			optional := false // a.optional ?? false
			if att.Optional != nil {
				optional = *att.Optional
			}
			out.Attendees = append(out.Attendees, calAttendeeOut{
				Email:          att.Email,
				ResponseStatus: att.ResponseStatus,
				Optional:       optional,
			})
		}
		events = append(events, out)
	}
	return Stringify(struct {
		Events        []calEventOut `json:"events"`
		Count         int           `json:"count"`
		NextPageToken *string       `json:"nextPageToken"`
	}{events, len(events), page.NextPageToken}), nil
}

// dcConferenceData is the Meet createRequest the upstream attaches when
// conferenceData is true.
type dcConferenceData struct {
	CreateRequest struct {
		RequestID             string `json:"requestId"`
		ConferenceSolutionKey struct {
			Type string `json:"type"`
		} `json:"conferenceSolutionKey"`
	} `json:"createRequest"`
}

func dcNewConferenceData() *dcConferenceData {
	cd := &dcConferenceData{}
	cd.CreateRequest.RequestID = "mcp-" + dcUUID()
	cd.CreateRequest.ConferenceSolutionKey.Type = "hangoutsMeet"
	return cd
}

// calWriteResp is the shared decode for the insert/patch/quickAdd responses.
type calWriteResp struct {
	ID          json.RawMessage   `json:"id"`
	Summary     json.RawMessage   `json:"summary"`
	Start       json.RawMessage   `json:"start"`
	End         json.RawMessage   `json:"end"`
	HTMLLink    json.RawMessage   `json:"htmlLink"`
	HangoutLink *string           `json:"hangoutLink"`
	Updated     json.RawMessage   `json:"updated"`
	Attendees   []json.RawMessage `json:"attendees"`
}

// createEvent — upstream: dist/tools/calendar/createEvent.js
func CreateEvent(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "calendarId", "summary", "description", "location", "start", "end", "attendees", "sendUpdates", "conferenceData")
	if err != nil {
		return "", err
	}
	m := dcMap(raw)
	if err := dcRejectNull(m, "string", "calendarId", "summary", "description", "location", "sendUpdates"); err != nil {
		return "", err
	}
	if err := dcRejectNull(m, "object", "start", "end"); err != nil {
		return "", err
	}
	if err := dcRejectNull(m, "array", "attendees"); err != nil {
		return "", err
	}
	if err := dcRejectNull(m, "boolean", "conferenceData"); err != nil {
		return "", err
	}

	calendarID, _, err := dcStr(m, "calendarId")
	if err != nil {
		return "", err
	}
	if _, ok := m["calendarId"]; !ok {
		calendarID = "primary"
	}
	summary, err := dcReqString(m, "summary")
	if err != nil {
		return "", err
	}
	description, descGiven, err := dcStr(m, "description")
	if err != nil {
		return "", err
	}
	location, locGiven, err := dcStr(m, "location")
	if err != nil {
		return "", err
	}
	start, err := dcParseEventDateTime(m, "start")
	if err != nil {
		return "", err
	}
	if _, ok := m["start"]; !ok {
		return "", &UserError{Msg: `Required: "start"`}
	}
	end, err := dcParseEventDateTime(m, "end")
	if err != nil {
		return "", err
	}
	if _, ok := m["end"]; !ok {
		return "", &UserError{Msg: `Required: "end"`}
	}
	attendees, attendeesGiven, err := dcParseAttendees(m)
	if err != nil {
		return "", err
	}
	sendUpdates, err := a.OptEnum("sendUpdates", "none", "all", "externalOnly", "none")
	if err != nil {
		return "", err
	}
	conferenceData := false
	if b, ok, berr := dcBool(m, "conferenceData"); berr != nil {
		return "", berr
	} else if ok {
		conferenceData = b
	}

	// requestBody key order matches the upstream literal.
	type createEventBody struct {
		Summary        string            `json:"summary"`
		Description    *string           `json:"description,omitempty"`
		Location       *string           `json:"location,omitempty"`
		Start          *dcEventDateTime  `json:"start"`
		End            *dcEventDateTime  `json:"end"`
		Attendees      *[]dcAttendee     `json:"attendees,omitempty"`
		ConferenceData *dcConferenceData `json:"conferenceData,omitempty"`
	}
	body := createEventBody{Summary: summary, Start: start, End: end}
	if descGiven {
		body.Description = &description
	}
	if locGiven {
		body.Location = &location
	}
	if attendeesGiven {
		body.Attendees = &attendees
	}
	if conferenceData {
		body.ConferenceData = dcNewConferenceData()
	}

	pairs := []string{"sendUpdates", sendUpdates}
	if conferenceData { // conferenceDataVersion: 1 : undefined
		pairs = append(pairs, "conferenceDataVersion", "1")
	}
	u := dcCalendarURL(calendarID) + "/events?" + Query(pairs...)

	respBody, status, err := g.Do(ctx, http.MethodPost, u, body)
	if err != nil {
		return "", &UserError{Msg: "Failed to create event: " + err.Error()}
	}
	if status == 404 {
		return "", &UserError{Msg: fmt.Sprintf("Calendar not found (ID: %s).", calendarID)}
	}
	if status == 403 {
		return "", &UserError{Msg: "Permission denied. Confirm the calendar.events scope was granted."}
	}
	if status == 400 {
		return "", &UserError{Msg: fmt.Sprintf("Calendar rejected the event: %s. Check that start/end formats are valid RFC3339.", dcErrMessageOr(respBody, "Bad request"))}
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to create event: " + ErrMessage(respBody, status)}
	}

	var event calWriteResp
	if err := json.Unmarshal(respBody, &event); err != nil {
		return "", &UserError{Msg: "Failed to create event: " + err.Error()}
	}
	return Stringify(struct {
		Success     bool            `json:"success"`
		ID          json.RawMessage `json:"id,omitempty"`
		Summary     json.RawMessage `json:"summary,omitempty"`
		Start       json.RawMessage `json:"start,omitempty"`
		End         json.RawMessage `json:"end,omitempty"`
		HTMLLink    json.RawMessage `json:"htmlLink,omitempty"`
		HangoutLink *string         `json:"hangoutLink"`
		Attendees   int             `json:"attendees"`
		Message     string          `json:"message"`
	}{true, event.ID, event.Summary, event.Start, event.End, event.HTMLLink,
		event.HangoutLink, len(event.Attendees),
		"Event \"" + dcJSStr(event.Summary) + "\" created."}), nil
}

// updateEvent — upstream: dist/tools/calendar/updateEvent.js
func UpdateEvent(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "calendarId", "eventId", "summary", "description", "location", "start", "end", "attendees", "sendUpdates")
	if err != nil {
		return "", err
	}
	m := dcMap(raw)
	if err := dcRejectNull(m, "string", "calendarId", "eventId", "summary", "description", "location", "sendUpdates"); err != nil {
		return "", err
	}
	if err := dcRejectNull(m, "object", "start", "end"); err != nil {
		return "", err
	}
	if err := dcRejectNull(m, "array", "attendees"); err != nil {
		return "", err
	}

	calendarID, _, err := dcStr(m, "calendarId")
	if err != nil {
		return "", err
	}
	if _, ok := m["calendarId"]; !ok {
		calendarID = "primary"
	}
	eventID, err := dcReqString(m, "eventId")
	if err != nil {
		return "", err
	}
	summary, summaryGiven, err := dcStr(m, "summary")
	if err != nil {
		return "", err
	}
	description, descGiven, err := dcStr(m, "description")
	if err != nil {
		return "", err
	}
	location, locGiven, err := dcStr(m, "location")
	if err != nil {
		return "", err
	}
	start, err := dcParseEventDateTime(m, "start")
	if err != nil {
		return "", err
	}
	end, err := dcParseEventDateTime(m, "end")
	if err != nil {
		return "", err
	}
	attendees, attendeesGiven, err := dcParseAttendees(m)
	if err != nil {
		return "", err
	}
	sendUpdates, err := a.OptEnum("sendUpdates", "none", "all", "externalOnly", "none")
	if err != nil {
		return "", err
	}

	// The upstream's Object.keys(requestBody).length === 0 check: calendarId,
	// eventId and sendUpdates are not body fields.
	if !summaryGiven && !descGiven && !locGiven && start == nil && end == nil && !attendeesGiven {
		return "", &UserError{Msg: "No fields provided to update. Pass at least one of summary, description, location, start, end, or attendees."}
	}

	// Body key order matches the upstream assignment order.
	type updateEventBody struct {
		Summary     *string          `json:"summary,omitempty"`
		Description *string          `json:"description,omitempty"`
		Location    *string          `json:"location,omitempty"`
		Start       *dcEventDateTime `json:"start,omitempty"`
		End         *dcEventDateTime `json:"end,omitempty"`
		Attendees   *[]dcAttendee    `json:"attendees,omitempty"`
	}
	body := updateEventBody{}
	if summaryGiven {
		body.Summary = &summary
	}
	if descGiven {
		body.Description = &description
	}
	if locGiven {
		body.Location = &location
	}
	if start != nil {
		body.Start = start
	}
	if end != nil {
		body.End = end
	}
	if attendeesGiven {
		body.Attendees = &attendees
	}

	u := JoinURL(dcCalendarURL(calendarID)+"/events", eventID) + "?" + Query("sendUpdates", sendUpdates)
	respBody, status, err := g.Do(ctx, http.MethodPatch, u, body)
	if err != nil {
		return "", &UserError{Msg: "Failed to update event: " + err.Error()}
	}
	if status == 404 {
		return "", &UserError{Msg: fmt.Sprintf("Event not found: %s on calendar %s.", eventID, calendarID)}
	}
	if status == 403 {
		return "", &UserError{Msg: "Permission denied. Confirm the calendar.events scope was granted."}
	}
	if status == 400 {
		return "", &UserError{Msg: fmt.Sprintf("Calendar rejected the update: %s.", dcErrMessageOr(respBody, "Bad request"))}
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to update event: " + ErrMessage(respBody, status)}
	}

	var event calWriteResp
	if err := json.Unmarshal(respBody, &event); err != nil {
		return "", &UserError{Msg: "Failed to update event: " + err.Error()}
	}
	return Stringify(struct {
		Success  bool            `json:"success"`
		ID       json.RawMessage `json:"id,omitempty"`
		Summary  json.RawMessage `json:"summary,omitempty"`
		Start    json.RawMessage `json:"start,omitempty"`
		End      json.RawMessage `json:"end,omitempty"`
		HTMLLink json.RawMessage `json:"htmlLink,omitempty"`
		Updated  json.RawMessage `json:"updated,omitempty"`
		Message  string          `json:"message"`
	}{true, event.ID, event.Summary, event.Start, event.End, event.HTMLLink,
		event.Updated, fmt.Sprintf("Event %s updated.", dcJSStr(event.ID))}), nil
}

// quickAddEvent — upstream: dist/tools/calendar/quickAdd.js
func QuickAddEvent(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "calendarId", "text", "sendUpdates")
	if err != nil {
		return "", err
	}
	m := dcMap(raw)
	if err := dcRejectNull(m, "string", "calendarId", "text", "sendUpdates"); err != nil {
		return "", err
	}
	calendarID, _, err := dcStr(m, "calendarId")
	if err != nil {
		return "", err
	}
	if _, ok := m["calendarId"]; !ok {
		calendarID = "primary"
	}
	text, err := dcReqString(m, "text")
	if err != nil {
		return "", err
	}
	sendUpdates, err := a.OptEnum("sendUpdates", "none", "all", "externalOnly", "none")
	if err != nil {
		return "", err
	}

	u := dcCalendarURL(calendarID) + "/events/quickAdd?" + Query("text", text, "sendUpdates", sendUpdates)
	respBody, status, err := g.Do(ctx, http.MethodPost, u, nil)
	if err != nil {
		return "", &UserError{Msg: "Failed to quick-add event: " + err.Error()}
	}
	if status == 404 {
		return "", &UserError{Msg: fmt.Sprintf("Calendar not found (ID: %s).", calendarID)}
	}
	if status == 403 {
		return "", &UserError{Msg: "Permission denied. Confirm the calendar.events scope was granted."}
	}
	if status == 400 {
		return "", &UserError{Msg: fmt.Sprintf("Calendar could not parse \"%s\" as an event. Try a clearer time format.", text)}
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to quick-add event: " + ErrMessage(respBody, status)}
	}

	var event calWriteResp
	if err := json.Unmarshal(respBody, &event); err != nil {
		return "", &UserError{Msg: "Failed to quick-add event: " + err.Error()}
	}
	return Stringify(struct {
		Success  bool            `json:"success"`
		ID       json.RawMessage `json:"id,omitempty"`
		Summary  json.RawMessage `json:"summary,omitempty"`
		Start    json.RawMessage `json:"start,omitempty"`
		End      json.RawMessage `json:"end,omitempty"`
		HTMLLink json.RawMessage `json:"htmlLink,omitempty"`
		Message  string          `json:"message"`
	}{true, event.ID, event.Summary, event.Start, event.End, event.HTMLLink,
		"Event \"" + dcJSStr(event.Summary) + "\" created from \"" + text + "\"."}), nil
}

// CalendarHandlers is the Calendar tools' registry contribution.
func CalendarHandlers() map[string]Handler {
	return map[string]Handler{
		"listEvents":    ListEvents,
		"createEvent":   CreateEvent,
		"updateEvent":   UpdateEvent,
		"quickAddEvent": QuickAddEvent,
	}
}
