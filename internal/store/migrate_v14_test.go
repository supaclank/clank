package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/store"

	_ "modernc.org/sqlite"
)

// TestMigrationV14_DropsLegacyFTSAndColumns reproduces the production
// failure where running the v14 migration on a database carrying
// experimental FTS5 search infrastructure failed with:
//
//	migration v14: SQL logic error: error in trigger
//	sessions_fts_insert after drop column: no such column: new.project_name
//
// The legacy FTS triggers reference project_name; SQLite refuses
// ALTER TABLE DROP COLUMN while a trigger still references the column.
// The migration must tear the FTS objects down first.
func TestMigrationV14_DropsLegacyFTSAndColumns(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Seed a v13-shaped sessions table that mirrors the user's broken
	// database: project_name still present, FTS5 virtual table + triggers
	// referencing it.
	if _, err := db.Exec(`
		CREATE TABLE sessions (
			id              TEXT PRIMARY KEY,
			external_id     TEXT NOT NULL DEFAULT '',
			backend         TEXT NOT NULL,
			status          TEXT NOT NULL DEFAULT 'idle',
			visibility      TEXT NOT NULL DEFAULT '',
			follow_up       INTEGER NOT NULL DEFAULT 0,
			project_dir     TEXT NOT NULL DEFAULT '',
			project_name    TEXT NOT NULL DEFAULT '',
			prompt          TEXT NOT NULL DEFAULT '',
			title           TEXT NOT NULL DEFAULT '',
			ticket_id       TEXT NOT NULL DEFAULT '',
			agent           TEXT NOT NULL DEFAULT '',
			draft           TEXT NOT NULL DEFAULT '',
			created_at      DATETIME NOT NULL,
			updated_at      DATETIME NOT NULL,
			last_read_at    DATETIME,
			worktree_branch TEXT NOT NULL DEFAULT '',
			worktree_dir    TEXT NOT NULL DEFAULT '',
			host_id         TEXT NOT NULL DEFAULT 'local',
			git_ref_url     TEXT NOT NULL DEFAULT '',
			git_ref_kind    TEXT NOT NULL DEFAULT '',
			git_ref_path    TEXT NOT NULL DEFAULT ''
		);
		CREATE VIRTUAL TABLE sessions_fts USING fts5(
			title, prompt, draft, project_name,
			content=sessions, content_rowid=rowid
		);
		CREATE TRIGGER sessions_fts_insert AFTER INSERT ON sessions BEGIN
			INSERT INTO sessions_fts(rowid, title, prompt, draft, project_name)
			VALUES (new.rowid, new.title, new.prompt, new.draft, new.project_name);
		END;
		CREATE TRIGGER sessions_fts_delete BEFORE DELETE ON sessions BEGIN
			INSERT INTO sessions_fts(sessions_fts, rowid, title, prompt, draft, project_name)
			VALUES ('delete', old.rowid, old.title, old.prompt, old.draft, old.project_name);
		END;
		CREATE TRIGGER sessions_fts_update AFTER UPDATE ON sessions BEGIN
			INSERT INTO sessions_fts(sessions_fts, rowid, title, prompt, draft, project_name)
			VALUES ('delete', old.rowid, old.title, old.prompt, old.draft, old.project_name);
			INSERT INTO sessions_fts(rowid, title, prompt, draft, project_name)
			VALUES (new.rowid, new.title, new.prompt, new.draft, new.project_name);
		END;
		PRAGMA user_version = 13;
	`); err != nil {
		t.Fatalf("seed v13 schema: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := db.Exec(`
		INSERT INTO sessions
			(id, backend, status, project_dir, project_name,
			 created_at, updated_at, host_id,
			 git_ref_kind, git_ref_url, git_ref_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "ses-fts-1", string(agent.BackendOpenCode), string(agent.StatusIdle),
		"/tmp/old", "old", now, now, "local",
		"remote", "git@github.com:acksell/clank.git", ""); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Re-open via store.Open to apply v14. Without the FTS teardown,
	// this fails with the trigger error from the bug report.
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open after seed: %v", err)
	}
	defer s.Close()

	sessions, err := s.LoadSessions()
	if err != nil {
		t.Fatalf("LoadSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	// Verify the legacy columns are gone.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen raw: %v", err)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT name FROM pragma_table_info('sessions')`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer rows.Close()
	// Note: project_dir is dropped by v14 then re-added by v15 in the new
	// (project_dir, git_remote_url) two-column GitRef shape, so we only
	// assert the columns that should remain absent end-to-end.
	dropped := map[string]bool{"project_name": false, "worktree_dir": false}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, ok := dropped[n]; ok {
			t.Errorf("column %q should have been dropped by v14", n)
		}
	}

	// Verify the FTS virtual table is gone.
	var ftsCount int
	if err := raw.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='sessions_fts'`).Scan(&ftsCount); err != nil {
		t.Fatalf("query sessions_fts: %v", err)
	}
	if ftsCount != 0 {
		t.Errorf("sessions_fts virtual table still present after v14")
	}
}
