// The dashboard's routes, after the session gate. Port of the second half of
// src/admin/app.ts.
package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/enforce"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/policy"
)

func (a *adminApp) postLogout(w http.ResponseWriter, r *http.Request, c reqCtx) {
	a.sessions.Destroy(c.session.ID)
	w.Header().Set("Set-Cookie", Cookie+"=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0")
	if au := a.d.Audit(); au != nil {
		au.Admin("logout", nil, nil, a.source(r))
	}
	http.Redirect(w, r, "/login", 303)
}

func (a *adminApp) getAgents(w http.ResponseWriter, r *http.Request, c reqCtx) {
	agents, err := c.store.ListAgents(false)
	if err != nil {
		panic(err)
	}
	var rows []agentListRow
	for _, agent := range agents {
		tokens, err := c.store.ListTokens(agent.Slug)
		if err != nil {
			panic(err)
		}
		active := 0
		for _, t := range tokens {
			if t.RevokedAt == nil {
				active++
			}
		}
		scopes, err := c.store.GetScopes(agent.Slug)
		if err != nil {
			panic(err)
		}
		rows = append(rows, agentListRow{agent, a.toolsOf(c.store, agent.Slug, agent.Scoped), active, scopes})
	}
	var collisionInput []collisionAgent
	for _, row := range rows {
		collisionInput = append(collisionInput, collisionAgent{
			Slug:         row.Slug,
			Tools:        row.Tools,
			Credentialed: row.Slug == policy.DefaultAgent || row.ActiveTokens > 0,
		})
	}
	warnings := collisionWarnings(collisionInput)
	flash := c.session.Flash
	c.session.Flash = nil
	all, err := c.store.ListAgents(true)
	if err != nil {
		panic(err)
	}
	a.page(w, 200, agentsPage(struct {
		Agents      []agentListRow
		Warnings    []string
		Flash       []string
		CSRF        string
		AnyArchived bool
	}{rows, warnings, flash, c.session.CSRF, len(all) > len(rows)}))
}

func (a *adminApp) postAgents(w http.ResponseWriter, r *http.Request, c reqCtx) {
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	name := r.PostFormValue("name")
	if a.attempt(c.session, "agent.create", func() error { return c.store.CreateAgent(slug, name) }) {
		a.done(w, r, c, "agent.create", []string{slug}, "/agents/"+slug, &slug)
		return
	}
	http.Redirect(w, r, "/", 303)
}

func (a *adminApp) getAgent(w http.ResponseWriter, r *http.Request, c reqCtx) {
	agent := a.requireAgent(w, r, c)
	if agent == nil {
		return
	}
	grants, err := c.store.GetGrants(agent.Slug)
	if err != nil {
		panic(err)
	}
	scopes, err := c.store.GetScopes(agent.Slug)
	if err != nil {
		panic(err)
	}
	tokens, err := c.store.ListTokens(agent.Slug)
	if err != nil {
		panic(err)
	}
	var pending []pendingToken
	for _, t := range tokens {
		if plaintext, ok := c.session.Pending[t.ID]; ok {
			pending = append(pending, pendingToken{t.ID, plaintext})
			delete(c.session.Pending, t.ID)
		}
	}
	flash := c.session.Flash
	c.session.Flash = nil
	a.page(w, 200, agentPage(struct {
		Agent    policy.AgentRow
		Grants   []policy.Grant
		Tools    []string
		Scopes   []policy.Scope
		Tokens   []policy.TokenRow
		Pending  []pendingToken
		Flash    []string
		CSRF     string
		Warnings []string
	}{*agent, grants, a.toolsOf(c.store, agent.Slug, agent.Scoped), scopes, tokens, pending, flash, c.session.CSRF,
		agentWarnings(warnAgent{agent.Scoped, scopes, grants})}))
}

func (a *adminApp) postGrants(w http.ResponseWriter, r *http.Request, c reqCtx) {
	agent := a.requireLiveAgent(w, r, c)
	if agent == nil {
		return
	}
	values := r.PostForm["g"]
	var grants []policy.Grant
	for _, v := range values {
		if !policy.IsGrantKey(v) {
			a.page(w, 400, errorPage(400, "Unknown grant."))
			return
		}
		parts := strings.SplitN(v, ":", 2)
		grants = append(grants, policy.Grant{Area: parts[0], Op: parts[1]})
	}
	if err := c.store.SetGrants(agent.Slug, grants); err != nil {
		panic(err)
	}
	a.done(w, r, c, "grants.set", []string{agent.Slug}, "/agents/"+agent.Slug, &agent.Slug)
}

func (a *adminApp) postArchive(w http.ResponseWriter, r *http.Request, c reqCtx) {
	agent := a.requireLiveAgent(w, r, c)
	if agent == nil {
		return
	}
	if a.attempt(c.session, "agent.archive", func() error { return c.store.ArchiveAgent(agent.Slug) }) {
		a.done(w, r, c, "agent.archive", []string{agent.Slug}, "/", &agent.Slug)
		return
	}
	http.Redirect(w, r, "/agents/"+agent.Slug, 303)
}

func (a *adminApp) postUnconfine(w http.ResponseWriter, r *http.Request, c reqCtx) {
	agent := a.requireLiveAgent(w, r, c)
	if agent == nil {
		return
	}
	if r.PostFormValue("confirm") != "yes" {
		c.session.Flash = append(c.session.Flash, "Tick the confirmation to remove confinement.")
		http.Redirect(w, r, "/agents/"+agent.Slug, 303)
		return
	}
	if a.attempt(c.session, "agent.unconfine", func() error { return c.store.SetUnconfined(agent.Slug) }) {
		a.done(w, r, c, "agent.unconfine", []string{agent.Slug}, "/agents/"+agent.Slug, &agent.Slug)
		return
	}
	http.Redirect(w, r, "/agents/"+agent.Slug, 303)
}

func (a *adminApp) postScopes(w http.ResponseWriter, r *http.Request, c reqCtx) {
	agent := a.requireLiveAgent(w, r, c)
	if agent == nil {
		return
	}
	back := "/agents/" + agent.Slug
	fail := func(message string) {
		c.session.Flash = append(c.session.Flash, message)
		http.Redirect(w, r, back, 303)
	}
	if agent.Slug == policy.DefaultAgent {
		fail(storeMessages["reserved"])
		return
	}
	id := ParseFolderInput(r.PostFormValue("folder"))
	if id == nil {
		fail("That is not a folder ID or a Drive folder URL.")
		return
	}
	if a.d.Drive() == nil {
		fail("Drive is not connected yet. Try again in a moment.")
		return
	}
	// Live: this decides what an agent may reach, so no cached answer.
	file, err := a.d.Resolver.Node(r.Context(), *id, true)
	if err != nil {
		fail("Could not check that folder with Drive: " + err.Error())
		return
	}
	root, err := a.d.Resolver.Node(r.Context(), "root", false)
	if err != nil {
		fail("Could not check that folder with Drive: " + err.Error())
		return
	}
	if file == nil {
		fail("Drive cannot find that folder, or this account cannot see it.")
		return
	}
	if file.MimeType != drive.FolderMime {
		fail("That is a file, not a folder.")
		return
	}
	if file.Trashed {
		fail("That folder is in the trash.")
		return
	}
	// The whole Drive is not a scope. Shared folders without a visible parent are fine.
	if root != nil && file.ID == root.ID {
		fail("You cannot scope an agent to the Drive root.")
		return
	}
	primary := r.PostFormValue("primary") == "1"
	if a.attempt(c.session, "scope.add", func() error { return c.store.AddScope(agent.Slug, file.ID, file.Name, primary) }) {
		a.d.Subtree.Flush()
		a.done(w, r, c, "scope.add", []string{agent.Slug, file.ID}, back, &agent.Slug)
		return
	}
	http.Redirect(w, r, back, 303)
}

func (a *adminApp) postScopeRemove(w http.ResponseWriter, r *http.Request, c reqCtx) {
	agent := a.requireLiveAgent(w, r, c)
	if agent == nil {
		return
	}
	folderID := r.PathValue("folderId")
	if a.attempt(c.session, "scope.remove", func() error { return c.store.RemoveScope(agent.Slug, folderID) }) {
		a.d.Subtree.Flush()
		a.done(w, r, c, "scope.remove", []string{agent.Slug, folderID}, "/agents/"+agent.Slug, &agent.Slug)
		return
	}
	http.Redirect(w, r, "/agents/"+agent.Slug, 303)
}

func (a *adminApp) postScopePrimary(w http.ResponseWriter, r *http.Request, c reqCtx) {
	agent := a.requireLiveAgent(w, r, c)
	if agent == nil {
		return
	}
	folderID := r.PathValue("folderId")
	if a.attempt(c.session, "scope.primary", func() error { return c.store.SetPrimary(agent.Slug, folderID) }) {
		a.done(w, r, c, "scope.primary", []string{agent.Slug, folderID}, "/agents/"+agent.Slug, &agent.Slug)
		return
	}
	http.Redirect(w, r, "/agents/"+agent.Slug, 303)
}

func (a *adminApp) getBrowse(w http.ResponseWriter, r *http.Request, c reqCtx) {
	agent := a.requireAgent(w, r, c)
	if agent == nil {
		return
	}
	parent := r.URL.Query().Get("parent")
	if parent == "" {
		parent = "root"
	}
	// It goes into a Drive query; nothing but an ID shape gets there.
	if parent != "root" && !enforce.IDRE.MatchString(parent) {
		a.page(w, 400, errorPage(400, "Not a folder ID."))
		return
	}
	d := a.d.Drive()
	if d == nil {
		a.page(w, 503, errorPage(503, "Drive is not connected yet."))
		return
	}
	listing, err := d.ListFiles(r.Context(), drive.ListParams{
		Q:        `'` + parent + `' in parents and mimeType='` + drive.FolderMime + `' and trashed=false`,
		Fields:   "files(id,name)",
		PageSize: 200,
		OrderBy:  "name",
	})
	if err != nil {
		a.page(w, 502, errorPage(502, "Drive did not answer: "+err.Error()))
		return
	}
	var folders []struct{ ID, Name string }
	for _, f := range listing.Files {
		id, _ := f["id"].(string)
		name, _ := f["name"].(string)
		folders = append(folders, struct{ ID, Name string }{id, name})
	}
	a.page(w, 200, browsePage(struct {
		Slug    string
		Parent  string
		Folders []struct{ ID, Name string }
		More    bool
		CSRF    string
	}{agent.Slug, parent, folders, listing.NextPageToken != "", c.session.CSRF}))
}

func (a *adminApp) postTokens(w http.ResponseWriter, r *http.Request, c reqCtx) {
	agent := a.requireLiveAgent(w, r, c)
	if agent == nil {
		return
	}
	plaintext, hash, prefix, err := policy.MintToken()
	if err != nil {
		panic(err)
	}
	label := r.PostFormValue("label")
	// Stored before it is ever shown: a crash in between loses nothing usable.
	id, err := c.store.InsertToken(agent.Slug, hash, prefix, label)
	if err != nil {
		panic(err)
	}
	c.session.Pending[id] = plaintext
	a.done(w, r, c, "token.mint", []string{agent.Slug, prefix}, "/agents/"+agent.Slug, &agent.Slug)
}

func (a *adminApp) postTokenRevoke(w http.ResponseWriter, r *http.Request, c reqCtx) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.page(w, 404, errorPage(404, "No such token."))
		return
	}
	token, err := c.store.GetToken(id)
	if err != nil {
		panic(err)
	}
	if token == nil {
		a.page(w, 404, errorPage(404, "No such token."))
		return
	}
	if err := c.store.RevokeToken(id); err != nil {
		panic(err)
	}
	a.done(w, r, c, "token.revoke", []string{token.Agent, token.Prefix}, "/agents/"+token.Agent, &token.Agent)
}

func (a *adminApp) getAudit(w http.ResponseWriter, r *http.Request, c reqCtx) {
	q := r.URL.Query()
	agent := q.Get("agent")
	decision := q.Get("decision")
	validDecision := false
	for _, d := range AuditDecisions {
		if decision == d {
			validDecision = true
		}
	}
	if !validDecision {
		decision = ""
	}
	rows, err := c.store.QueryAudit(policy.AuditFilter{Agent: agent, Decision: decision, Limit: 200})
	if err != nil {
		panic(err)
	}
	all, err := c.store.ListAgents(true)
	if err != nil {
		panic(err)
	}
	var slugs []string
	for _, ag := range all {
		slugs = append(slugs, ag.Slug)
	}
	a.page(w, 200, auditPage(struct {
		Rows     []policy.AuditRow
		Agent    string
		Decision string
		Agents   []string
		CSRF     string
	}{rows, agent, decision, slugs, c.session.CSRF}))
}

func (a *adminApp) postCacheFlush(w http.ResponseWriter, r *http.Request, c reqCtx) {
	a.d.Resolver.Flush()
	a.d.Subtree.Flush()
	a.done(w, r, c, "cache.flush", nil, "/", nil)
}
