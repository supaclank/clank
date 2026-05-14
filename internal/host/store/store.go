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
				worktree_id     TEXT NOT NULL DEFAULT '',
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
				worktree_id         TEXT NOT NULL DEFAULT '',
				primary_agents_json TEXT NOT NULL DEFAULT '[]',
				updated_at          DATETIME NOT NULL,
				PRIMARY KEY (backend, project_dir, worktree_id)
			);
			PRAGMA user_version = 1;
		`)
		if err != nil {
			return fmt.Errorf("migration v1: %w", err)
		}
		version = 1
	}
	if version < 2 {
		// Earlier dev installs were created when sessions/primary_agents
		// had a git_remote column. The conceptual rename (git URL →
		// clank-sync worktree id) means dropping the URL value entirely
		// rather than carrying it forward. SQLite's ALTER ... RENAME
		// COLUMN preserves data byte-for-byte, but we want a clean
		// "(re-register your worktree to populate this)" reset, so we
		// rename to keep the schema shape and let the values default to
		// "" via UPDATE.
		//
		// New installs that hit v1 with worktree_id already in the
		// schema short-circuit via pragma_table_info — no-op.
		var hasOld int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'git_remote'`).Scan(&hasOld); err != nil {
			return fmt.Errorf("migration v2: probe sessions.git_remote: %w", err)
		}
		if hasOld > 0 {
			if _, err := s.db.Exec(`ALTER TABLE sessions RENAME COLUMN git_remote TO worktree_id`); err != nil {
				return fmt.Errorf("migration v2: rename sessions.git_remote: %w", err)
			}
			if _, err := s.db.Exec(`UPDATE sessions SET worktree_id = ''`); err != nil {
				return fmt.Errorf("migration v2: clear sessions.worktree_id: %w", err)
			}
		}
		// primary_agents has the column inside a PRIMARY KEY. SQLite's
		// RENAME COLUMN handles PK columns since 3.25; the index is
		// preserved automatically.
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('primary_agents') WHERE name = 'git_remote'`).Scan(&hasOld); err != nil {
			return fmt.Errorf("migration v2: probe primary_agents.git_remote: %w", err)
		}
		if hasOld > 0 {
			if _, err := s.db.Exec(`ALTER TABLE primary_agents RENAME COLUMN git_remote TO worktree_id`); err != nil {
				return fmt.Errorf("migration v2: rename primary_agents.git_remote: %w", err)
			}
			// The (backend, project_dir, '') rows would now collide on
			// the new PK if any duplicates existed pre-rename. Wipe the
			// cache — primary agents reload from the host on demand.
			if _, err := s.db.Exec(`DELETE FROM primary_agents`); err != nil {
				return fmt.Errorf("migration v2: clear primary_agents: %w", err)
			}
		}
		if _, err := s.db.Exec(`PRAGMA user_version = 2`); err != nil {
			return fmt.Errorf("migration v2: bump version: %w", err)
		}
		version = 2
	}
	if version < 3 {
		// Migration v3: replace DATETIME-as-TEXT columns with INTEGER
		// unix-millis. modernc.org/sqlite serialises time.Time via
		// t.String() (including the monotonic-clock m=+... suffix for
		// time.Now() values), which it then can't parse back —
		// reproduced empirically in v1.50.1. Storing INTEGER millis
		// sidesteps the entire round-trip class.
		//
		// Strategy: build a parallel table with the new shape, copy
		// rows over (parsing every legacy stringified-time format we've
		// seen in production via parseLegacyTimeMs), drop the old
		// table, rename, recreate indexes. Standard SQLite migration
		// pattern; survives in a transaction.
		if err := migrateSessionsToMillis(s.db); err != nil {
			return fmt.Errorf("migration v3: sessions: %w", err)
		}
		if err := migratePrimaryAgentsToMillis(s.db); err != nil {
			return fmt.Errorf("migration v3: primary_agents: %w", err)
		}
		if _, err := s.db.Exec(`PRAGMA user_version = 3`); err != nil {
			return fmt.Errorf("migration v3: bump version: %w", err)
		}
		version = 3
	}
	_ = version
	return nil
}

// migrateSessionsToMillis is migration v3 for the sessions table.
// See the comment in migrate() for context. Failure leaves the
// original table untouched (the new table lives in a transaction).
func migrateSessionsToMillis(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`CREATE TABLE sessions_v3 (
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
	)`); err != nil {
		return fmt.Errorf("create sessions_v3: %w", err)
	}

	// Read legacy rows. Time columns are scanned as raw strings to
	// avoid the very driver bug this migration exists to fix.
	rows, err := tx.Query(`SELECT
		id, external_id, backend, status, visibility, follow_up,
		project_dir, worktree_id, worktree_branch, prompt, title,
		ticket_id, agent, draft,
		CAST(created_at AS TEXT), CAST(updated_at AS TEXT), CAST(last_read_at AS TEXT)
	FROM sessions`)
	if err != nil {
		return fmt.Errorf("read legacy rows: %w", err)
	}
	defer rows.Close()

	insert, err := tx.Prepare(`INSERT INTO sessions_v3 (
		id, external_id, backend, status, visibility, follow_up,
		project_dir, worktree_id, worktree_branch, prompt, title,
		ticket_id, agent, draft, created_at, updated_at, last_read_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer insert.Close()

	for rows.Next() {
		var (
			id, externalID, backend, status, visibility           string
			followUp                                              int64
			projectDir, worktreeID, worktreeBranch, prompt, title string
			ticketID, agentSlug, draft                            string
			createdRaw, updatedRaw, lastReadRaw                   sql.NullString
		)
		if err := rows.Scan(
			&id, &externalID, &backend, &status, &visibility, &followUp,
			&projectDir, &worktreeID, &worktreeBranch, &prompt, &title,
			&ticketID, &agentSlug, &draft,
			&createdRaw, &updatedRaw, &lastReadRaw,
		); err != nil {
			return fmt.Errorf("scan legacy row: %w", err)
		}
		createdMs := parseLegacyTimeMs(createdRaw.String, id)
		updatedMs := parseLegacyTimeMs(updatedRaw.String, id)
		var lastReadMs sql.NullInt64
		if lastReadRaw.Valid && lastReadRaw.String != "" {
			lastReadMs = sql.NullInt64{Int64: parseLegacyTimeMs(lastReadRaw.String, id), Valid: true}
		}
		if _, err := insert.Exec(
			id, externalID, backend, status, visibility, followUp,
			projectDir, worktreeID, worktreeBranch, prompt, title,
			ticketID, agentSlug, draft,
			createdMs, updatedMs, lastReadMs,
		); err != nil {
			return fmt.Errorf("insert into sessions_v3 (%s): %w", id, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy rows: %w", err)
	}

	if _, err := tx.Exec(`DROP TABLE sessions`); err != nil {
		return fmt.Errorf("drop legacy table: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE sessions_v3 RENAME TO sessions`); err != nil {
		return fmt.Errorf("rename sessions_v3: %w", err)
	}
	// Indexes were dropped along with the legacy table.
	if _, err := tx.Exec(`
		CREATE INDEX idx_sessions_external_id ON sessions(external_id);
		CREATE INDEX idx_sessions_status ON sessions(status);
		CREATE INDEX idx_sessions_visibility ON sessions(visibility);
	`); err != nil {
		return fmt.Errorf("recreate indexes: %w", err)
	}
	return tx.Commit()
}

// migratePrimaryAgentsToMillis mirrors migrateSessionsToMillis for
// the primary_agents table — only one time column (updated_at), no
// secondary indexes.
func migratePrimaryAgentsToMillis(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`CREATE TABLE primary_agents_v3 (
		backend             TEXT NOT NULL,
		project_dir         TEXT NOT NULL DEFAULT '',
		worktree_id         TEXT NOT NULL DEFAULT '',
		primary_agents_json TEXT NOT NULL DEFAULT '[]',
		updated_at          INTEGER NOT NULL,
		PRIMARY KEY (backend, project_dir, worktree_id)
	)`); err != nil {
		return fmt.Errorf("create primary_agents_v3: %w", err)
	}

	rows, err := tx.Query(`SELECT
		backend, project_dir, worktree_id, primary_agents_json,
		CAST(updated_at AS TEXT)
	FROM primary_agents`)
	if err != nil {
		return fmt.Errorf("read legacy rows: %w", err)
	}
	defer rows.Close()

	insert, err := tx.Prepare(`INSERT INTO primary_agents_v3 (
		backend, project_dir, worktree_id, primary_agents_json, updated_at
	) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer insert.Close()

	for rows.Next() {
		var (
			backend, projectDir, worktreeID, json string
			updatedRaw                            sql.NullString
		)
		if err := rows.Scan(&backend, &projectDir, &worktreeID, &json, &updatedRaw); err != nil {
			return fmt.Errorf("scan legacy row: %w", err)
		}
		// primary_agents has no ULID — fall back to now() if the
		// stored value is malformed. Acceptable: this table is a
		// cache, callers re-derive content on miss.
		updatedMs := parseLegacyTimeMs(updatedRaw.String, "")
		if _, err := insert.Exec(backend, projectDir, worktreeID, json, updatedMs); err != nil {
			return fmt.Errorf("insert into primary_agents_v3: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy rows: %w", err)
	}

	if _, err := tx.Exec(`DROP TABLE primary_agents`); err != nil {
		return fmt.Errorf("drop legacy table: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE primary_agents_v3 RENAME TO primary_agents`); err != nil {
		return fmt.Errorf("rename primary_agents_v3: %w", err)
	}
	return tx.Commit()
}

// withQ exposes the generated Queries to callers in the same package
// (e.g. sessions.go). External packages should use the high-level
// methods on Store.
func (s *Store) withQ(_ context.Context) *hostsqlitedb.Queries {
	return s.q
}
