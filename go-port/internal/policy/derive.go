package policy

// From grants to tools. Static and exhaustive: an upstream tool that is not in
// this map is never exposed to anyone, whatever the allowlist says, so a new
// upstream release cannot widen an agent's surface on its own.
//
// Deliberately absent, forever: setFilePermission (sharing is an exfiltration
// channel), appendTableRows (the upstream registers two different tools under
// that name), and everything Gmail.

type Grant struct {
	Area string `json:"area"`
	Op   string `json:"op"`
}

var Areas = []string{"docs", "sheets", "drive", "calendar"}
var Ops = []string{"read", "write", "create"}

var toolMap = map[string][]string{
	"docs:read":       {"readDocument", "listDocuments", "searchDocuments"},
	"docs:write":      {"appendText", "insertText", "findAndReplace", "appendMarkdown"},
	"docs:create":     {"createDocument"},
	"sheets:read":     {"readSpreadsheet", "listSpreadsheets"},
	"sheets:write":    {"writeSpreadsheet", "appendRows"},
	"sheets:create":   {"createSpreadsheet"},
	"drive:read":      {"searchDriveFiles", "listDriveFiles"},
	"drive:write":     {},
	"drive:create":    {"createFolder"},
	"calendar:read":   {"listEvents"},
	"calendar:write":  {"createEvent", "updateEvent", "quickAddEvent"},
	"calendar:create": {},
}

// DefaultGrants is every grant that maps to at least one tool: exactly today's 20.
func DefaultGrants() []Grant {
	var out []Grant
	for _, area := range Areas {
		for _, op := range Ops {
			if len(toolMap[area+":"+op]) > 0 {
				out = append(out, Grant{Area: area, Op: op})
			}
		}
	}
	return out
}

// ScopedWithheld: upstream lists these from the whole Drive with no folder
// argument, so for a folder-confined agent they can only ever return nothing
// useful. Confined agents search with searchDriveFiles, which the gateway scopes.
var ScopedWithheld = map[string]bool{
	"listDocuments":    true,
	"searchDocuments":  true,
	"listSpreadsheets": true,
}

var CalendarTools = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range toolMap["calendar:read"] {
		m[n] = true
	}
	for _, n := range toolMap["calendar:write"] {
		m[n] = true
	}
	return m
}()

func IsGrantKey(value string) bool {
	_, ok := toolMap[value]
	return ok
}

func DeriveTools(grants []Grant) map[string]bool {
	tools := map[string]bool{}
	for _, g := range grants {
		for _, name := range toolMap[g.Area+":"+g.Op] {
			tools[name] = true
		}
	}
	return tools
}

// EnabledTools: the tools one agent sees, in the upstream's own order — so the
// legacy connection's tools/list is identical to what it was before policies
// existed.
func EnabledTools(grants []Grant, scoped bool, exposedOrder []string, withheldScoped, withheldAll map[string]bool) []string {
	derived := DeriveTools(grants)
	var out []string
	for _, name := range exposedOrder {
		if !derived[name] || withheldAll[name] {
			continue
		}
		if scoped && (ScopedWithheld[name] || withheldScoped[name]) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// ToolMap exposes the grant→tools map for the admin views (TS exports TOOL_MAP).
func ToolMap() map[string][]string { return toolMap }
