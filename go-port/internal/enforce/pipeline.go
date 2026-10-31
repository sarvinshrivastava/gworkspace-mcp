// What to do with one tools/call: deny it, relay it (perhaps with rewritten
// arguments), or serve it from the gateway.
//
// Order matters and is fixed: guards that apply to every agent run first, so
// no early return can skip them; calendar tools are never folder-scoped;
// unconfined agents are then a plain relay, exactly as before policies.
// Port of src/enforce/pipeline.ts.
package enforce

import (
	"context"
	"fmt"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/policy"
)

// DenyReasons, verbatim from the TS DenyReason union.
const (
	ReasonNotFound           = "not_found"
	ReasonOutside            = "outside"
	ReasonTrashed            = "trashed"
	ReasonNoParents          = "no_parents"
	ReasonTooDeep            = "too_deep"
	ReasonDriveError         = "drive_error"
	ReasonCredentialRejected = "credential_rejected"
	ReasonCalendarRejected   = "calendar_rejected"
	ReasonNoScope            = "no_scope"
	ReasonBadID              = "bad_id"
	ReasonNoPrimary          = "no_primary"
	ReasonPrimaryUnavailable = "primary_unavailable"
	ReasonNotFolder          = "not_folder"
	ReasonPagination         = "pagination"
	ReasonInitialData        = "initial_data"
	ReasonTooLarge           = "too_large"
	ReasonQuickaddGuest      = "quickadd_guest"
	ReasonCalendarNotPrimary = "calendar_not_primary"
	ReasonEventHasGuests     = "event_has_guests"
	ReasonEventUnverifiable  = "event_unverifiable"
	ReasonNotEnabled         = "not_enabled"
	ReasonUnmapped           = "unmapped"
)

// DenyMessage: user-facing compatibility contract, verbatim from denyMessage().
func DenyMessage(reason string, id string, detail string) string {
	switch reason {
	case ReasonNotFound, ReasonOutside, ReasonTrashed, ReasonNoParents, ReasonTooDeep:
		if id == "" {
			id = "that file"
		}
		return fmt.Sprintf("Denied: `%s` was not found or is outside this agent's folder scope.", id)
	case ReasonDriveError:
		return "Could not verify the folder scope right now — try again."
	case ReasonCredentialRejected:
		msg := "Google access rejected"
		if detail != "" {
			msg += " (" + detail + ")"
		}
		return msg + " — do not retry. The Workspace owner must re-consent and update GOOGLE_TOKEN_JSON, or enable the API."
	case ReasonCalendarRejected:
		msg := "Google Calendar access rejected"
		if detail != "" {
			msg += " (" + detail + ")"
		}
		return msg + " — do not retry. The Workspace owner must enable the Calendar API or grant the Calendar scope."
	case ReasonNoScope:
		return "This agent has no Drive folder assigned yet."
	case ReasonBadID:
		return "Denied: that is not a valid Google file ID."
	case ReasonNoPrimary:
		return "Denied: this agent has no primary folder to create files in."
	case ReasonPrimaryUnavailable:
		return "Denied: this agent's folder is missing or in the trash."
	case ReasonNotFolder:
		return "Denied: the parent must be a folder inside this agent's scope."
	case ReasonPagination:
		return "Paging is not available here — narrow the query (name, mimeType, folderId or modifiedAfter) instead."
	case ReasonInitialData:
		return "Create the spreadsheet without data, then use writeSpreadsheet."
	case ReasonTooLarge:
		return "This agent's folder tree is too large to search (over 500 folders) — pass a folderId deeper in the tree."
	case ReasonQuickaddGuest:
		return "quickAddEvent cannot add guests — remove the e-mail address."
	case ReasonCalendarNotPrimary:
		return "Denied: events can only be written to this account's own calendar (calendarId \"primary\")."
	case ReasonEventUnverifiable:
		return "Could not check this event's guests, so it was not changed. Try again; changing only its time does not need the check."
	case ReasonEventHasGuests:
		return "Denied: that event has guests, so only its time (start, end) can be changed here."
	case ReasonNotEnabled, ReasonUnmapped:
		return "Denied: this tool is not enabled for this agent."
	}
	return "Denied."
}

// Plan is what the gateway does with the call.
type Plan struct {
	Kind      string // "deny" | "relay" | "serve"
	Message   string
	Reason    string
	Category  string // set on drive_error / credential_rejected denies
	Detail    string
	Args      map[string]any
	Mutated   bool
	Resources []string
	Serve     func(ctx context.Context) (string, error)
	After     func()
}

type PipelineDeps struct {
	Resolver *ScopeResolver
	Subtree  *SubtreeCache
	Drive    func() *drive.Client
}

// The argument holding the file a tool reads or writes.
var IDArg = map[string]string{
	"readDocument":     "documentId",
	"appendText":       "documentId",
	"insertText":       "documentId",
	"findAndReplace":   "documentId",
	"appendMarkdown":   "documentId",
	"readSpreadsheet":  "spreadsheetId",
	"writeSpreadsheet": "spreadsheetId",
	"appendRows":       "spreadsheetId",
}

var WriteTools = map[string]bool{
	"appendText": true, "insertText": true, "findAndReplace": true,
	"appendMarkdown": true, "writeSpreadsheet": true, "appendRows": true,
}

var CreateTools = map[string]bool{
	"createDocument": true, "createSpreadsheet": true, "createFolder": true,
}

func capResources(resources []string) []string {
	if len(resources) > policy.MaxResources {
		return resources[:policy.MaxResources]
	}
	return resources
}

func deny(reason string, resources []string, id, detail string) Plan {
	return Plan{
		Kind:      "deny",
		Reason:    reason,
		Message:   DenyMessage(reason, id, detail),
		Detail:    detail,
		Resources: capResources(resources),
	}
}

// denyDrive: a denial because Google could not answer or refused; the
// category says which, and cannot be left out.
func denyDrive(reason string, resources []string, category, detail string) Plan {
	msg := ""
	if reason == ReasonCredentialRejected {
		msg = DenyMessage(reason, "", detail)
	} else {
		msg = DenyMessage(reason, "", "")
	}
	return Plan{Kind: "deny", Reason: reason, Category: category, Message: msg, Detail: detail, Resources: capResources(resources)}
}

func isDriveVerdict(v Verdict) bool {
	return !v.OK && (v.Reason == ReasonDriveError || v.Reason == ReasonCredentialRejected)
}

func validID(value any) (string, bool) {
	s, ok := value.(string)
	return s, ok && IDRE.MatchString(s)
}

func PlanCall(ctx context.Context, agent *policy.AgentContext, tool string, rawArgs map[string]any, deps PipelineDeps) (Plan, error) {
	// 1. Guards for every agent, before any early return.
	args := rawArgs
	mutated := false
	if guard, ok := AllAgentGuards[tool]; ok {
		result := guard(rawArgs)
		if result.Deny != "" {
			return deny(result.Deny, nil, "", ""), nil
		}
		args = result.Args
		mutated = result.Mutated
	}

	// 1b. updateEvent is a PATCH: guests already on the event stay, and new
	// text syncs to their calendars even with sendUpdates "none". So an event
	// with guests keeps its words; only its time may move.
	if tool == "updateEvent" {
		if eventID, ok := args["eventId"].(string); ok && eventID != "" && hasNonTimeKeys(args) {
			d := deps.Drive()
			if d == nil {
				return deny(ReasonEventUnverifiable, nil, "", ""), nil
			}
			guests, err := d.EventGuests(ctx, "primary", eventID)
			if err != nil {
				// Fail closed, transient or not: an unchecked event may have guests.
				fmt.Printf("[guard] could not read an event's guests: %v\n", err)
				var de *drive.Error
				if drive.As(err, &de) && de.Rejection != "" {
					if de.Source == drive.SourceCalendar {
						return deny(ReasonCalendarRejected, nil, "", drive.RejectionDetail(de)), nil
					}
					return denyDrive(ReasonCredentialRejected, nil, drive.CategoryOf(err), drive.RejectionDetail(de)), nil
				}
				return deny(ReasonEventUnverifiable, nil, "", ""), nil
			}
			if len(guests) > 0 {
				return deny(ReasonEventHasGuests, nil, "", ""), nil
			}
		}
	}

	// 2. Calendar is never folder-scoped. 3. Unconfined agents relay as before.
	if policy.CalendarTools[tool] || !agent.Scoped {
		var resources []string
		if key, ok := IDArg[tool]; ok {
			if id, valid := validID(args[key]); valid {
				resources = []string{id}
			}
		}
		plan := Plan{Kind: "relay", Args: args, Mutated: mutated, Resources: resources}
		if tool == "createFolder" {
			plan.After = deps.Subtree.Flush
		}
		return plan, nil
	}

	// 4. Confined with no folder: no Docs, Sheets or Drive at all.
	if len(agent.Scopes) == 0 {
		return deny(ReasonNoScope, nil, "", ""), nil
	}

	scopeIDs := map[string]bool{}
	var primary string
	for _, s := range agent.Scopes {
		scopeIDs[s.FolderID] = true
		if s.IsPrimary {
			primary = s.FolderID
		}
	}
	r := deps.Resolver

	verdictDeny := func(v Verdict, id string) Plan {
		if isDriveVerdict(v) {
			return denyDrive(v.Reason, []string{id}, v.Category, v.Detail)
		}
		return deny(v.Reason, []string{id}, id, "")
	}

	// A folder argument the agent supplied: valid, in scope, and a folder.
	checkFolder := func(value any, live bool) (string, *Plan, error) {
		id, ok := validID(value)
		if !ok {
			p := deny(ReasonBadID, nil, "", "")
			return "", &p, nil
		}
		v := r.Check(ctx, id, scopeIDs, live)
		if !v.OK {
			p := verdictDeny(v, id)
			return "", &p, nil
		}
		if v.File.MimeType != drive.FolderMime {
			p := deny(ReasonNotFolder, []string{id}, id, "")
			return "", &p, nil
		}
		return id, nil, nil
	}

	// The primary, checked before anything is put in it.
	checkPrimary := func(live bool) (string, *Plan, error) {
		if primary == "" {
			p := deny(ReasonNoPrimary, nil, "", "")
			return "", &p, nil
		}
		v := r.Check(ctx, primary, scopeIDs, live)
		// Drive not answering is "try again", never "your folder is gone".
		if isDriveVerdict(v) {
			p := denyDrive(v.Reason, []string{primary}, v.Category, v.Detail)
			return "", &p, nil
		}
		if !v.OK || v.File.MimeType != drive.FolderMime {
			p := deny(ReasonPrimaryUnavailable, []string{primary}, "", "")
			return "", &p, nil
		}
		return primary, nil, nil
	}

	// The folder in key when the agent gave one, else the primary — and
	// filling in the primary rewrites the call. Presence, not a value check:
	// "" is "given", and the upstream treats it as Drive root.
	folderOrPrimary := func(key string, live bool) (string, *Plan, error) {
		if _, given := args[key]; given {
			return checkFolder(args[key], live)
		}
		p, plan, err := checkPrimary(live)
		if plan == nil && err == nil {
			mutated = true
		}
		return p, plan, err
	}

	// 6. Reads and writes of one file.
	if idArgKey, ok := IDArg[tool]; ok {
		id, valid := validID(args[idArgKey])
		if !valid {
			return deny(ReasonBadID, nil, "", ""), nil
		}
		v := r.Check(ctx, id, scopeIDs, WriteTools[tool])
		if !v.OK {
			return verdictDeny(v, id), nil
		}
		if tool == "writeSpreadsheet" || tool == "appendRows" {
			// USER_ENTERED evaluates formulas, and =IMPORTRANGE("<any id>")
			// reads a spreadsheet outside the scope into one inside it.
			if args["valueInputOption"] != "RAW" {
				args = copyArgs(args)
				args["valueInputOption"] = "RAW"
				mutated = true
			}
		}
		return Plan{Kind: "relay", Args: args, Mutated: mutated, Resources: []string{id}}, nil
	}

	// 7. Creates: inside the scope, or in the primary when no parent is given.
	if CreateTools[tool] {
		// The upstream writes initialData USER_ENTERED, unconditionally.
		if tool == "createSpreadsheet" {
			if _, given := args["initialData"]; given {
				return deny(ReasonInitialData, nil, "", ""), nil
			}
		}
		parent, plan, err := folderOrPrimary("parentFolderId", true)
		if err != nil {
			return Plan{}, err
		}
		if plan != nil {
			return *plan, nil
		}
		relayArgs := copyArgs(args)
		relayArgs["parentFolderId"] = parent
		p := Plan{Kind: "relay", Args: relayArgs, Mutated: mutated, Resources: []string{parent}}
		if tool == "createFolder" {
			p.After = deps.Subtree.Flush
		}
		return p, nil
	}

	// 8. Direct children of one folder.
	if tool == "listDriveFiles" {
		folder, plan, err := folderOrPrimary("folderId", false)
		if err != nil {
			return Plan{}, err
		}
		if plan != nil {
			return *plan, nil
		}
		relayArgs := copyArgs(args)
		relayArgs["folderId"] = folder
		return Plan{Kind: "relay", Args: relayArgs, Mutated: mutated, Resources: []string{folder}}, nil
	}

	// 9. Search, served here.
	if tool == "searchDriveFiles" {
		if _, given := args["pageToken"]; given {
			return deny(ReasonPagination, nil, "", ""), nil
		}
		var roots []string
		if _, given := args["folderId"]; given {
			folder, plan, err := checkFolder(args["folderId"], false)
			if err != nil {
				return Plan{}, err
			}
			if plan != nil {
				return *plan, nil
			}
			roots = []string{folder}
		} else {
			// TS iterates the scope Set in insertion order (primary first,
			// then created_at, folder_id); agent.Scopes carries that order,
			// and Go map iteration would randomize audit rows and chunking.
			for _, scope := range agent.Scopes {
				v := r.Check(ctx, scope.FolderID, scopeIDs, false)
				if v.OK {
					roots = append(roots, scope.FolderID)
				} else if isDriveVerdict(v) {
					return denyDrive(v.Reason, []string{scope.FolderID}, v.Category, v.Detail), nil
				}
			}
			if len(roots) == 0 {
				all := make([]string, 0, len(agent.Scopes))
				for _, scope := range agent.Scopes {
					all = append(all, scope.FolderID)
				}
				return deny(ReasonPrimaryUnavailable, all, "", ""), nil
			}
		}
		d := deps.Drive()
		if d == nil {
			return denyDrive(ReasonDriveError, nil, "unavailable", "not ready"), nil
		}
		searchArgs := copyArgs(args)
		delete(searchArgs, "folderId")
		return Plan{
			Kind:      "serve",
			Resources: capResources(roots),
			Serve: func(ctx context.Context) (string, error) {
				return ScopedSearchDriveFiles(ctx, d, deps.Subtree, roots, searchArgs, 25_000)
			},
		}, nil
	}

	// 10. Withheld from confined agents; unreachable through tools/list.
	return deny(ReasonUnmapped, nil, "", ""), nil
}

func hasNonTimeKeys(args map[string]any) bool {
	// TS: Object.keys(args).some(k => args[k] !== undefined && !TIME_ONLY.has(k)).
	// Object.keys includes keys set to null, and null !== undefined — so an
	// explicit null COUNTS here. Only absent keys are skipped.
	for key := range args {
		if !EventTimeOnlyKeys[key] {
			return true
		}
	}
	return false
}

func copyArgs(args map[string]any) map[string]any {
	out := make(map[string]any, len(args)+1)
	for k, v := range args {
		out[k] = v
	}
	return out
}
