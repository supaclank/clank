package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/internal/sqlmigrate"
	"github.com/acksell/clank/internal/store"
)

// baselineVersion is the goose version id of the baseline migration.
// Bump only if the baseline file is ever renamed (it shouldn't be).
const baselineVersion = 20260730000001

// legacyFinalSchema is the exact shape the retired PRAGMA user_version
// chain (v1–v33) left behind, ALTER history and all. Frozen here as the
// adoption contract: databases in this shape must open cleanly under
// goose, with the baseline no-opping via IF NOT EXISTS.
const legacyFinalSchema = `
	CREATE TABLE hosts (
		id          TEXT PRIMARY KEY,
		user_id     TEXT NOT NULL,
		provider    TEXT NOT NULL,
		external_id TEXT NOT NULL,
		hostname    TEXT NOT NULL,
		status      TEXT NOT NULL,
		last_url    TEXT NOT NULL DEFAULT '',
		last_token  TEXT NOT NULL DEFAULT '',
		auto_wake   INTEGER NOT NULL DEFAULT 0,
		created_at  DATETIME NOT NULL,
		updated_at  DATETIME NOT NULL,
		UNIQUE (user_id, provider)
	);
	ALTER TABLE hosts ADD COLUMN auth_token TEXT NOT NULL DEFAULT '';
	ALTER TABLE hosts ADD COLUMN notifier_token TEXT NOT NULL DEFAULT '';
	CREATE UNIQUE INDEX hosts_notifier_token_idx ON hosts(notifier_token) WHERE notifier_token != '';
	ALTER TABLE hosts ADD COLUMN provider_meta TEXT NOT NULL DEFAULT '{}';
	CREATE TABLE devices (
		user_id      TEXT NOT NULL,
		push_token   TEXT NOT NULL,
		platform     TEXT NOT NULL CHECK (platform IN ('ios', 'android')),
		created_at   DATETIME NOT NULL,
		last_seen_at DATETIME NOT NULL,
		PRIMARY KEY (user_id, push_token)
	);
	CREATE INDEX devices_user_id_idx ON devices(user_id);
	CREATE INDEX devices_push_token_idx ON devices(push_token);
	PRAGMA user_version = 33;
`

// TestOpen_AdoptsLegacyUserVersionDatabase seeds a database exactly as
// the retired migration chain left it — including rows — and asserts
// goose takes it over in place: no error, data intact, bookkeeping
// recorded, and the (user_id, provider) upsert path still working
// against the legacy inline UNIQUE constraint.
func TestOpen_AdoptsLegacyUserVersionDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "clank.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(legacyFinalSchema); err != nil {
		raw.Close()
		t.Fatalf("seed legacy schema: %v", err)
	}
	now := time.Now().UTC()
	if _, err := raw.Exec(
		`INSERT INTO hosts (id, user_id, provider, external_id, hostname, status, auth_token, notifier_token, created_at, updated_at)
		 VALUES ('h1', 'u1', 'flysprites', 'ext-1', 'h1.example.com', 'running', 'tok-auth', 'tok-notif', ?, ?)`,
		now, now,
	); err != nil {
		raw.Close()
		t.Fatalf("seed host row: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO devices (user_id, push_token, platform, created_at, last_seen_at)
		 VALUES ('u1', 'ExponentPushToken[x]', 'ios', ?, ?)`,
		now, now,
	); err != nil {
		raw.Close()
		t.Fatalf("seed device row: %v", err)
	}
	raw.Close()

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open on legacy-shaped DB: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	h, err := s.GetHostByUser(ctx, "u1", "flysprites")
	if err != nil {
		t.Fatalf("GetHostByUser after adoption: %v", err)
	}
	if h.ID != "h1" || h.AuthToken != "tok-auth" {
		t.Errorf("adopted host = %+v, want id=h1 auth_token=tok-auth", h)
	}
	devices, err := s.ListDevicesByUser(ctx, "u1")
	if err != nil {
		t.Fatalf("ListDevicesByUser after adoption: %v", err)
	}
	if len(devices) != 1 || devices[0].PushToken != "ExponentPushToken[x]" {
		t.Errorf("adopted devices = %+v, want the seeded row", devices)
	}

	// The (user_id, provider) upsert must keep working: on adopted DBs
	// the conflict target matches the legacy inline UNIQUE constraint
	// (fresh DBs match hosts_user_id_provider_idx instead).
	h.Hostname = "h1-renamed.example.com"
	if err := s.UpsertHost(ctx, h); err != nil {
		t.Fatalf("UpsertHost after adoption: %v", err)
	}
	h2, err := s.GetHostByUser(ctx, "u1", "flysprites")
	if err != nil {
		t.Fatalf("GetHostByUser after upsert: %v", err)
	}
	if h2.Hostname != "h1-renamed.example.com" {
		t.Errorf("Hostname = %q, want the upserted value", h2.Hostname)
	}

	assertBaselineRecorded(t, path)
}

// TestOpen_RecordsBaselineOnFreshDB pins the goose bookkeeping for the
// from-scratch path too.
func TestOpen_RecordsBaselineOnFreshDB(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "clank.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.Close()
	assertBaselineRecorded(t, path)
}

func assertBaselineRecorded(t *testing.T, path string) {
	t.Helper()
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

// TestOpen_FailsOnPreBaselineLegacyShape documents the adoption cutoff:
// databases older than the final legacy shape (user_version < 33 with
// missing columns) are not upgraded — the baseline's IF NOT EXISTS
// no-ops on the stale table and the first query against a missing
// column fails. Anyone holding such a database recreates it (delete the
// file) rather than expecting an upgrade path.
func TestOpen_FailsOnPreBaselineLegacyShape(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "clank.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// v17-era hosts: predates auth_token/notifier_token/provider_meta.
	if _, err := raw.Exec(`
		CREATE TABLE hosts (
			id          TEXT PRIMARY KEY,
			user_id     TEXT NOT NULL,
			provider    TEXT NOT NULL,
			external_id TEXT NOT NULL,
			hostname    TEXT NOT NULL,
			status      TEXT NOT NULL,
			last_url    TEXT NOT NULL DEFAULT '',
			last_token  TEXT NOT NULL DEFAULT '',
			auto_wake   INTEGER NOT NULL DEFAULT 0,
			created_at  DATETIME NOT NULL,
			updated_at  DATETIME NOT NULL,
			UNIQUE (user_id, provider)
		);
		PRAGMA user_version = 17;
	`); err != nil {
		raw.Close()
		t.Fatalf("seed v17 DB: %v", err)
	}
	raw.Close()

	s, err := store.Open(path)
	if err != nil {
		// Also acceptable: failing at Open would be an even earlier
		// signal. Today the baseline no-ops silently and queries fail.
		return
	}
	defer s.Close()
	_, err = s.GetHostByUser(context.Background(), "nobody", "flysprites")
	if err == nil || errors.Is(err, store.ErrHostNotFound) {
		t.Errorf("query on pre-baseline DB unexpectedly worked (err=%v); the adoption cutoff has silently widened", err)
	}
}
