-- Schema for sqlc type-checking of the host-side sessions store.
-- NOT a migration — production migrations live in store.go's migrate()
-- function. Mirror the post-migration shape of every host-side table
-- here so sqlc can type-check queries against it.
--
-- Compared to the legacy hub-side sessions table (PR <3) this drops:
--   - host_id (one host per user; no catalog dispatch)
--   - hub-internal indices that no longer apply
-- and renames git_remote_url → git_remote for clarity.

CREATE TABLE sessions (
    id              TEXT PRIMARY KEY,                  -- daemon-assigned ULID
    external_id     TEXT NOT NULL DEFAULT '',          -- backend's session id
    backend         TEXT NOT NULL,                     -- "opencode" | "claude"
    status          TEXT NOT NULL DEFAULT 'idle',      -- idle | busy | done | error
    visibility      TEXT NOT NULL DEFAULT '',          -- "" | done | archived
    follow_up       INTEGER NOT NULL DEFAULT 0,
    project_dir     TEXT NOT NULL DEFAULT '',
    git_remote      TEXT NOT NULL DEFAULT '',
    worktree_branch TEXT NOT NULL DEFAULT '',
    prompt          TEXT NOT NULL DEFAULT '',
    title           TEXT NOT NULL DEFAULT '',
    ticket_id       TEXT NOT NULL DEFAULT '',
    agent           TEXT NOT NULL DEFAULT '',          -- primary agent slug
    draft           TEXT NOT NULL DEFAULT '',
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL,
    last_read_at    DATETIME
);

CREATE INDEX idx_sessions_external_id ON sessions(external_id);
CREATE INDEX idx_sessions_status ON sessions(status);
CREATE INDEX idx_sessions_visibility ON sessions(visibility);

-- Primary agent catalog cache. Per-repo (project_dir, git_remote)
-- because opencode/claude config is committed to git and shared
-- across branches. backend keys the cache so opencode and claude
-- each have their own list.
CREATE TABLE primary_agents (
    backend             TEXT NOT NULL,
    project_dir         TEXT NOT NULL DEFAULT '',
    git_remote          TEXT NOT NULL DEFAULT '',
    primary_agents_json TEXT NOT NULL DEFAULT '[]',
    updated_at          DATETIME NOT NULL,
    PRIMARY KEY (backend, project_dir, git_remote)
);
