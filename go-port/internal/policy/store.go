// The policy database: agents, their grants, folder scopes and tokens, and the
// audit log. Every SQL statement in the service lives in this file.
//
// Port of src/policy/store.ts (pinned commit e8d3d40) onto database/sql with
// modernc.org/sqlite (pure Go, no CGO — keeps the scratch-based minimal image).
package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var SlugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

const DefaultAgent = "default"

// MaxResources is the number of resource IDs kept per audit row.
const MaxResources = 50

// StoreError codes match the TS StoreError.code strings byte-for-byte.
type StoreError struct{ Code string }

func (e *StoreError) Error() string { return e.Code }

var (
	ErrInvalidSlug = &StoreError{"invalid_slug"}
	ErrInvalidName = &StoreError{"invalid_name"}
	ErrExists      = &StoreError{"exists"}
	ErrReserved    = &StoreError{"reserved"}
	ErrNotFound    = &StoreError{"not_found"}
	ErrHasScopes   = &StoreError{"has_scopes"}
)

// OpenError mirrors the TS OpenError: Existed decides degraded vs refuse.
type OpenError struct {
	Existed bool
	Cause   error
}

func (e *OpenError) Error() string { return e.Cause.Error() }
func (e *OpenError) Unwrap() error { return e.Cause }

type Scope struct {
	FolderID   string `json:"folderId"`
	FolderName string `json:"folderName"`
	IsPrimary  bool   `json:"isPrimary"`
}

type AgentRow struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Scoped     bool   `json:"scoped"`
	CreatedAt  int64  `json:"createdAt"`
	ArchivedAt *int64 `json:"archivedAt"`
}

type TokenRow struct {
	ID        int64  `json:"id"`
	Agent     string `json:"agent"`
	Prefix    string `json:"prefix"`
	Label     string `json:"label"`
	CreatedAt int64  `json:"createdAt"`
	RevokedAt *int64 `json:"revokedAt"`
}

type AuditRow struct {
	ID        int64    `json:"id"`
	Ts        int64    `json:"ts"`
	Kind      string   `json:"kind"`
	Agent     *string  `json:"agent"`
	Tool      *string  `json:"tool"`
	Decision  string   `json:"decision"`
	Reason    string   `json:"reason"`
	Resources []string `json:"resources"`
	Source    string   `json:"source"`
}

// DatabaseExists decides what a failed open means: a database that existed may
// hold restrictions, so failing to read it must stop the service rather than
// quietly run without them. Anything other than "no such file" counts as
// existing. A 0-byte file with no WAL holds nothing.
func DatabaseExists(file string) bool {
	size := func(p string) (int64, bool) {
		st, err := os.Stat(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return 0, false
			}
			return -1, true // any other error: treat as existing (strict reading)
		}
		return st.Size(), true
	}
	mainSize, mainOK := size(file)
	walSize, walOK := size(file + "-wal")
	if walOK && (walSize > 0 || walSize == -1) {
		return true
	}
	if !mainOK {
		return false
	}
	return mainSize > 0 || mainSize == -1
}

type PolicyStore struct {
	db            *sql.DB
	migrations    []string
	now           func() int64
	SchemaVersion int
}

// OpenPolicyStore opens (or creates) the store, matching openPolicyStore.
func OpenPolicyStore(file string) (*PolicyStore, error) {
	existed := DatabaseExists(file)
	db, err := sql.Open("sqlite", file+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, &OpenError{Existed: existed, Cause: err}
	}
	// One connection: database/sql pools, and WAL + in-process only means a
	// second connection buys contention, not parallelism. (node:sqlite is
	// single-handle too.)
	db.SetMaxOpenConns(1)
	store, err := newPolicyStore(db, Migrations, nil)
	if err != nil {
		db.Close()
		return nil, &OpenError{Existed: existed, Cause: err}
	}
	return store, nil
}

func newPolicyStore(db *sql.DB, migrations []string, now func() int64) (*PolicyStore, error) {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	s := &PolicyStore{db: db, migrations: migrations, now: now}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	v, err := s.userVersion()
	if err != nil {
		return nil, err
	}
	s.SchemaVersion = v
	return s, nil
}

func (s *PolicyStore) userVersion() (int, error) {
	var v int
	err := s.db.QueryRow("PRAGMA user_version").Scan(&v)
	return v, err
}

// tx runs fn in one BEGIN IMMEDIATE transaction (via _txlock=immediate on the
// DSN), rolling back and rethrowing on failure — the TS tx() semantics.
func (s *PolicyStore) tx(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *PolicyStore) migrate() error {
	current, err := s.userVersion()
	if err != nil {
		return err
	}
	if current > len(s.migrations) {
		// A newer image migrated this database and we are its rollback.
		fmt.Printf("[db] schema version %d is newer than this build knows (%d) — running without migrating\n", current, len(s.migrations))
		return nil
	}
	for version := current; version < len(s.migrations); version++ {
		if err := s.tx(func(tx *sql.Tx) error {
			if _, err := tx.Exec(s.migrations[version]); err != nil {
				return err
			}
			_, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version+1))
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// EnsureDefaultAgent creates the `default` agent — the one the legacy bearer
// maps to — only if it has never existed. Never re-seeds.
func (s *PolicyStore) EnsureDefaultAgent(seed []Grant) (bool, error) {
	inserted := false
	err := s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			"INSERT OR IGNORE INTO agents (slug, name, scoped, created_at) VALUES (?, ?, 0, ?)",
			DefaultAgent, "Default (legacy bearer)", s.now())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		for _, g := range seed {
			if _, err := tx.Exec("INSERT INTO grants (agent, area, op) VALUES (?, ?, ?)", DefaultAgent, g.Area, g.Op); err != nil {
				return err
			}
		}
		inserted = true
		return nil
	})
	return inserted, err
}

func scanAgent(row interface{ Scan(...any) error }) (*AgentRow, error) {
	var a AgentRow
	var scoped int
	var archived sql.NullInt64
	err := row.Scan(&a.Slug, &a.Name, &scoped, &a.CreatedAt, &archived)
	if err != nil {
		return nil, err
	}
	a.Scoped = scoped == 1
	if archived.Valid {
		a.ArchivedAt = &archived.Int64
	}
	return &a, nil
}

func (s *PolicyStore) ListAgents(includeArchived bool) ([]AgentRow, error) {
	query := "SELECT * FROM agents WHERE archived_at IS NULL ORDER BY created_at, slug"
	if includeArchived {
		query = "SELECT * FROM agents ORDER BY created_at, slug"
	}
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRow
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *PolicyStore) GetAgent(slug string) (*AgentRow, error) {
	a, err := scanAgent(s.db.QueryRow("SELECT * FROM agents WHERE slug = ?", slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func (s *PolicyStore) requireAgent(slug string) (*AgentRow, error) {
	agent, err := s.GetAgent(slug)
	if err != nil {
		return nil, err
	}
	if agent == nil {
		return nil, ErrNotFound
	}
	return agent, nil
}

// CreateAgent: new agents are confined — no Drive access until a folder is assigned.
func (s *PolicyStore) CreateAgent(slug, name string) error {
	if !SlugRE.MatchString(slug) {
		return ErrInvalidSlug
	}
	trimmed := strings.TrimSpace(name)
	// JS .length counts UTF-16 code units, not bytes.
	if trimmed == "" || utf16Len(trimmed) > 80 {
		return ErrInvalidName
	}
	// The exists check and the insert share one immediate transaction: under
	// Go concurrency a check-then-act outside it can race a twin request into
	// a raw SQLite constraint error instead of ErrExists.
	return s.tx(func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRow("SELECT 1 FROM agents WHERE slug = ?", slug).Scan(&one)
		if err == nil {
			// Archived slugs stay reserved: a new agent must never inherit an
			// old one's tokens, grants or scopes through a reused key.
			return ErrExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.Exec("INSERT INTO agents (slug, name, scoped, created_at) VALUES (?, ?, 1, ?)", slug, trimmed, s.now())
		return err
	})
}

func (s *PolicyStore) ArchiveAgent(slug string) error {
	if slug == DefaultAgent {
		return ErrReserved
	}
	at := s.now()
	return s.tx(func(tx *sql.Tx) error {
		if err := requireAgentTx(tx, slug); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE agents SET archived_at = ? WHERE slug = ?", at, slug); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE tokens SET revoked_at = ? WHERE agent = ? AND revoked_at IS NULL", at, slug)
		return err
	})
}

// SetUnconfined lifts confinement — only with no folders left, so it is never accidental.
func (s *PolicyStore) SetUnconfined(slug string) error {
	return s.tx(func(tx *sql.Tx) error {
		if err := requireAgentTx(tx, slug); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM folder_scopes WHERE agent = ?", slug).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return ErrHasScopes
		}
		_, err := tx.Exec("UPDATE agents SET scoped = 0 WHERE slug = ?", slug)
		return err
	})
}

func (s *PolicyStore) GetGrants(slug string) ([]Grant, error) {
	rows, err := s.db.Query("SELECT area, op FROM grants WHERE agent = ? ORDER BY area, op", slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.Area, &g.Op); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *PolicyStore) SetGrants(slug string, grants []Grant) error {
	return s.tx(func(tx *sql.Tx) error {
		if err := requireAgentTx(tx, slug); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM grants WHERE agent = ?", slug); err != nil {
			return err
		}
		for _, g := range grants {
			if _, err := tx.Exec("INSERT OR IGNORE INTO grants (agent, area, op) VALUES (?, ?, ?)", slug, g.Area, g.Op); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PolicyStore) GetScopes(slug string) ([]Scope, error) {
	rows, err := s.db.Query(
		"SELECT folder_id, folder_name, is_primary FROM folder_scopes WHERE agent = ? ORDER BY is_primary DESC, created_at, folder_id", slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Scope{}
	for rows.Next() {
		var sc Scope
		var primary int
		if err := rows.Scan(&sc.FolderID, &sc.FolderName, &primary); err != nil {
			return nil, err
		}
		sc.IsPrimary = primary == 1
		out = append(out, sc)
	}
	return out, rows.Err()
}

func (s *PolicyStore) hasScope(slug, folderID string) (bool, error) {
	var one int
	err := s.db.QueryRow("SELECT 1 FROM folder_scopes WHERE agent = ? AND folder_id = ?", slug, folderID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// requireAgentTx is requireAgent inside the caller's serialized transaction.
func requireAgentTx(tx *sql.Tx, slug string) error {
	var one int
	err := tx.QueryRow("SELECT 1 FROM agents WHERE slug = ?", slug).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// AddScope adds a folder to an agent's scope and confines the agent. The first
// folder is the primary — where creates land — whatever was asked.
func (s *PolicyStore) AddScope(slug, folderID, folderName string, primary bool) error {
	// `default` is the legacy connection's identity; confining it would change
	// what Open WebUI sees on a connection nobody reconfigured.
	if slug == DefaultAgent {
		return ErrReserved
	}
	return s.tx(func(tx *sql.Tx) error {
		if err := requireAgentTx(tx, slug); err != nil {
			return err
		}
		var one int
		err := tx.QueryRow("SELECT 1 FROM folder_scopes WHERE agent = ? AND folder_id = ?", slug, folderID).Scan(&one)
		if err == nil {
			return ErrExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM folder_scopes WHERE agent = ?", slug).Scan(&count); err != nil {
			return err
		}
		first := count == 0
		makePrimary := primary || first
		if makePrimary {
			if _, err := tx.Exec("UPDATE folder_scopes SET is_primary = 0 WHERE agent = ?", slug); err != nil {
				return err
			}
		}
		primaryInt := 0
		if makePrimary {
			primaryInt = 1
		}
		if _, err := tx.Exec(
			"INSERT INTO folder_scopes (agent, folder_id, folder_name, is_primary, created_at) VALUES (?, ?, ?, ?, ?)",
			slug, folderID, folderName, primaryInt, s.now()); err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE agents SET scoped = 1 WHERE slug = ?", slug)
		return err
	})
}

// RemoveScope: removing the last folder leaves the agent confined, with no
// Drive access. Removing the primary promotes the oldest remaining folder.
func (s *PolicyStore) RemoveScope(slug, folderID string) error {
	return s.tx(func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRow("SELECT 1 FROM folder_scopes WHERE agent = ? AND folder_id = ?", slug, folderID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM folder_scopes WHERE agent = ? AND folder_id = ?", slug, folderID); err != nil {
			return err
		}
		rows, err := tx.Query(
			"SELECT folder_id, is_primary FROM folder_scopes WHERE agent = ? ORDER BY is_primary DESC, created_at, folder_id", slug)
		if err != nil {
			return err
		}
		type rem struct {
			id      string
			primary bool
		}
		var remaining []rem
		for rows.Next() {
			var r rem
			var p int
			if err := rows.Scan(&r.id, &p); err != nil {
				rows.Close()
				return err
			}
			r.primary = p == 1
			remaining = append(remaining, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(remaining) > 0 && !remaining[0].primary {
			if _, err := tx.Exec("UPDATE folder_scopes SET is_primary = 1 WHERE agent = ? AND folder_id = ?", slug, remaining[0].id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PolicyStore) SetPrimary(slug, folderID string) error {
	return s.tx(func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRow("SELECT 1 FROM folder_scopes WHERE agent = ? AND folder_id = ?", slug, folderID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE folder_scopes SET is_primary = 0 WHERE agent = ?", slug); err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE folder_scopes SET is_primary = 1 WHERE agent = ? AND folder_id = ?", slug, folderID)
		return err
	})
}

func (s *PolicyStore) InsertToken(slug, hash, prefix, label string) (int64, error) {
	l := strings.TrimSpace(label)
	// JS .slice(0, 80) counts UTF-16 code units, not bytes.
	l = utf16Slice(l, 80)
	var id int64
	err := s.tx(func(tx *sql.Tx) error {
		if err := requireAgentTx(tx, slug); err != nil {
			return err
		}
		res, err := tx.Exec(
			"INSERT INTO tokens (agent, token_hash, prefix, label, created_at) VALUES (?, ?, ?, ?, ?)",
			slug, hash, prefix, l, s.now())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

func (s *PolicyStore) RevokeToken(id int64) error {
	_, err := s.db.Exec("UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", s.now(), id)
	return err
}

func (s *PolicyStore) GetToken(id int64) (*TokenRow, error) {
	var t TokenRow
	var hash string
	var revoked sql.NullInt64
	err := s.db.QueryRow("SELECT * FROM tokens WHERE id = ?", id).
		Scan(&t.ID, &t.Agent, &hash, &t.Prefix, &t.Label, &t.CreatedAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if revoked.Valid {
		t.RevokedAt = &revoked.Int64
	}
	return &t, nil
}

func (s *PolicyStore) ListTokens(slug string) ([]TokenRow, error) {
	rows, err := s.db.Query("SELECT * FROM tokens WHERE agent = ? ORDER BY created_at DESC, id DESC", slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenRow{}
	for rows.Next() {
		var t TokenRow
		var hash string
		var revoked sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Agent, &hash, &t.Prefix, &t.Label, &t.CreatedAt, &revoked); err != nil {
			return nil, err
		}
		if revoked.Valid {
			t.RevokedAt = &revoked.Int64
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PolicyStore) AgentForTokenHash(hash string) (string, error) {
	var slug string
	err := s.db.QueryRow(
		"SELECT t.agent FROM tokens t JOIN agents a ON a.slug = t.agent WHERE t.token_hash = ? AND t.revoked_at IS NULL AND a.archived_at IS NULL",
		hash).Scan(&slug)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return slug, err
}

func (s *PolicyStore) InsertAudit(row AuditRow) error {
	resources := row.Resources
	if len(resources) > MaxResources {
		resources = resources[:MaxResources]
	}
	if resources == nil {
		resources = []string{}
	}
	text, err := json.Marshal(resources)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		"INSERT INTO audit_log (ts, kind, agent, tool, decision, reason, resources, source) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		row.Ts, row.Kind, row.Agent, row.Tool, row.Decision, row.Reason, string(text), row.Source)
	return err
}

type AuditFilter struct {
	Agent    string
	Decision string
	Limit    int
}

func (s *PolicyStore) QueryAudit(f AuditFilter) ([]AuditRow, error) {
	var where []string
	var params []any
	if f.Agent != "" {
		where = append(where, "agent = ?")
		params = append(params, f.Agent)
	}
	if f.Decision != "" {
		where = append(where, "decision = ?")
		params = append(params, f.Decision)
	}
	limit := f.Limit
	if limit < 1 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}
	query := "SELECT * FROM audit_log"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY ts DESC, id DESC LIMIT %d", limit)
	rows, err := s.db.Query(query, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditRow{}
	for rows.Next() {
		var r AuditRow
		var agent, tool sql.NullString
		var resourcesText string
		if err := rows.Scan(&r.ID, &r.Ts, &r.Kind, &agent, &tool, &r.Decision, &r.Reason, &resourcesText, &r.Source); err != nil {
			return nil, err
		}
		if agent.Valid {
			r.Agent = &agent.String
		}
		if tool.Valid {
			r.Tool = &tool.String
		}
		r.Resources = parseResources(resourcesText)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PolicyStore) PruneAudit(beforeTs int64) (int64, error) {
	res, err := s.db.Exec("DELETE FROM audit_log WHERE ts < ?", beforeTs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *PolicyStore) Close() error { return s.db.Close() }

func parseResources(text string) []string {
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return []string{}
	}
	arr, ok := value.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if v == nil {
			// JS String(null) === "null"; fmt.Sprint(nil) would give "<nil>".
			out = append(out, "null")
			continue
		}
		out = append(out, fmt.Sprint(v))
	}
	return out
}
