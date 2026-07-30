-- Declarative schema for the host-side database: the single source of
-- truth for its desired shape.
--
--   * sqlc type-checks queries/ against this file (sqlc.yaml points here).
--   * Atlas diffs this file against migrations/ to generate new goose
--     migration files — see the `make migration` target. Never edit the
--     database by hand-writing DDL elsewhere; change this file and diff.
--
-- This file is never executed against a real database; migrations/ is
-- what runs (applied by goose at Open()).
--
-- Time columns are INTEGER (unix milliseconds since epoch) rather than
-- DATETIME-as-TEXT. modernc.org/sqlite serialises time.Time parameters
-- via t.String() — including the monotonic clock suffix (m=+...) for
-- time.Now() values — which it then can't parse back. Storing as
-- INTEGER avoids the round-trip bug entirely.

CREATE TABLE sessions (
    id              TEXT PRIMARY KEY,                  -- daemon-assigned ULID
    external_id     TEXT NOT NULL DEFAULT '',          -- backend's session id
    backend         TEXT NOT NULL,                     -- "opencode" | "claude"
    status          TEXT NOT NULL DEFAULT 'idle',      -- idle | busy | done | error
    visibility      TEXT NOT NULL DEFAULT '',          -- "" | done | archived
    follow_up       INTEGER NOT NULL DEFAULT 0,
    project_dir     TEXT NOT NULL DEFAULT '',
    worktree_id     TEXT NOT NULL DEFAULT '',          -- clank-sync worktree ULID; cross-machine stable identity
    worktree_branch TEXT NOT NULL DEFAULT '',
    prompt          TEXT NOT NULL DEFAULT '',
    title           TEXT NOT NULL DEFAULT '',
    ticket_id       TEXT NOT NULL DEFAULT '',
    agent           TEXT NOT NULL DEFAULT '',          -- primary agent slug
    draft           TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,                  -- unix millis
    updated_at      INTEGER NOT NULL,                  -- unix millis
    last_read_at    INTEGER,                           -- unix millis, null = unread
    subdir          TEXT NOT NULL DEFAULT '',          -- GitRef.Subdir: working dir relative to project_dir; '' = root
    display_name    TEXT NOT NULL DEFAULT ''           -- GitRef.DisplayName: label; project_dir is root-normalized so not re-derivable
);

CREATE INDEX idx_sessions_external_id ON sessions (external_id);
CREATE INDEX idx_sessions_status ON sessions (status);
CREATE INDEX idx_sessions_visibility ON sessions (visibility);

-- Primary agent catalog cache. Per-repo (project_dir, worktree_id)
-- because opencode/claude config is committed to git and shared
-- across branches. backend keys the cache so opencode and claude
-- each have their own list.
CREATE TABLE primary_agents (
    backend             TEXT NOT NULL,
    project_dir         TEXT NOT NULL DEFAULT '',
    worktree_id         TEXT NOT NULL DEFAULT '',
    primary_agents_json TEXT NOT NULL DEFAULT '[]',
    updated_at          INTEGER NOT NULL,              -- unix millis
    PRIMARY KEY (backend, project_dir, worktree_id)
);
