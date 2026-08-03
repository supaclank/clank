package store_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/supaclank/clank/internal/host/store"
	"github.com/supaclank/clank/internal/sqlmigrate"
)

// baselineVersion is the goose version id of the baseline migration.
const baselineVersion = 20260730000001

// TestOpen_RecordsBaselineOnFreshDB pins the goose bookkeeping for the
// from-scratch path.
func TestOpen_RecordsBaselineOnFreshDB(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "host.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.Close()

	raw, err := sql.Open("sqlite", path)
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
