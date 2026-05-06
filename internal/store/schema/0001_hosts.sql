-- Schema for sqlc type-checking. NOT a migration — production migrations
-- live in store.go's migrate() function (currently up to user_version=16
-- before this PR introduces v17 for the hosts table).
--
-- Mirror the post-migration shape of every sqlc-managed table here.

CREATE TABLE hosts (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL,
    provider    TEXT NOT NULL,
    external_id TEXT NOT NULL,
    hostname    TEXT NOT NULL,
    status      TEXT NOT NULL,
    -- last_url / last_token: provider-edge-specific cache (e.g. Daytona's
    -- preview URL + preview-token). Refreshed on every EnsureHost when
    -- the cached value goes stale (URL rotation across stop/resume).
    last_url    TEXT NOT NULL DEFAULT '',
    last_token  TEXT NOT NULL DEFAULT '',
    -- auth_token: clank-host's own bearer token, checked by the
    -- require-bearer middleware on every HTTP request. Universal
    -- across providers (Daytona stacks it on top of last_token;
    -- Sprites use it as the only auth layer). Stable across
    -- stop/resume — baked into the sandbox/sprite at create time.
    auth_token  TEXT NOT NULL DEFAULT '',
    auto_wake   INTEGER NOT NULL DEFAULT 0,
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL,
    UNIQUE (user_id, provider)
);
