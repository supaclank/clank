package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/internal/host/store"
	"github.com/acksell/clank/internal/sqlmigrate"
)

// baselineVersion is the goose version id of the baseline migration.
const baselineVersion = 20260730000001

// legacyFinalSchema is the exact shape the retired PRAGMA user_version
// chain (v1–v4 plus the probe-based column reconcile) left behind.
// Frozen here as the adoption contract: databases in this shape must
// open cleanly under goose, with the baseline no-opping via
// IF NOT EXISTS.
const legacyFinalSchema = `
	CREATE TABLE sessions (
		id              TEXT PRIMARY KEY,
		external_id     TEXT NOT NULL DEFAULT '',
		backend         TEXT NOT NULL,
		status          TEXT NOT NULL DEFAULT 'idle',
		visibility      TEXT NOT NULL DEFAULT '',
		follow_up       INTEGER NOT NULL DEFAULT 0,
		project_dir     TEXT NOT NULL DEFAULT '',
		worktree_id     TEXT NOT NULL DEFAULT '',
		worktree_branch TEXT NOT NULL DEFAULT '',
		prompt          TEXT NOT NULL DEFAULT '',
		title           TEXT NOT NULL DEFAULT '',
		ticket_id       TEXT NOT NULL DEFAULT '',
		agent           TEXT NOT NULL DEFAULT '',
		draft           TEXT NOT NULL DEFAULT '',
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL,
		last_read_at    INTEGER
	);
	ALTER TABLE sessions ADD COLUMN subdir TEXT NOT NULL DEFAULT '';
	ALTER TABLE sessions ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
	CREATE INDEX idx_sessions_external_id ON sessions(external_id);
	CREATE INDEX idx_sessions_status ON sessions(status);
	CREATE INDEX idx_sessions_visibility ON sessions(visibility);
	CREATE TABLE primary_agents (
		backend             TEXT NOT NULL,
		project_dir         TEXT NOT NULL DEFAULT '',
		worktree_id         TEXT NOT NULL DEFAULT '',
		primary_agents_json TEXT NOT NULL DEFAULT '[]',
		updated_at          INTEGER NOT NULL,
		PRIMARY KEY (backend, project_dir, worktree_id)
	);
	PRAGMA user_version = 4;
`

// TestOpen_AdoptsLegacyUserVersionDatabase seeds a database exactly as
// the retired migration chain left it — including a session row — and
// asserts goose takes it over in place: no error, data intact,
// bookkeeping recorded.
func TestOpen_AdoptsLegacyUserVersionDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "host.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(legacyFinalSchema); err != nil {
		raw.Close()
		t.Fatalf("seed legacy schema: %v", err)
	}
	seededMs := time.Date(2026, 5, 12, 14, 55, 0, 0, time.UTC).UnixMilli()
	if _, err := raw.Exec(
		`INSERT INTO sessions (id, backend, title, subdir, created_at, updated_at)
		 VALUES ('adopted-1', 'opencode', 'pre-goose session', 'web-app', ?, ?)`,
		seededMs, seededMs,
	); err != nil {
		raw.Close()
		t.Fatalf("seed session row: %v", err)
	}
	raw.Close()

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open on legacy-shaped DB: %v", err)
	}
	defer s.Close()

	got, err := s.GetSession(context.Background(), "adopted-1")
	if err != nil {
		t.Fatalf("GetSession after adoption: %v", err)
	}
	if got.Title != "pre-goose session" || got.GitRef.Subdir != "web-app" {
		t.Errorf("adopted session = %+v, want seeded title/subdir", got)
	}
	if got.CreatedAt.UnixMilli() != seededMs {
		t.Errorf("CreatedAt = %d ms, want %d ms (timestamps must survive adoption untouched)",
			got.CreatedAt.UnixMilli(), seededMs)
	}

	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var applied int
	if err := raw.QueryRow(
		`SELECT COUNT(*) FROM goose_db_version WHERE version_id = ? AND is_applied = 1`,
		baselineVersion,
	).Scan(&applied); err != nil {
		t.Fatalf("read goose_db_version: %v", err)
	}
	if applied != 1 {
		t.Errorf("baseline %d applied %d times in goose_db_version, want 1", baselineVersion, applied)
	}
}

// TestMigrationsMatchDeclaredSchema keeps the two schema sources honest
// against each other: the goose migrations (what a real database runs)
// must produce exactly the schema declared in schema.sql (what sqlc
// type-checks queries against, and what Atlas diffs to plan the next
// migration). Drift here means the generated query code is lying about
// the database it talks to.
func TestMigrationsMatchDeclaredSchema(t *testing.T) {
	t.Parallel()

	migratedPath := filepath.Join(t.TempDir(), "migrated.db")
	s, err := store.Open(migratedPath)
	if err != nil {
		t.Fatalf("Open (goose path): %v", err)
	}
	s.Close()
	migratedDB, err := sql.Open("sqlite", migratedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migratedDB.Close()
	migrated, err := sqlmigrate.DumpSchema(migratedDB)
	if err != nil {
		t.Fatalf("dump migrated schema: %v", err)
	}

	declaredSQL, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	declaredDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "declared.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer declaredDB.Close()
	if _, err := declaredDB.Exec(string(declaredSQL)); err != nil {
		t.Fatalf("exec schema.sql: %v", err)
	}
	declared, err := sqlmigrate.DumpSchema(declaredDB)
	if err != nil {
		t.Fatalf("dump declared schema: %v", err)
	}

	if migrated != declared {
		t.Errorf("migrations/ and schema.sql disagree.\n--- via migrations:\n%s\n--- via schema.sql:\n%s", migrated, declared)
	}
}
