-- Baseline: the full schema as of the goose adoption; plain CREATEs,
-- so a database not created through goose fails loudly here.
--
-- Column documentation lives in ../schema.sql (the declarative source
-- of truth this file was generated from). Later migrations are
-- generated with `make migration`; don't hand-edit applied ones.

-- +goose Up
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
    last_read_at    INTEGER,
    subdir          TEXT NOT NULL DEFAULT '',
    display_name    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_sessions_external_id ON sessions (external_id);
CREATE INDEX idx_sessions_status ON sessions (status);
CREATE INDEX idx_sessions_visibility ON sessions (visibility);

CREATE TABLE primary_agents (
    backend             TEXT NOT NULL,
    project_dir         TEXT NOT NULL DEFAULT '',
    worktree_id         TEXT NOT NULL DEFAULT '',
    primary_agents_json TEXT NOT NULL DEFAULT '[]',
    updated_at          INTEGER NOT NULL,
    PRIMARY KEY (backend, project_dir, worktree_id)
);

-- +goose Down
DROP TABLE IF EXISTS primary_agents;
DROP INDEX IF EXISTS idx_sessions_visibility;
DROP INDEX IF EXISTS idx_sessions_status;
DROP INDEX IF EXISTS idx_sessions_external_id;
DROP TABLE IF EXISTS sessions;
