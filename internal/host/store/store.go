// Package store is the host's local SQLite for session metadata and
// the primary-agent cache. Owned by clank-host (the per-user host
// process), opened at the path specified by --data-dir.
//
// Compared to internal/store (provisioner-side, hosts table) this
// store lives on a different machine in the cloud topology — the
// host runs inside a sprite/sandbox, the provisioner runs in the
// gateway/clankd process. Same Go pattern, different file, different
// owner.
package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/acksell/clank/internal/host/store/hostsqlitedb"

	// Pure-Go SQLite driver (no CGo).
	_ "modernc.org/sqlite"
)

// Store wraps the host's SQLite database. The high-level methods
// in sessions.go delegate to the sqlc-generated Queries.
type Store struct {
	db *sql.DB
	q  *hostsqlitedb.Queries
}

// Open opens (or creates) the host's SQLite database at dbPath and
// runs any pending schema migrations.
func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}

	// SQLite supports a single concurrent writer. Limiting the pool
	// to one connection keeps PRAGMAs consistent and serialises
	// writes through Go's sql.DB.
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("exec %q: %w", pragma, err)
		}
	}

	s := &Store{db: db, q: hostsqlitedb.New(db)}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close releases the underlying database connection.
func (s *Store) Close() error { return s.db.Close() }

// migrate applies host-side schema migrations using PRAGMA user_version.
// Schema is mirrored in schema/0001_sessions.sql for sqlc.
func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	if version < 1 {
		_, err := s.db.Exec(`
			CREATE TABLE sessions (
				id              TEXT PRIMARY KEY,
				external_id     TEXT NOT NULL DEFAULT '',
				backend         TEXT NOT NULL,
				status          TEXT NOT NULL DEFAULT 'idle',
				visibility      TEXT NOT NULL DEFAULT '',
				follow_up       INTEGER NOT NULL DEFAULT 0,
				project_dir     TEXT NOT NULL DEFAULT '',
				git_remote      TEXT NOT NULL DEFAULT '',
				worktree_branch TEXT NOT NULL DEFAULT '',
				prompt          TEXT NOT NULL DEFAULT '',
				title           TEXT NOT NULL DEFAULT '',
				ticket_id       TEXT NOT NULL DEFAULT '',
				agent           TEXT NOT NULL DEFAULT '',
				draft           TEXT NOT NULL DEFAULT '',
				created_at      DATETIME NOT NULL,
				updated_at      DATETIME NOT NULL,
				last_read_at    DATETIME
			);
			CREATE INDEX idx_sessions_external_id ON sessions(external_id);
			CREATE INDEX idx_sessions_status ON sessions(status);
			CREATE INDEX idx_sessions_visibility ON sessions(visibility);
			CREATE TABLE primary_agents (
				backend             TEXT NOT NULL,
				project_dir         TEXT NOT NULL DEFAULT '',
				git_remote          TEXT NOT NULL DEFAULT '',
				primary_agents_json TEXT NOT NULL DEFAULT '[]',
				updated_at          DATETIME NOT NULL,
				PRIMARY KEY (backend, project_dir, git_remote)
			);
			PRAGMA user_version = 1;
		`)
		if err != nil {
			return fmt.Errorf("migration v1: %w", err)
		}
		version = 1
	}
	_ = version
	return nil
}

// withQ exposes the generated Queries to callers in the same package
// (e.g. sessions.go). External packages should use the high-level
// methods on Store.
func (s *Store) withQ(_ context.Context) *hostsqlitedb.Queries {
	return s.q
}
