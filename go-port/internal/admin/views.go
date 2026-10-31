// Server-rendered pages. Forms only, no JavaScript — the CSP forbids scripts
// outright — so every value below is escaped. Port of src/admin/views.ts.
package admin

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/policy"
)

// when is `${new Date(ms).toISOString().replace("T"," ").slice(0,19)} UTC`.
func when(ms *int64) string {
	if ms == nil {
		return "—"
	}
	return time.UnixMilli(*ms).UTC().Format("2006-01-02 15:04:05") + " UTC"
}

func csrfField(csrf string) Safe {
	return raw(`<input type="hidden" name="_csrf" value="` + Esc(csrf) + `">`)
}

func notes(items []string, kind string) Safe {
	var parts []Safe
	for _, text := range items {
		parts = append(parts, h(`<div class="`+kind+`">`, text, `</div>`))
	}
	if len(parts) == 0 {
		return raw("")
	}
	return raw(render(parts))
}

// AuditDecisions: the audit page's decision filter, in display order.
var AuditDecisions = []string{"allow", "mutate", "deny", "error"}

type loginState string

const (
	loginStarting      loginState = "starting"
	loginUnconfigured  loginState = "unconfigured"
	loginMisconfigured loginState = "misconfigured"
	loginLocked        loginState = "locked"
	loginForm          loginState = "form"
)

func loginBody(state loginState, errorText string) Safe {
	switch state {
	case loginStarting:
		return raw(`<div class="note">Starting — the service is still reading its configuration. Try again in a moment.</div>`)
	case loginUnconfigured:
		return raw(`<div class="note">Dashboard login is not configured. Store the output of <code>hash-password</code> as <code>DASHBOARD_ADMIN_PASSWORD_HASH</code> in the vault and restart.</div>`)
	case loginMisconfigured:
		return raw(`<div class="error">The stored <code>DASHBOARD_ADMIN_PASSWORD_HASH</code> is not a usable scrypt hash, so no password can match it. Replace it with the exact output of <code>hash-password</code> and restart.</div>`)
	case loginLocked:
		return raw(`<div class="error">Too many failed logins. Locked for 15 minutes. Every tunnel connection shares one source address, so this lock applies to everyone; restarting the container clears it.</div>`)
	default:
		var b strings.Builder
		if errorText != "" {
			b.WriteString(`<div class="error">` + Esc(errorText) + `</div>`)
		}
		b.WriteString(`
            <form method="post" action="/login" class="row">
              <input type="password" name="password" autocomplete="current-password" aria-label="Password" required autofocus>
              <button type="submit">Log in</button>
            </form>`)
		return raw(b.String())
	}
}

func loginPage(state loginState, errorText string) string {
	return layout("Log in", h(`<section><h1>gworkspace-mcp admin</h1>`, loginBody(state, errorText), `</section>`), false, "")
}

func errorPage(status int, message string) string {
	return layout(fmt.Sprintf("Error %d", status),
		h(`<section><h1>Error `, status, `</h1><div class="error">`, message, `</div><p><a href="/">Back</a></p></section>`), false, "")
}

type scopeLike struct {
	Scoped bool
	Scopes []policy.Scope
}

func scopeBadge(a scopeLike) Safe {
	if !a.Scoped {
		return raw(`<span class="badge">unconfined</span>`)
	}
	if len(a.Scopes) == 0 {
		return raw(`<span class="badge bad">no folder</span>`)
	}
	primary := false
	for _, s := range a.Scopes {
		if s.IsPrimary {
			primary = true
		}
	}
	if !primary {
		return raw(`<span class="badge bad">no primary</span>`)
	}
	return raw(`<span class="badge ok">confined</span>`)
}

// CollisionWarnings: Open WebUI merges every connection's tools into one
// registry, so the same tool name on two connections routes ambiguously.
type collisionAgent struct {
	Slug         string
	Tools        []string
	Credentialed bool
}

func collisionWarnings(agents []collisionAgent) []string {
	var live []collisionAgent
	for _, a := range agents {
		if a.Credentialed {
			live = append(live, a)
		}
	}
	var out []string
	for i := 0; i < len(live); i++ {
		for j := i + 1; j < len(live); j++ {
			var shared []string
			for _, t := range live[i].Tools {
				for _, u := range live[j].Tools {
					if t == u {
						shared = append(shared, t)
						break
					}
				}
			}
			if len(shared) > 0 {
				out = append(out, fmt.Sprintf(
					"%s and %s both expose %s. Open WebUI merges tools from every connection by name, so calls to these may reach either agent. Give each connection a disjoint set, or retire one connection.",
					live[i].Slug, live[j].Slug, strings.Join(shared, ", ")))
			}
		}
	}
	if len(live) > 4 {
		out = append(out, fmt.Sprintf(
			"%d agents have active credentials. Each Open WebUI connection re-sends its tool schemas on every chat turn; keep the count small.",
			len(live)))
	}
	return out
}

type warnAgent struct {
	Scoped bool
	Scopes []policy.Scope
	Grants []policy.Grant
}

func agentWarnings(a warnAgent) []string {
	var out []string
	if a.Scoped && len(a.Scopes) == 0 {
		out = append(out, "No folder assigned: this agent's Docs, Sheets and Drive calls are all denied until you add one.")
	}
	primary := false
	for _, s := range a.Scopes {
		if s.IsPrimary {
			primary = true
		}
	}
	if a.Scoped && len(a.Scopes) > 0 && !primary {
		out = append(out, "No primary folder: creates are denied. Mark one folder as primary.")
	}
	has := func(area, op string) bool {
		for _, g := range a.Grants {
			if g.Area == area && g.Op == op {
				return true
			}
		}
		return false
	}
	if a.Scoped && (has("docs", "read") || has("sheets", "read")) && !has("drive", "read") {
		out = append(out, "No discovery tool: a confined agent finds files with searchDriveFiles. Grant drive:read, or it can only open IDs it is given.")
	}
	if a.Scoped {
		out = append(out, "Folder scope covers Docs, Sheets and Drive only. Calendar grants reach the whole calendar.")
	}
	return out
}

type agentListRow struct {
	policy.AgentRow
	Tools        []string
	ActiveTokens int
	Scopes       []policy.Scope
}

func agentsPage(p struct {
	Agents      []agentListRow
	Warnings    []string
	Flash       []string
	CSRF        string
	AnyArchived bool
}) string {
	var rows []Safe
	for _, a := range p.Agents {
		creds := fmt.Sprintf("%d", a.ActiveTokens)
		if a.Slug == policy.DefaultAgent {
			creds = fmt.Sprintf("legacy bearer + %d", a.ActiveTokens)
		}
		rows = append(rows, raw(`<tr>
      <td><a href="/agents/`+Esc(a.Slug)+`"><code>`+Esc(a.Slug)+`</code></a><br><span class="muted">`+Esc(a.Name)+`</span></td>
      <td>`+Esc(len(a.Tools))+`</td>
      <td>`+Esc(creds)+`</td>
      <td>`+scopeBadge(scopeLike{a.Scoped, a.Scopes}).HTML+`</td>
    </tr>`))
	}
	var b strings.Builder
	b.WriteString(render(notes(p.Flash, "note")))
	b.WriteString(render(notes(p.Warnings, "error")))
	if len(p.Agents) == 1 && !p.AnyArchived {
		b.WriteString(`<div class="note">Only the <code>default</code> agent exists. It is what the existing Open WebUI connection's bearer maps to. Create an agent per connection, mint it a token, and use that token as the connection's Bearer key.</div>`)
	}
	b.WriteString(`
    <section><h1>Agents</h1><div class="wrap"><table>
      <thead><tr><th>Agent</th><th>Tools</th><th>Active credentials</th><th>Folder scope</th></tr></thead>
      <tbody>` + render(rows) + `</tbody></table></div></section>
    <section><h2>New agent</h2>
      <p class="muted">New agents start confined: no Docs, Sheets or Drive access until you assign a folder.</p>
      <form method="post" action="/agents" class="row">` + csrfField(p.CSRF).HTML + `
        <input type="text" name="slug" placeholder="slug (a-z, 0-9, -)" pattern="[a-z0-9][a-z0-9-]{1,31}" required aria-label="Slug">
        <input type="text" name="name" placeholder="Display name" maxlength="80" required aria-label="Name">
        <button type="submit">Create</button>
      </form></section>`)
	return layout("Agents", raw(b.String()), true, p.CSRF)
}

type pendingToken struct {
	ID        int64
	Plaintext string
}

func agentPage(p struct {
	Agent    policy.AgentRow
	Grants   []policy.Grant
	Tools    []string
	Scopes   []policy.Scope
	Tokens   []policy.TokenRow
	Pending  []pendingToken
	Flash    []string
	CSRF     string
	Warnings []string
}) string {
	a := p.Agent
	isDefault := a.Slug == policy.DefaultAgent
	granted := func(area, op string) bool {
		for _, g := range p.Grants {
			if g.Area == area && g.Op == op {
				return true
			}
		}
		return false
	}

	// Grants grid: AREAS × OPS, cells from the TOOL_MAP.
	var grid strings.Builder
	grid.WriteString(`<table><thead><tr><th>Area</th>`)
	for _, op := range policy.Ops {
		grid.WriteString(`<th>` + Esc(op) + `</th>`)
	}
	grid.WriteString(`</tr></thead><tbody>`)
	toolMap := policy.ToolMap()
	for _, area := range policy.Areas {
		grid.WriteString(`<tr><td>` + Esc(area) + `</td>`)
		for _, op := range policy.Ops {
			tools := toolMap[area+":"+op]
			if len(tools) == 0 {
				grid.WriteString(`<td class="muted">—</td>`)
			} else {
				checked := ""
				if granted(area, op) {
					checked = "checked"
				}
				grid.WriteString(`<td><label><input type="checkbox" name="g" value="` + Esc(area+":"+op) + `" ` + checked + `> <span class="muted">` + Esc(strings.Join(tools, ", ")) + `</span></label></td>`)
			}
		}
		grid.WriteString(`</tr>`)
	}
	grid.WriteString(`</tbody></table>`)

	var pendingHTML strings.Builder
	for _, pt := range p.Pending {
		pendingHTML.WriteString(`<div class="secret"><strong>New token #` + Esc(pt.ID) + ` — copy it now, it is shown once:</strong><br><code>` + Esc(pt.Plaintext) + `</code>
      <p class="muted">Use it as the Bearer key of one Open WebUI tool-server connection. A token you did not copy is useless; revoke it.</p></div>`)
	}

	var scopesHTML strings.Builder
	for _, s := range p.Scopes {
		name := s.FolderName
		if name == "" {
			name = "(unnamed)"
		}
		primaryCell := `<form class="inline" method="post" action="/agents/` + Esc(a.Slug) + `/scopes/` + Esc(s.FolderID) + `/primary">` + csrfField(p.CSRF).HTML + `<button class="quiet" type="submit">Make primary</button></form>`
		if s.IsPrimary {
			primaryCell = `<span class="badge ok">primary</span>`
		}
		scopesHTML.WriteString(`<tr><td>` + Esc(name) + `<br><code>` + Esc(s.FolderID) + `</code></td>
      <td>` + primaryCell + `</td>
      <td><form class="inline" method="post" action="/agents/` + Esc(a.Slug) + `/scopes/` + Esc(s.FolderID) + `/remove">` + csrfField(p.CSRF).HTML + `<button class="danger" type="submit">Remove</button></form></td></tr>`)
	}

	var tokensHTML strings.Builder
	for _, t := range p.Tokens {
		action := `revoked ` + when(t.RevokedAt)
		if t.RevokedAt == nil {
			action = `<form class="inline" method="post" action="/tokens/` + Esc(t.ID) + `/revoke">` + csrfField(p.CSRF).HTML + `<button class="danger" type="submit">Revoke</button></form>`
		}
		tokensHTML.WriteString(`<tr><td><code>` + Esc(t.Prefix) + `…</code></td><td>` + Esc(t.Label) + `</td><td>` + when(&t.CreatedAt) + `</td>
      <td>` + action + `</td></tr>`)
	}

	var scopeSection string
	if isDefault {
		scopeSection = `<section><h2>Folder scope</h2><p class="muted"><code>default</code> is the legacy Open WebUI connection and stays unconfined, so that connection keeps the tools it always had. Create a new agent for confined access.</p></section>`
	} else {
		var inner string
		if a.Scoped {
			inner = `<div class="wrap"><table><tbody>` + scopesHTML.String() + `</tbody></table></div>
          <form method="post" action="/agents/` + Esc(a.Slug) + `/scopes" class="row spaced">` + csrfField(p.CSRF).HTML + `
            <input type="text" name="folder" placeholder="Folder ID or Drive folder URL" required aria-label="Folder" size="48">
            <label><input type="checkbox" name="primary" value="1"> primary (creates land here)</label>
            <button type="submit">Add folder</button>
          </form>
          <p><a href="/agents/` + Esc(a.Slug) + `/browse?parent=root">Browse Drive folders…</a></p>`
			if len(p.Scopes) == 0 {
				inner += `<form method="post" action="/agents/` + Esc(a.Slug) + `/unconfine" class="row">` + csrfField(p.CSRF).HTML + `
            <label><input type="checkbox" name="confirm" value="yes" required> I understand this gives the agent the whole Drive</label>
            <button class="danger" type="submit">Remove confinement</button></form>`
			}
		} else {
			inner = `<p class="note">Unconfined: this agent reaches the whole Drive. Adding a folder confines it again.</p>
          <form method="post" action="/agents/` + Esc(a.Slug) + `/scopes" class="row">` + csrfField(p.CSRF).HTML + `
            <input type="text" name="folder" placeholder="Folder ID or Drive folder URL" required aria-label="Folder" size="48">
            <input type="hidden" name="primary" value="1"><button type="submit">Confine to folder</button></form>`
		}
		scopeSection = `<section><h2>Folder scope ` + scopeBadge(scopeLike{a.Scoped, p.Scopes}).HTML + `</h2>` + inner + `</section>`
	}

	archivedBanner := ""
	if a.ArchivedAt != nil {
		archivedBanner = `<div class="error">Archived ` + when(a.ArchivedAt) + `.</div>`
	}
	toolsText := "none"
	if len(p.Tools) > 0 {
		toolsText = strings.Join(p.Tools, ", ")
	}
	legacyNote := ""
	if isDefault {
		legacyNote = `<p class="muted">The legacy bearer (vault <code>GWORKSPACE_MCP_TOKEN</code>) always maps here and is rotated in the vault, not here.</p>`
	}
	archiveSection := ""
	if !isDefault && a.ArchivedAt == nil {
		archiveSection = `<section><h2>Archive</h2>
      <form method="post" action="/agents/` + Esc(a.Slug) + `/archive" class="row">` + csrfField(p.CSRF).HTML + `
        <span class="muted">Revokes every token. The slug stays reserved.</span>
        <button class="danger" type="submit">Archive agent</button></form></section>`
	}

	var b strings.Builder
	b.WriteString(render(notes(p.Flash, "note")))
	b.WriteString(pendingHTML.String())
	b.WriteString(render(notes(p.Warnings, "error")))
	b.WriteString(`
    <section><h1><code>` + Esc(a.Slug) + `</code> <span class="muted">` + Esc(a.Name) + `</span></h1>
      ` + archivedBanner + `
      <p class="muted">Enabled tools (` + Esc(len(p.Tools)) + `): ` + Esc(toolsText) + `</p></section>
    <section><h2>Grants</h2>
      <form method="post" action="/agents/` + Esc(a.Slug) + `/grants">` + csrfField(p.CSRF).HTML + `<div class="wrap">` + grid.String() + `</div>
      <p><button type="submit">Save grants</button></p></form></section>
    ` + scopeSection + `
    <section><h2>Tokens</h2>
      ` + legacyNote + `
      <div class="wrap"><table><thead><tr><th>Prefix</th><th>Label</th><th>Created</th><th></th></tr></thead><tbody>` + tokensHTML.String() + `</tbody></table></div>
      <form method="post" action="/agents/` + Esc(a.Slug) + `/tokens" class="row spaced">` + csrfField(p.CSRF).HTML + `
        <input type="text" name="label" placeholder="Label (e.g. Open WebUI chat-main)" maxlength="80" aria-label="Label">
        <button type="submit">Mint token</button></form></section>
    ` + archiveSection)
	return layout(a.Slug, raw(b.String()), true, p.CSRF)
}

func browsePage(p struct {
	Slug    string
	Parent  string
	Folders []struct{ ID, Name string }
	More    bool
	CSRF    string
}) string {
	var rows strings.Builder
	for _, f := range p.Folders {
		rows.WriteString(`<tr><td><a href="/agents/` + Esc(p.Slug) + `/browse?parent=` + url.QueryEscape(f.ID) + `">` + Esc(f.Name) + `</a><br><code>` + Esc(f.ID) + `</code></td>
      <td><form class="inline" method="post" action="/agents/` + Esc(p.Slug) + `/scopes">` + csrfField(p.CSRF).HTML + `<input type="hidden" name="folder" value="` + Esc(f.ID) + `"><button class="quiet" type="submit">Add as scope</button></form></td></tr>`)
	}
	listing := `<p class="muted">No folders here.</p>`
	if len(p.Folders) > 0 {
		listing = `<div class="wrap"><table><tbody>` + rows.String() + `</tbody></table></div>`
	}
	moreNote := ""
	if p.More {
		moreNote = `<p class="note">Only the first 200 folders are shown. Paste the folder's ID or URL on the agent page to add one not listed.</p>`
	}
	body := `<section><h1>Folders in <code>` + Esc(p.Parent) + `</code></h1>
      <p><a href="/agents/` + Esc(p.Slug) + `">← back to ` + Esc(p.Slug) + `</a> · <a href="/agents/` + Esc(p.Slug) + `/browse?parent=root">My Drive</a></p>
      ` + listing + moreNote + `
      <p class="muted">Shared drives and folders shared with this account are not browsable here; paste their URL on the agent page.</p></section>`
	return layout("Browse", raw(body), true, p.CSRF)
}

func auditPage(p struct {
	Rows     []policy.AuditRow
	Agent    string
	Decision string
	Agents   []string
	CSRF     string
}) string {
	option := func(value, current, label string) string {
		sel := ""
		if value == current {
			sel = "selected"
		}
		return `<option value="` + Esc(value) + `" ` + sel + `>` + Esc(label) + `</option>`
	}
	badgeClass := func(decision string) string {
		if decision == "deny" || decision == "error" {
			return "bad"
		}
		if decision == "mutate" {
			return "warn"
		}
		return "ok"
	}
	var rows strings.Builder
	for _, r := range p.Rows {
		agent := "—"
		if r.Agent != nil {
			agent = *r.Agent
		}
		tool := "—"
		if r.Tool != nil {
			tool = *r.Tool
		}
		var res strings.Builder
		for _, id := range r.Resources {
			res.WriteString(`<code>` + Esc(id) + `</code> `)
		}
		rows.WriteString(`<tr><td>` + when(&r.Ts) + `</td><td>` + Esc(r.Kind) + `</td><td>` + Esc(agent) + `</td><td>` + Esc(tool) + `</td>
      <td><span class="badge ` + badgeClass(r.Decision) + `">` + Esc(r.Decision) + `</span></td>
      <td>` + Esc(r.Reason) + `</td><td>` + res.String() + `</td><td>` + Esc(r.Source) + `</td></tr>`)
	}
	var agentOpts strings.Builder
	agentOpts.WriteString(option("", p.Agent, "all agents"))
	for _, a := range p.Agents {
		agentOpts.WriteString(option(a, p.Agent, a))
	}
	var decisionOpts strings.Builder
	decisionOpts.WriteString(option("", p.Decision, "all decisions"))
	for _, d := range AuditDecisions {
		decisionOpts.WriteString(option(d, p.Decision, d))
	}
	body := `<section><h1>Audit log</h1>
      <form method="get" action="/audit" class="row">
        <select name="agent" aria-label="Agent">` + agentOpts.String() + `</select>
        <select name="decision" aria-label="Decision">` + decisionOpts.String() + `</select>
        <button type="submit">Filter</button></form>
      <p class="muted">Newest 200. Resource IDs only; argument values and results are never recorded. Kept 90 days.</p>
      <div class="wrap"><table><thead><tr><th>Time</th><th>Kind</th><th>Agent</th><th>Tool</th><th>Decision</th><th>Reason</th><th>Resources</th><th>Source</th></tr></thead><tbody>` + rows.String() + `</tbody></table></div></section>`
	return layout("Audit", raw(body), true, p.CSRF)
}
