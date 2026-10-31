// Daily housekeeping: prune the audit log, then back up the policy DB.
// Idempotent — BackupDaily keeps a copy already written today (UTC).
// Port of src/housekeeping.ts + backupDaily from src/policy/store.ts.
package housekeeping

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/audit"
	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/policy"
)

type Result struct {
	Pruned int     `json:"pruned"`
	Backup *string `json:"backup"`
}

type Deps struct {
	Audit     *audit.Audit
	Store     *policy.PolicyStore
	BackupDir string
	DBFile    string // the on-disk path of the policy DB, for the file-copy backup
}

// Single flight: two overlapping runs would share one .tmp target. A caller
// that arrives during a run waits for that run's result.
type Runner struct {
	deps Deps
	mu   sync.Mutex
	done chan struct{}
	res  Result
	err  error
}

func NewRunner(deps Deps) *Runner { return &Runner{deps: deps} }

func (r *Runner) Run() (Result, error) {
	r.mu.Lock()
	if r.done != nil {
		wait := r.done
		r.mu.Unlock()
		<-wait
		return r.res, r.err
	}
	r.done = make(chan struct{})
	r.mu.Unlock()

	pruned := r.deps.Audit.Prune(90)
	backup, err := BackupDaily(r.deps.DBFile, r.deps.BackupDir, time.Now().UTC())

	r.mu.Lock()
	r.res = Result{Pruned: pruned, Backup: backup}
	r.err = err
	close(r.done)
	r.done = nil
	r.mu.Unlock()
	return r.res, r.err
}

// BackupDaily: one backup per weekday, overwritten a week later. Written to a
// temporary file and renamed, so a backup that dies half way never replaces
// last week's good copy. A file already written today (UTC) is kept.
//
// The copy uses SQLite's VACUUM INTO on a second connection (busy_timeout 5s): a consistent
// online backup that needs no CGO backup API and works on a read_only rootfs
// as long as the backup dir is writable (it is: /state volume).
func BackupDaily(dbFile, dir string, now time.Time) (*string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	target := filepath.Join(dir, fmt.Sprintf("policy-%d.sqlite", int(now.Weekday())))
	if st, err := os.Stat(target); err == nil {
		if st.ModTime().UTC().Format("2006-01-02") == now.Format("2006-01-02") {
			return nil, nil
		}
	}
	temp := target + ".tmp"
	os.Remove(temp)
	db, err := sql.Open("sqlite", dbFile+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf("VACUUM INTO '%s'", temp)); err != nil {
		os.Remove(temp)
		return nil, err
	}
	if err := os.Rename(temp, target); err != nil {
		os.Remove(temp)
		return nil, err
	}
	return &target, nil
}

// Start: run once at boot, then every 24 h when scheduler is "internal".
func (r *Runner) Start(scheduler string) {
	once := func() {
		if _, err := r.Run(); err != nil {
			fmt.Printf("[db] housekeeping failed: %v\n", err)
		}
	}
	go once()
	if scheduler == "internal" {
		go func() {
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				once()
			}
		}()
	}
}
