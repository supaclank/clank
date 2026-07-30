-- Baseline: the full schema as of the goose adoption; plain CREATEs,
-- so a database not created through goose fails loudly here.
--
-- Column documentation lives in ../schema.sql (the declarative source
-- of truth this file was generated from). Later migrations are
-- generated with `make migration`; don't hand-edit applied ones.

-- +goose Up
CREATE TABLE hosts (
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

CREATE UNIQUE INDEX hosts_user_id_provider_idx
ON hosts (user_id, provider);

CREATE UNIQUE INDEX hosts_notifier_token_idx
ON hosts (notifier_token)
WHERE notifier_token != '';

CREATE TABLE devices (
    user_id      TEXT NOT NULL,
    push_token   TEXT NOT NULL,
    platform     TEXT NOT NULL CHECK (platform IN ('ios', 'android')),
    created_at   DATETIME NOT NULL,
    last_seen_at DATETIME NOT NULL,
    PRIMARY KEY (user_id, push_token)
);

CREATE INDEX devices_user_id_idx ON devices (user_id);

CREATE INDEX devices_push_token_idx ON devices (push_token);

-- +goose Down
DROP INDEX IF EXISTS devices_push_token_idx;
DROP INDEX IF EXISTS devices_user_id_idx;
DROP TABLE IF EXISTS devices;
DROP INDEX IF EXISTS hosts_notifier_token_idx;
DROP INDEX IF EXISTS hosts_user_id_provider_idx;
DROP TABLE IF EXISTS hosts;
