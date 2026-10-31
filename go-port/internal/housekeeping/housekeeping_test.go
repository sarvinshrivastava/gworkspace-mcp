package housekeeping

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/policy"
	_ "modernc.org/sqlite"
)

// Review gate: a VACUUM INTO backup must carry the migrated schema
// (user_version survives), restore cleanly through OpenPolicyStore with no
// migration error, and leave no tmp/WAL/SHM debris in the backup dir.
func TestBackupDaily_RestoresCleanly(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(dir, "policy.sqlite")
	store, err := policy.OpenPolicyStore(dbFile)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := store.CreateAgent("backup-test", "Backup Test"); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	backupDir := filepath.Join(dir, "backups")
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	target, err := BackupDaily(dbFile, backupDir, now)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if target == nil {
		t.Fatal("first backup of the day must produce a file")
	}

	// user_version survives the VACUUM INTO, at the migrated version.
	bdb, err := sql.Open("sqlite", "file:"+*target+"?mode=ro")
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	var backupVersion int
	if err := bdb.QueryRow("PRAGMA user_version").Scan(&backupVersion); err != nil {
		t.Fatalf("read backup user_version: %v", err)
	}
	if backupVersion != 1 {
		t.Fatalf("backup user_version = %d, want 1 (schema v1)", backupVersion)
	}
	var name string
	if err := bdb.QueryRow("SELECT name FROM agents WHERE slug = ?", "backup-test").Scan(&name); err != nil {
		t.Fatalf("agent missing from backup: %v", err)
	}
	if name != "Backup Test" {
		t.Fatalf("restored name = %q", name)
	}
	bdb.Close()

	// Fresh-open through the real store path: schema already current, so no
	// migration runs and the store is usable immediately.
	restored, err := policy.OpenPolicyStore(*target)
	if err != nil {
		t.Fatalf("restore open: %v", err)
	}
	got, err := restored.GetAgent("backup-test")
	if err != nil || got == nil {
		t.Fatalf("restored store cannot read agent: %v", err)
	}
	restored.Close()

	// No tmp/WAL/SHM debris in the backup directory.
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".tmp") || strings.HasSuffix(n, "-wal") || strings.HasSuffix(n, "-shm") {
			t.Fatalf("debris left in backup dir: %s", n)
		}
	}

	// Same-day rerun keeps the existing file (keep-today).
	again, err := BackupDaily(dbFile, backupDir, now)
	if err != nil {
		t.Fatalf("same-day rerun: %v", err)
	}
	if again != nil {
		t.Fatal("same-day rerun must keep the existing backup")
	}
}
