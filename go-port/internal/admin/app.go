// The admin dashboard — a separate HTTP app, so no /mcp route exists on this
// port at all. Port of src/admin/app.ts.
package admin

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/audit"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/enforce"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/policy"
)

const Cookie = "gws_admin_sid"

// Deps is AdminDeps.
type Deps struct {
	Store        func() *policy.PolicyStore
	DBError      func() string
	Audit        func() *audit.Audit
	Drive        func() *drive.Client
	Resolver     *enforce.ScopeResolver
	Subtree      *enforce.SubtreeCache
	PasswordHash func() *string // nil until the vault has answered; "" = none
	ExposedOrder func() []string
	Allowed      func() map[string]bool
	AllowedHosts []string
	Sessions     *SessionStore
	Limiter      *LoginLimiter
}

var storeMessages = map[string]string{
	"invalid_slug": "Slugs are 2–32 characters: lowercase letters, digits and hyphens, not starting with a hyphen.",
	"invalid_name": "The display name must be 1–80 characters.",
	"exists":       "That already exists. Archived agents keep their slug forever.",
	"reserved":     "The default agent cannot be archived or confined.",
	"not_found":    "Not found.",
	"has_scopes":   "Remove every folder first.",
}

var storeActionMessages = map[string]string{
	"scope.add:exists": "That folder is already in this agent's scope.",
}

func readCookie(header, name string) string {
	for _, part := range strings.Split(header, ";") {
		if eq := strings.Index(part, "="); eq > 0 && strings.TrimSpace(part[:eq]) == name {
			return strings.TrimSpace(part[eq+1:])
		}
	}
	return ""
}

var folderURLRE = regexp.MustCompile(`/folders/([A-Za-z0-9_-]+)`)
var folderIDRE = regexp.MustCompile(`[?&]id=([A-Za-z0-9_-]+)`)

// ParseFolderInput: a folder ID, or the ID inside a Drive folder URL.
func ParseFolderInput(input string) *string {
	value := strings.TrimSpace(input)
	id := value
	if m := folderURLRE.FindStringSubmatch(value); m != nil {
		id = m[1]
	} else if m := folderIDRE.FindStringSubmatch(value); m != nil {
		id = m[1]
	}
	if enforce.IDRE.MatchString(id) {
		return &id
	}
	return nil
}

type adminApp struct {
	d        *Deps
	sessions *SessionStore
	limiter  *LoginLimiter
}

// Handler builds the dashboard app.
func (d *Deps) Handler() http.Handler {
	sessions := d.Sessions
	if sessions == nil {
		sessions = NewSessionStore()
	}
	limiter := d.Limiter
	if limiter == nil {
		limiter = NewLoginLimiter()
	}
	a := &adminApp{d: d, sessions: sessions, limiter: limiter}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			sessions.Sweep()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", a.getLogin)
	mux.HandleFunc("POST /login", a.postLogin)
	mux.HandleFunc("POST /logout", a.withSession(a.postLogout))
	mux.HandleFunc("GET /{$}", a.withSession(a.getAgents))
	mux.HandleFunc("POST /agents", a.withSession(a.postAgents))
	mux.HandleFunc("GET /agents/{slug}", a.withSession(a.getAgent))
	mux.HandleFunc("POST /agents/{slug}/grants", a.withSession(a.postGrants))
	mux.HandleFunc("POST /agents/{slug}/archive", a.withSession(a.postArchive))
	mux.HandleFunc("POST /agents/{slug}/unconfine", a.withSession(a.postUnconfine))
	mux.HandleFunc("POST /agents/{slug}/scopes", a.withSession(a.postScopes))
	mux.HandleFunc("POST /agents/{slug}/scopes/{folderId}/remove", a.withSession(a.postScopeRemove))
	mux.HandleFunc("POST /agents/{slug}/scopes/{folderId}/primary", a.withSession(a.postScopePrimary))
	mux.HandleFunc("GET /agents/{slug}/browse", a.withSession(a.getBrowse))
	mux.HandleFunc("POST /agents/{slug}/tokens", a.withSession(a.postTokens))
	mux.HandleFunc("POST /tokens/{id}/revoke", a.withSession(a.postTokenRevoke))
	mux.HandleFunc("GET /audit", a.withSession(a.getAudit))
	mux.HandleFunc("POST /cache/flush", a.withSession(a.postCacheFlush))
	mux.HandleFunc("/", a.notFound)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			// Anything a route did not handle: the dashboard's own escaped
			// page, never a stack trace.
			if rec := recover(); rec != nil {
				fmt.Printf("[admin] unhandled error: %v\n", rec)
				a.page(w, 500, errorPage(500, "Something went wrong. The error is in the service log."))
			}
		}()
		// 1. Who may connect at all. The socket, never a header.
		source := r.RemoteAddr
		if i := strings.LastIndex(source, ":"); i > 0 && !strings.HasSuffix(source, "]") {
			source = source[:i]
		}
		if !IsAllowed(source, d.Allowed()) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(403)
			fmt.Fprint(w, "forbidden")
			return
		}
		// 1b. And only under a loopback name: see IsAllowedHost.
		if !IsAllowedHost(r.Host, d.AllowedHosts...) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(403)
			fmt.Fprint(w, "forbidden host")
			return
		}
		// 2. Headers. No script may run on these pages, from anywhere.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		// express.urlencoded({limit:"64kb"}): cap form bodies the same way.
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		mux.ServeHTTP(w, r)
	})
}

func (a *adminApp) page(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

func (a *adminApp) source(r *http.Request) string {
	source := r.RemoteAddr
	if i := strings.LastIndex(source, ":"); i > 0 && !strings.HasSuffix(source, "]") {
		source = source[:i]
	}
	return source
}

func (a *adminApp) dbUnavailable(w http.ResponseWriter) {
	msg := a.d.DBError()
	if msg == "" {
		msg = "unknown error"
	}
	a.page(w, 503, errorPage(503, "The policy database is unavailable: "+msg))
}

func (a *adminApp) notFound(w http.ResponseWriter, _ *http.Request) {
	a.page(w, 404, errorPage(404, "Not found."))
}

// --- login -----------------------------------------------------------------

func (a *adminApp) loginStateOf(r *http.Request) loginState {
	hash := a.d.PasswordHash()
	if hash == nil {
		return loginStarting
	}
	if *hash == "" {
		return loginUnconfigured
	}
	if !UsableHash(*hash) {
		return loginMisconfigured
	}
	if a.limiter.Locked(a.source(r)) {
		return loginLocked
	}
	return loginForm
}

func (a *adminApp) getLogin(w http.ResponseWriter, r *http.Request) {
	if a.d.Store() == nil {
		a.dbUnavailable(w)
		return
	}
	a.page(w, 200, loginPage(a.loginStateOf(r), ""))
}

func (a *adminApp) postLogin(w http.ResponseWriter, r *http.Request) {
	if a.d.Store() == nil {
		a.page(w, 503, errorPage(503, "The policy database is unavailable."))
		return
	}
	state := a.loginStateOf(r)
	if state == loginLocked {
		a.page(w, 429, loginPage(loginLocked, ""))
		return
	}
	if state != loginForm {
		a.page(w, 503, loginPage(state, ""))
		return
	}
	if !a.limiter.Acquire() {
		a.page(w, 429, loginPage(loginForm, "Another login is being checked. Try again."))
		return
	}
	var ok bool
	func() {
		defer a.limiter.Release()
		_ = r.ParseForm()
		password := r.PostFormValue("password")
		ok = len(password) > 0 && len(password) <= 1024 && VerifyPassword(password, *a.d.PasswordHash())
	}()
	source := a.source(r)
	if !ok {
		if au := a.d.Audit(); au != nil {
			au.AdminDeny("login", source)
		}
		if a.limiter.Fail(source) {
			if au := a.d.Audit(); au != nil {
				au.AdminDeny("login_lockout", source)
			}
		}
		state := loginForm
		if a.limiter.Locked(source) {
			state = loginLocked
		}
		a.page(w, 401, loginPage(state, "Wrong password."))
		return
	}
	a.limiter.Success(source)
	session := a.sessions.Create()
	if au := a.d.Audit(); au != nil {
		au.Admin("login", nil, nil, source)
	}
	secure := ""
	if r.TLS != nil {
		secure = "; Secure"
	}
	w.Header().Set("Set-Cookie", Cookie+"="+session.ID+"; HttpOnly; SameSite=Strict; Path=/"+secure)
	http.Redirect(w, r, "/", 303)
}

// --- everything else needs a session (and a store) -------------------------

type reqCtx struct {
	session *Session
	store   *policy.PolicyStore
}

func (a *adminApp) withSession(next func(w http.ResponseWriter, r *http.Request, c reqCtx)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store := a.d.Store()
		if store == nil {
			a.dbUnavailable(w)
			return
		}
		session := a.sessions.Get(readCookie(r.Header.Get("Cookie"), Cookie))
		if session == nil {
			http.Redirect(w, r, "/login", 303)
			return
		}
		if r.Method == "POST" {
			_ = r.ParseForm()
			token := r.PostFormValue("_csrf")
			if !policy.SafeEqual(token, session.CSRF) {
				if au := a.d.Audit(); au != nil {
					au.AdminDeny("csrf", a.source(r))
				}
				a.page(w, 403, errorPage(403, "This form has expired. Go back, reload and try again."))
				return
			}
		}
		next(w, r, reqCtx{session, store})
	}
}

func (a *adminApp) done(w http.ResponseWriter, r *http.Request, c reqCtx, action string, resources []string, to string, agent *string) {
	if au := a.d.Audit(); au != nil {
		au.Admin(action, agent, resources, a.source(r))
	}
	http.Redirect(w, r, to, 303)
}

// attempt: a StoreError becomes a flash message.
func (a *adminApp) attempt(session *Session, action string, fn func() error) bool {
	if err := fn(); err != nil {
		var se *policy.StoreError
		if errors.As(err, &se) {
			msg := storeActionMessages[action+":"+se.Code]
			if msg == "" {
				msg = storeMessages[se.Code]
			}
			session.Flash = append(session.Flash, msg)
			return false
		}
		panic(err)
	}
	return true
}

func (a *adminApp) toolsOf(store *policy.PolicyStore, slug string, scoped bool) []string {
	grants, _ := store.GetGrants(slug)
	return policy.EnabledTools(grants, scoped, a.d.ExposedOrder(), map[string]bool{}, map[string]bool{})
}

func (a *adminApp) requireAgent(w http.ResponseWriter, r *http.Request, c reqCtx) *policy.AgentRow {
	agent, err := c.store.GetAgent(r.PathValue("slug"))
	if err != nil {
		panic(err)
	}
	if agent == nil {
		a.page(w, 404, errorPage(404, "No such agent."))
		return nil
	}
	return agent
}

// requireLiveAgent: an archived agent is a record, not something to edit.
func (a *adminApp) requireLiveAgent(w http.ResponseWriter, r *http.Request, c reqCtx) *policy.AgentRow {
	agent := a.requireAgent(w, r, c)
	if agent != nil && agent.ArchivedAt != nil {
		a.page(w, 409, errorPage(409, "This agent is archived and can no longer be changed."))
		return nil
	}
	return agent
}
