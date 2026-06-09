package store_test

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// seedV25Worktrees creates the pre-v26 worktrees table (owner_kind/
// owner_id present, worktrees_owner_idx indexing them) at user_version
// 25 and inserts one fully-populated row. Reopening the path with
// store.Open then runs the v26 drop-ownership migration onward. The
// checkpoints table (created at v21 in a real DB) is seeded too so later
// migrations that ALTER it — e.g. v30 — have the table a v25 DB would have.
func seedV25Worktrees(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.Exec(`
		CREATE TABLE worktrees (
			id                          TEXT PRIMARY KEY,
			user_id                     TEXT NOT NULL,
			display_name                TEXT NOT NULL,
			owner_kind                  TEXT NOT NULL DEFAULT 'local',
			owner_id                    TEXT NOT NULL DEFAULT '',
			latest_synced_checkpoint    TEXT NOT NULL DEFAULT '',
			created_at                  DATETIME NOT NULL,
			updated_at                  DATETIME NOT NULL,
			origin_repo                 TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX worktrees_user_id_idx ON worktrees(user_id);
		CREATE INDEX worktrees_owner_idx   ON worktrees(owner_kind, owner_id);
		INSERT INTO worktrees
			(id, user_id, display_name, owner_kind, owner_id,
			 latest_synced_checkpoint, created_at, updated_at, origin_repo)
		VALUES
			('wt-keep', 'user-A', 'myrepo (main)', 'remote', 'sprite-9',
			 'ck-7', '` + now + `', '` + now + `', 'acme/api');

		CREATE TABLE checkpoints (
			id                  TEXT PRIMARY KEY,
			worktree_id         TEXT NOT NULL,
			head_commit         TEXT NOT NULL,
			head_ref            TEXT NOT NULL DEFAULT '',
			index_tree          TEXT NOT NULL,
			worktree_tree       TEXT NOT NULL,
			incremental_commit  TEXT NOT NULL,
			created_at          DATETIME NOT NULL,
			created_by          TEXT NOT NULL DEFAULT '',
			uploaded_at         DATETIME
		);
		CREATE INDEX checkpoints_worktree_idx ON checkpoints(worktree_id, created_at DESC);
		PRAGMA user_version = 25;
	`)
	if err != nil {
		t.Fatalf("seed v25 schema: %v", err)
	}
}

// TestMigrateV27_CreatesHeadBundles pins that opening a store runs the
// migration chain through v27 and creates the head_bundles table.
func TestMigrateV27_CreatesHeadBundles(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	_ = mustOpen(t, path) // runs migrations to the latest version

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen raw db: %v", err)
	}
	defer db.Close()

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version < 27 {
		t.Errorf("user_version = %d, want >= 27", version)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='head_bundles'`).Scan(&n); err != nil {
		t.Fatalf("probe head_bundles: %v", err)
	}
	if n != 1 {
		t.Error("head_bundles table missing after migrate")
	}
}

// TestMigrateV26_DropsOwnershipPreservesRows pins the v26 migration:
// dropping the indexed owner_kind/owner_id columns must remove them
// (and their index) while leaving every other column of existing rows
// intact. DROP COLUMN on an indexed column would fail without the
// migration's preceding DROP INDEX, so this also guards that ordering.
func TestMigrateV26_DropsOwnershipPreservesRows(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	seedV25Worktrees(t, path)

	// Reopen → runs the v26 migration.
	s := mustOpen(t, path)

	got, err := s.GetWorktreeByID(t.Context(), "wt-keep")
	if err != nil {
		t.Fatalf("GetWorktreeByID after migrate: %v", err)
	}
	if got.UserID != "user-A" || got.DisplayName != "myrepo (main)" {
		t.Fatalf("identity columns lost across migration: %+v", got)
	}
	if got.OriginRepo != "acme/api" || got.LatestSyncedCheckpoint != "ck-7" {
		t.Fatalf("non-owner columns lost across migration: %+v", got)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen raw db: %v", err)
	}
	defer db.Close()

	for _, col := range []string{"owner_kind", "owner_id"} {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('worktrees') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatalf("probe column %s: %v", col, err)
		}
		if n != 0 {
			t.Errorf("column %s should be dropped, still present", col)
		}
	}

	var idx int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='worktrees_owner_idx'`,
	).Scan(&idx); err != nil {
		t.Fatalf("probe index: %v", err)
	}
	if idx != 0 {
		t.Errorf("worktrees_owner_idx should be dropped, still present")
	}
}
