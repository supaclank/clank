// Package store provides SQLite-backed persistence for session metadata.
//
// The store is the source of truth for user-owned fields (visibility,
// follow_up, draft, last_read_at) while backend-owned fields (title,
// timestamps, status) are refreshed from the agent backend on discovery.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/acksell/clank/internal/agent"

	// Pure-Go SQLite driver (no CGo).
	_ "modernc.org/sqlite"
)

// Store wraps a SQLite database for persisting session metadata.
type Store struct {
	db *sql.DB
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

	s := &Store{db: db}
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
	_ = version // suppress unused warning after last migration

	return nil
}

// LoadSessions returns all persisted sessions. Returns an empty (non-nil)
// slice when no sessions exist.
func (s *Store) LoadSessions() ([]agent.SessionInfo, error) {
	rows, err := s.db.Query(`
		SELECT id, external_id, backend, status, visibility, follow_up,
		       project_dir, project_name, worktree_branch, worktree_dir,
		       prompt, title, ticket_id, agent, draft,
		       created_at, updated_at, last_read_at
		FROM sessions
	`)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	var sessions []agent.SessionInfo
	for rows.Next() {
		var info agent.SessionInfo
		var followUp int
		var lastReadAt sql.NullTime
		err := rows.Scan(
			&info.ID,
			&info.ExternalID,
			&info.Backend,
			&info.Status,
			&info.Visibility,
			&followUp,
			&info.ProjectDir,
			&info.ProjectName,
			&info.WorktreeBranch,
			&info.WorktreeDir,
			&info.Prompt,
			&info.Title,
			&info.TicketID,
			&info.Agent,
			&info.Draft,
			&info.CreatedAt,
			&info.UpdatedAt,
			&lastReadAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan session row: %w", err)
		}
		info.FollowUp = followUp != 0
		if lastReadAt.Valid {
			info.LastReadAt = lastReadAt.Time
		}
		sessions = append(sessions, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}

	if sessions == nil {
		sessions = []agent.SessionInfo{}
	}
	return sessions, nil
}

// UpsertSession inserts or replaces a session in the database.
// All fields are overwritten.
func (s *Store) UpsertSession(info agent.SessionInfo) error {
	followUp := 0
	if info.FollowUp {
		followUp = 1
	}
	var lastReadAt *time.Time
	if !info.LastReadAt.IsZero() {
		lastReadAt = &info.LastReadAt
	}

	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO sessions
			(id, external_id, backend, status, visibility, follow_up,
			 project_dir, project_name, worktree_branch, worktree_dir,
			 prompt, title, ticket_id, agent, draft,
			 created_at, updated_at, last_read_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		info.ID,
		info.ExternalID,
		string(info.Backend),
		string(info.Status),
		string(info.Visibility),
		followUp,
		info.ProjectDir,
		info.ProjectName,
		info.WorktreeBranch,
		info.WorktreeDir,
		info.Prompt,
		info.Title,
		info.TicketID,
		info.Agent,
		info.Draft,
		info.CreatedAt,
		info.UpdatedAt,
		lastReadAt,
	)
	if err != nil {
		return fmt.Errorf("upsert session %s: %w", info.ID, err)
	}
	return nil
}

// DeleteSession removes a session by its daemon ID.
func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	return nil
}

// LoadPrimaryAgents returns the cached primary agent list for the given
// backend and project directory. Returns nil (not an error) if no cached
// entry exists.
func (s *Store) LoadPrimaryAgents(backend agent.BackendType, projectDir string) ([]agent.AgentInfo, error) {
	var agentsJSON string
	err := s.db.QueryRow(`
		SELECT primary_agents_json FROM primary_agents WHERE backend = ? AND project_dir = ?
	`, string(backend), projectDir).Scan(&agentsJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load primary agents for %s/%s: %w", backend, projectDir, err)
	}
	var agents []agent.AgentInfo
	if err := json.Unmarshal([]byte(agentsJSON), &agents); err != nil {
		return nil, fmt.Errorf("decode primary agents for %s/%s: %w", backend, projectDir, err)
	}
	return agents, nil
}

// UpsertPrimaryAgents stores the primary agent list for the given backend
// and project directory.
func (s *Store) UpsertPrimaryAgents(backend agent.BackendType, projectDir string, agents []agent.AgentInfo) error {
	data, err := json.Marshal(agents)
	if err != nil {
		return fmt.Errorf("encode primary agents: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT OR REPLACE INTO primary_agents (backend, project_dir, primary_agents_json, updated_at)
		VALUES (?, ?, ?, ?)
	`, string(backend), projectDir, string(data), time.Now())
	if err != nil {
		return fmt.Errorf("upsert primary agents for %s/%s: %w", backend, projectDir, err)
	}
	return nil
}

// KnownProjectDirs returns the distinct project directories that have at
// least one session for the given backend type. Used to know which projects
// to pre-warm primary agent caches for.
func (s *Store) KnownProjectDirs(backend agent.BackendType) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT project_dir FROM sessions WHERE backend = ?
	`, string(backend))
	if err != nil {
		return nil, fmt.Errorf("query project dirs: %w", err)
	}
	defer rows.Close()
	var dirs []string
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			return nil, fmt.Errorf("scan project dir: %w", err)
		}
		dirs = append(dirs, dir)
	}
	return dirs, rows.Err()
}

// FindByExternalID returns the persisted session matching the given
// external (backend) ID, or nil if not found.
func (s *Store) FindByExternalID(externalID string) (*agent.SessionInfo, error) {
	var info agent.SessionInfo
	var followUp int
	var lastReadAt sql.NullTime
	err := s.db.QueryRow(`
		SELECT id, external_id, backend, status, visibility, follow_up,
		       project_dir, project_name, worktree_branch, worktree_dir,
		       prompt, title, ticket_id, agent, draft,
		       created_at, updated_at, last_read_at
		FROM sessions
		WHERE external_id = ?
	`, externalID).Scan(
		&info.ID,
		&info.ExternalID,
		&info.Backend,
		&info.Status,
		&info.Visibility,
		&followUp,
		&info.ProjectDir,
		&info.ProjectName,
		&info.WorktreeBranch,
		&info.WorktreeDir,
		&info.Prompt,
		&info.Title,
		&info.TicketID,
		&info.Agent,
		&info.Draft,
		&info.CreatedAt,
		&info.UpdatedAt,
		&lastReadAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find session by external_id %s: %w", externalID, err)
	}
	info.FollowUp = followUp != 0
	if lastReadAt.Valid {
		info.LastReadAt = lastReadAt.Time
	}
	return &info, nil
}
