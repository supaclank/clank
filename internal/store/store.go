// Package store provides SQLite-backed persistence for the provisioner's
// host registry (the `hosts` table). PR 3 dropped the hub-owned session,
// agent, and sync tables; session metadata now lives in the host's own
// store at internal/host/store.
package store

import (
	"database/sql"
	"fmt"

	"github.com/acksell/clank/internal/store/sqlitedb"

	// Pure-Go SQLite driver (no CGo).
	_ "modernc.org/sqlite"
)

// Store wraps a SQLite database for persisting session metadata and host
// registry state. New tables are accessed via the sqlc-generated Queries
// in q; legacy tables (sessions, primary_agents, sync_state, etc.) still
// use raw SQL on db.
type Store struct {
	db *sql.DB
	q  *sqlitedb.Queries
}

// Open opens (or creates) a SQLite database at dbPath and runs any
// pending schema migrations. The caller must call Close when done.
func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}

	// SQLite only supports a single concurrent writer. Limiting the pool
	// to one connection ensures all PRAGMAs apply to every query (they are
	// per-connection state) and serialises writes through Go's sql.DB,
	// avoiding SQLITE_BUSY errors from pool-spawned connections that would
	// otherwise lack the busy_timeout PRAGMA.
	db.SetMaxOpenConns(1)

	// Configure SQLite for concurrent access and durability.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("exec %q: %w", p, err)
		}
	}

	s := &Store{db: db, q: sqlitedb.New(db)}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate applies schema migrations using PRAGMA user_version.
func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	if version < 1 {
		_, err := s.db.Exec(`
			CREATE TABLE IF NOT EXISTS sessions (
				id            TEXT PRIMARY KEY,
				external_id   TEXT NOT NULL DEFAULT '',
				backend       TEXT NOT NULL,
				status        TEXT NOT NULL DEFAULT 'idle',
				visibility    TEXT NOT NULL DEFAULT '',
				follow_up     INTEGER NOT NULL DEFAULT 0,
				project_dir   TEXT NOT NULL,
				project_name  TEXT NOT NULL,
				prompt        TEXT NOT NULL DEFAULT '',
				title         TEXT NOT NULL DEFAULT '',
				ticket_id     TEXT NOT NULL DEFAULT '',
				agent         TEXT NOT NULL DEFAULT '',
				draft         TEXT NOT NULL DEFAULT '',
				created_at    DATETIME NOT NULL,
				updated_at    DATETIME NOT NULL,
				last_read_at  DATETIME
			);
			CREATE INDEX IF NOT EXISTS idx_sessions_external_id ON sessions(external_id);
			PRAGMA user_version = 1;
		`)
		if err != nil {
			return fmt.Errorf("migration v1: %w", err)
		}
		version = 1
	}

	if version < 2 {
		_, err := s.db.Exec(`
			CREATE TABLE IF NOT EXISTS agents (
				backend      TEXT NOT NULL,
				project_dir  TEXT NOT NULL,
				agents_json  TEXT NOT NULL DEFAULT '[]',
				updated_at   DATETIME NOT NULL,
				PRIMARY KEY (backend, project_dir)
			);
			PRAGMA user_version = 2;
		`)
		if err != nil {
			return fmt.Errorf("migration v2: %w", err)
		}
		version = 2
	}

	if version < 3 {
		_, err := s.db.Exec(`
			ALTER TABLE agents RENAME TO primary_agents;
			ALTER TABLE primary_agents RENAME COLUMN agents_json TO primary_agents_json;
			PRAGMA user_version = 3;
		`)
		if err != nil {
			return fmt.Errorf("migration v3: %w", err)
		}
		version = 3
	}

	if version < 9 {
		_, err := s.db.Exec(`
			ALTER TABLE sessions ADD COLUMN branch TEXT NOT NULL DEFAULT '';
			ALTER TABLE sessions ADD COLUMN worktree_dir TEXT NOT NULL DEFAULT '';
			PRAGMA user_version = 9;
		`)
		if err != nil {
			return fmt.Errorf("migration v9: %w", err)
		}
		version = 9
	}

	if version < 10 {
		_, err := s.db.Exec(`
			ALTER TABLE sessions RENAME COLUMN branch TO worktree_branch;
			PRAGMA user_version = 10;
		`)
		if err != nil {
			return fmt.Errorf("migration v10: %w", err)
		}
		version = 10
	}
	if version < 11 {
		_, err := s.db.Exec(`
			ALTER TABLE sessions ADD COLUMN host_id TEXT NOT NULL DEFAULT 'local';
			ALTER TABLE sessions ADD COLUMN repo_remote_url TEXT NOT NULL DEFAULT '';
			PRAGMA user_version = 11;
		`)
		if err != nil {
			return fmt.Errorf("migration v11: %w", err)
		}
		version = 11
	}
	if version < 12 {
		_, err := s.db.Exec(`
			ALTER TABLE sessions RENAME COLUMN repo_remote_url TO git_ref_url;
			ALTER TABLE sessions ADD COLUMN git_ref_kind TEXT NOT NULL DEFAULT '';
			ALTER TABLE sessions ADD COLUMN git_ref_path TEXT NOT NULL DEFAULT '';
			UPDATE sessions SET git_ref_kind = 'remote' WHERE git_ref_url != '';
			PRAGMA user_version = 12;
		`)
		if err != nil {
			return fmt.Errorf("migration v12: %w", err)
		}
		version = 12
	}
	if version < 13 {
		_, err := s.db.Exec(`
			DROP TABLE IF EXISTS primary_agents;
			CREATE TABLE primary_agents (
				backend             TEXT NOT NULL,
				host_id             TEXT NOT NULL,
				git_ref_kind        TEXT NOT NULL,
				git_ref_url         TEXT NOT NULL DEFAULT '',
				git_ref_path        TEXT NOT NULL DEFAULT '',
				primary_agents_json TEXT NOT NULL DEFAULT '[]',
				updated_at          DATETIME NOT NULL,
				PRIMARY KEY (backend, host_id, git_ref_kind, git_ref_url, git_ref_path)
			);
			PRAGMA user_version = 13;
		`)
		if err != nil {
			return fmt.Errorf("migration v13: %w", err)
		}
		version = 13
	}
	if version < 14 {
		if err := s.dropMigrationV14(); err != nil {
			return fmt.Errorf("migration v14: %w", err)
		}
		version = 14
	}
	if version < 15 {
		// Step 8 of hub_host_refactor_code_review.md §7.8: collapse
		// (kind, url, path) → (project_dir, git_remote_url) on both
		// sessions and primary_agents. The new GitRef pointer-shape
		// (Local *LocalRef | Remote *RemoteRef) maps cleanly to two
		// optional columns: at most one is non-empty.
		if err := s.migrateV15(); err != nil {
			return fmt.Errorf("migration v15: %w", err)
		}
		version = 15
	}
	if version < 16 {
		// Hub-to-hub sync tables. See migrate_v16.go for shape rationale.
		if err := s.migrateV16(); err != nil {
			return fmt.Errorf("migration v16: %w", err)
		}
		version = 16
	}
	if version < 17 {
		// hosts: per-user persistent compute (Daytona sandbox, Sprite,
		// k8s pod, …). UNIQUE (user_id, provider) enforces the
		// one-host-per-user-per-provider invariant at the DB layer
		// even if app code has a bug.
		// Schema mirrored in internal/store/schema/0001_hosts.sql for
		// sqlc; keep them in sync.
		_, err := s.db.Exec(`
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
		`)
		if err != nil {
			return fmt.Errorf("migration v17: %w", err)
		}
		version = 17
	}
	if version < 18 {
		// auth_token: clank-host's bearer-token, checked by the
		// require-bearer middleware on every HTTP request. Universal
		// across providers (Daytona stacks it on top of last_token's
		// preview-token; Sprites use it as the only auth layer).
		// See PR 2 of the persistent-host roadmap.
		//
		// (Renamed from cap_token to auth_token in v19 — see below.
		// This migration uses the post-rename name so installs that
		// jump straight from v17 to v19 don't need the rename step.)
		_, err := s.db.Exec(`
			ALTER TABLE hosts ADD COLUMN auth_token TEXT NOT NULL DEFAULT '';
			PRAGMA user_version = 18;
		`)
		if err != nil {
			return fmt.Errorf("migration v18: %w", err)
		}
		version = 18
	}
	if version < 19 {
		// Rename auth_token's predecessor (cap_token) for installs
		// that ran an earlier draft of v18. SQLite raises "duplicate
		// column" on the ALTER below if cap_token doesn't exist; we
		// look for it first and skip the rename when it's already
		// auth_token.
		var exists int
		if err := s.db.QueryRow(`
			SELECT COUNT(*) FROM pragma_table_info('hosts') WHERE name = 'cap_token'
		`).Scan(&exists); err != nil {
			return fmt.Errorf("migration v19: probe for legacy column: %w", err)
		}
		if exists > 0 {
			if _, err := s.db.Exec(`ALTER TABLE hosts RENAME COLUMN cap_token TO auth_token;`); err != nil {
				return fmt.Errorf("migration v19: rename cap_token: %w", err)
			}
		}
		if _, err := s.db.Exec(`PRAGMA user_version = 19;`); err != nil {
			return fmt.Errorf("migration v19: bump version: %w", err)
		}
		version = 19
	}
	if version < 20 {
		// PR 3 deletes the hub. Sessions, primary_agents, and sync_state
		// were hub-owned tables; session metadata now lives in the
		// host's own SQLite (internal/host/store) and the hub-to-hub
		// sync mirror is gone. Drop the orphaned tables so clank.db
		// shrinks to provisioner state (just `hosts`).
		_, err := s.db.Exec(`
			DROP TABLE IF EXISTS sessions;
			DROP TABLE IF EXISTS primary_agents;
			DROP TABLE IF EXISTS sync_state;
			DROP TABLE IF EXISTS synced_repos;
			DROP TABLE IF EXISTS synced_branches;
			PRAGMA user_version = 20;
		`)
		if err != nil {
			return fmt.Errorf("migration v20: %w", err)
		}
		version = 20
	}
	_ = version // suppress unused warning after last migration

	return nil
}

