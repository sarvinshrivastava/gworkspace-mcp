// gworkspace-mcp — the Google Workspace MCP relay on Streamable HTTP, one
// policy-scoped identity per agent. Go-only: no Node child, the
// tool handlers run in-process.
//
// Boot wiring only, mirroring src/index.ts:
//  1. open the policy DB (before any listener, so no request sees it half-open)
//  2. listen on :3014 (MCP) and :3015 (admin)
//  3. read the vault, build the Drive client, run the boot probe, mark ready
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/admin"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/audit"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/config"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/enforce"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/gateway"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/housekeeping"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/notify"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/policy"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/tools"
)

func main() {
	if code := healthcheckExit(); code != nil {
		os.Exit(*code)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	ctx := context.Background()

	// --- 1. the policy DB, or degraded mode ---------------------------------
	//
	// A database that EXISTED may hold restrictions, so failing to read it
	// stops the service rather than serving without them. A database that
	// never existed and cannot be created restricts nothing: the legacy
	// bearer gets exactly what a first boot would have seeded.
	var store *policy.PolicyStore
	var dbError string
	if s, err := policy.OpenPolicyStore(cfg.DBPath); err != nil {
		var oe *policy.OpenError
		if ok := errorAs(err, &oe); ok && !oe.Existed {
			dbError = err.Error()
			fmt.Printf("[db] DEGRADED: cannot create the policy database at %s (%v). Only the legacy bearer is served, with the first-boot tool set; minted tokens are refused; the dashboard is unavailable.\n", cfg.DBPath, err)
		} else {
			return fmt.Errorf("FATAL: policy DB exists but cannot be opened at %s: %w. Refusing to start without the restrictions it holds. Restore it from %s or fix its permissions.", cfg.DBPath, err, cfg.BackupDir)
		}
	} else {
		store = s
		if created, err := store.EnsureDefaultAgent(policy.DefaultGrants()); err != nil {
			return fmt.Errorf("seed default agent: %w", err)
		} else if created {
			fmt.Println("[db] created the default agent for the legacy bearer, with every mapped tool")
		}
	}

	var aud *audit.Audit
	if store != nil {
		aud = audit.New(store)
	}

	var hk *housekeeping.Runner
	if store != nil {
		hk = housekeeping.NewRunner(housekeeping.Deps{
			Audit:     aud,
			DBFile:    cfg.DBPath,
			BackupDir: cfg.BackupDir,
		})
		hk.Start(cfg.HousekeepingScheduler)
	}

	// --- state shared by both listeners --------------------------------------
	var (
		legacyToken  string
		passwordHash *string // nil until the vault has answered
		plrToken     string
		driveClient  *drive.Client
		ready        bool
	)
	googleHealth := drive.NewHealth()

	resolver := enforce.NewScopeResolver(func() *drive.Client { return driveClient })
	subtree := enforce.NewSubtreeCache(func() *drive.Client { return driveClient })

	schemas, err := gateway.LoadSchemas(cfg.ToolAllowlist)
	if err != nil {
		return err
	}

	authenticator := func() func(string) (*policy.AgentContext, error) {
		if store != nil {
			return policy.MakeAuthenticator(store, func() string { return legacyToken })
		}
		degraded := policy.MakeDegradedAuthenticator(func() string { return legacyToken })
		return func(bearer string) (*policy.AgentContext, error) { return degraded(bearer), nil }
	}

	// The tool registry: all handler groups merged. searchDriveFiles is
	// served by the pipeline for CONFINED agents; the registry entry covers
	// the legacy relay path (the TS child's own shortcut).
	registry := map[string]tools.Handler{
		"listDriveFiles":   tools.ListDriveFiles,
		"searchDriveFiles": tools.SearchDriveFiles,
	}
	for name, h := range tools.SheetsHandlers() {
		registry[name] = h
	}
	for name, h := range tools.DriveHandlers() {
		registry[name] = h
	}
	for name, h := range tools.CalendarHandlers() {
		registry[name] = h
	}
	for name, h := range tools.DocsHandlers() {
		registry[name] = h
	}

	googleTools := &tools.Google{Timeout: 30 * time.Second}

	mcpDeps := &gateway.Deps{
		Authenticate: authenticator,
		Audit:        func() *audit.Audit { return aud },
		Pipeline: enforce.PipelineDeps{
			Resolver: resolver,
			Subtree:  subtree,
			Drive:    func() *drive.Client { return driveClient },
		},
		Registry:    registry,
		GoogleTools: googleTools,
		Ready:       func() bool { return ready },
		DBUp:        func() bool { return store != nil },
		AuditErrors: func() int64 {
			if aud != nil {
				return aud.Errors.Load()
			}
			return 0
		},
		Housekeeping: func() *housekeeping.Runner { return hk },
		PlrToken:     func() string { return plrToken },
		Google:       googleHealth,
		Schemas:      schemas,
	}

	adminDeps := &admin.Deps{
		Store:        func() *policy.PolicyStore { return store },
		DBError:      func() string { return dbError },
		Audit:        func() *audit.Audit { return aud },
		Drive:        func() *drive.Client { return driveClient },
		Resolver:     resolver,
		Subtree:      subtree,
		PasswordHash: func() *string { return passwordHash },
		ExposedOrder: func() []string {
			out := make([]string, len(schemas))
			for i, t := range schemas {
				out[i] = t.Name
			}
			return out
		},
		Allowed:      admin.MakeAllowedSources(cfg.AdminAllowedSources, admin.DefaultNet()),
		AllowedHosts: cfg.AdminAllowedHosts,
	}

	// --- 2. listen ------------------------------------------------------------
	mcpSrv := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Port), Handler: mcpDeps.Handler()}
	// 0.0.0.0 so the 127.0.0.1-only host publish can reach it; the app's
	// source allowlist is what keeps other containers out.
	adminSrv := &http.Server{Addr: fmt.Sprintf(":%d", cfg.AdminPort), Handler: adminDeps.Handler()}
	errCh := make(chan error, 2)
	go func() {
		fmt.Printf("gworkspace-mcp listening on :%d\n", cfg.Port)
		if err := mcpSrv.ListenAndServe(); err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	go func() {
		fmt.Printf("admin dashboard listening on :%d\n", cfg.AdminPort)
		if err := adminSrv.ListenAndServe(); err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// --- 3. the vault, the Drive client, the probe ----------------------------
	// Sequential fetches against a slow vault would outlast the deploy's
	// health wait; these run concurrently.
	type vaultResult struct {
		key   string
		value string
		err   error
	}
	keys := []struct {
		key      string
		optional bool
	}{
		{"GWORKSPACE_MCP_TOKEN", false},
		{"GOOGLE_CLIENT_ID", false},
		{"GOOGLE_CLIENT_SECRET", false},
		{"GOOGLE_TOKEN_JSON", false},
		{"DASHBOARD_ADMIN_PASSWORD_HASH", true},
		{"GWORKSPACE_PLR_TOKEN", true},
		{"GWORKSPACE_NTFY_TOPIC", true},
	}
	results := make(chan vaultResult, len(keys))
	for _, k := range keys {
		go func(k struct {
			key      string
			optional bool
		}) {
			v, err := cfg.VaultFetch(ctx, k.key, k.optional)
			results <- vaultResult{k.key, v, err}
		}(k)
	}
	vault := map[string]string{}
	for range keys {
		r := <-results
		if r.err != nil {
			return fmt.Errorf("vault: %w", r.err)
		}
		vault[r.key] = r.value
	}
	legacyToken = vault["GWORKSPACE_MCP_TOKEN"]
	if h, ok := vault["DASHBOARD_ADMIN_PASSWORD_HASH"]; ok {
		passwordHash = &h
	} else {
		empty := ""
		passwordHash = &empty
		fmt.Println("[admin] no DASHBOARD_ADMIN_PASSWORD_HASH in the vault — dashboard login is disabled")
	}
	plrToken = gateway.UsablePlrToken(vault["GWORKSPACE_PLR_TOKEN"])
	if vault["GWORKSPACE_PLR_TOKEN"] != "" && plrToken == "" {
		fmt.Println("[internal] GWORKSPACE_PLR_TOKEN is shorter than 32 characters after trimming whitespace — ignored; /internal/* answers 503 until it is replaced")
	}
	if plrToken == "" && cfg.HousekeepingScheduler == "external" {
		fmt.Println("[db] HOUSEKEEPING_SCHEDULER=external but no usable GWORKSPACE_PLR_TOKEN: housekeeping runs at boot only until the key is set and the service restarted")
	}

	// Nothing reads the vault after boot. This removes the token from this
	// process's view; /proc/<pid>/environ keeps the original block (spec D8
	// tracks bounding that).
	os.Unsetenv("SECRETS_MANAGER_TOKEN")
	cfg.Vault.Token = ""

	// The Drive client, same credential the TS child used; it never leaves
	// this process. The health state machine's broadcaster pushes broken-only
	// notices to the shared ntfy topic.
	notifier := notify.NewNotifier(vault["GWORKSPACE_NTFY_TOPIC"], cfg.NtfyBase, 10*time.Second)
	googleHealth.SetBroadcast(notify.MakeBroadcaster(notifier))
	normalised, err := normaliseToken(vault["GOOGLE_TOKEN_JSON"])
	if err != nil {
		return err
	}
	driveClient = drive.NewClient(drive.Creds{
		ClientID:     vault["GOOGLE_CLIENT_ID"],
		ClientSecret: vault["GOOGLE_CLIENT_SECRET"],
		RefreshToken: refreshTokenOf(normalised),
	}, 30*time.Second, googleHealth.Ok)
	googleTools.Tok = driveClient

	// Boot probe: a revoked or expired grant boots perfectly and then fails
	// every user action. One cheap call turns that into a line in the log at
	// the moment it becomes true. Deliberately not fatal.
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, probeErr := driveClient.ListFiles(probeCtx, drive.ListParams{Q: "trashed=false", PageSize: 1, Fields: "files(id)"})
	cancel()
	googleHealth.Probe(probeResultOf(probeErr))

	ready = true
	names := make([]string, len(schemas))
	for i, t := range schemas {
		names[i] = t.Name
	}
	fmt.Printf("exposing: %s\n", joinOr(names, ", ", "(none)"))

	// --- signals: closing the database checkpoints the WAL -------------------
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	select {
	case err := <-errCh:
		return err
	case <-sigCh:
	}
	if store != nil {
		store.Close()
	}
	return nil
}

func errorAs(err error, target **policy.OpenError) bool {
	for err != nil {
		if oe, ok := err.(*policy.OpenError); ok {
			*target = oe
			return true
		}
		if u, ok := err.(interface{ Unwrap() error }); ok {
			err = u.Unwrap()
			continue
		}
		break
	}
	return false
}

func joinOr(items []string, sep, empty string) string {
	if len(items) == 0 {
		return empty
	}
	out := items[0]
	for _, s := range items[1:] {
		out += sep + s
	}
	return out
}
