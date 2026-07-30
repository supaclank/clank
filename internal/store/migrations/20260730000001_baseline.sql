-- Baseline: the full schema as of the goose adoption (squashes the
-- retired PRAGMA user_version chain, v1–v33). IF NOT EXISTS makes it a
-- no-op on databases created by that chain, so they adopt in place —
-- goose records the row and takes over from here.
--
-- Column documentation lives in ../schema.sql (the declarative source
-- of truth this file was generated from). Later migrations are
-- generated with `make migration`; don't hand-edit applied ones.

-- +goose Up
CREATE TABLE IF NOT EXISTS hosts (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL,
    provider    TEXT NOT NULL,
    external_id TEXT NOT NULL,
    hostname    TEXT NOT NULL,
    status      TEXT NOT NULL,
    last_url    TEXT NOT NULL DEFAULT '',
    last_token  TEXT NOT NULL DEFAULT '',
    auth_token  TEXT NOT NULL DEFAULT '',
    notifier_token TEXT NOT NULL DEFAULT '',
    auto_wake   INTEGER NOT NULL DEFAULT 0,
    provider_meta TEXT NOT NULL DEFAULT '{}',
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS hosts_user_id_provider_idx
ON hosts (user_id, provider);

CREATE UNIQUE INDEX IF NOT EXISTS hosts_notifier_token_idx
ON hosts (notifier_token)
WHERE notifier_token != '';

CREATE TABLE IF NOT EXISTS devices (
    user_id      TEXT NOT NULL,
    push_token   TEXT NOT NULL,
    platform     TEXT NOT NULL CHECK (platform IN ('ios', 'android')),
    created_at   DATETIME NOT NULL,
    last_seen_at DATETIME NOT NULL,
    PRIMARY KEY (user_id, push_token)
);

CREATE INDEX IF NOT EXISTS devices_user_id_idx ON devices (user_id);

CREATE INDEX IF NOT EXISTS devices_push_token_idx ON devices (push_token);

-- +goose Down
DROP INDEX IF EXISTS devices_push_token_idx;
DROP INDEX IF EXISTS devices_user_id_idx;
DROP TABLE IF EXISTS devices;
DROP INDEX IF EXISTS hosts_notifier_token_idx;
DROP INDEX IF EXISTS hosts_user_id_provider_idx;
DROP TABLE IF EXISTS hosts;
