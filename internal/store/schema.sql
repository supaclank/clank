-- Declarative schema for the provisioner-side database (clank.db):
-- the single source of truth for its desired shape.
--
--   * sqlc type-checks queries/ against this file (sqlc.yaml points here).
--   * Atlas diffs this file against migrations/ to generate new goose
--     migration files — see the `make migration` target. Never edit the
--     database by hand-writing DDL elsewhere; change this file and diff.
--
-- This file is never executed against a real database; migrations/ is
-- what runs (applied by goose at Open()).

CREATE TABLE hosts (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL,
    provider    TEXT NOT NULL,
    external_id TEXT NOT NULL,
    hostname    TEXT NOT NULL,
    status      TEXT NOT NULL,
    -- last_url / last_token: provider-edge-specific cache (e.g. a
    -- preview URL + preview-token). Refreshed on every EnsureHost when
    -- the cached value goes stale (URL rotation across stop/resume).
    last_url    TEXT NOT NULL DEFAULT '',
    last_token  TEXT NOT NULL DEFAULT '',
    -- auth_token: clank-host's own bearer token, checked by the
    -- require-bearer middleware on every HTTP request. Universal
    -- across providers: it layers on top of any provider-edge
    -- preview-token stored in last_token; Sprites use it as the only
    -- auth layer. Stable across stop/resume — baked into the
    -- sandbox/sprite at create time.
    auth_token  TEXT NOT NULL DEFAULT '',
    -- notifier_token: the per-host bearer credential clank-host sends
    -- *outbound* on notifier webhook calls back to clankd. The
    -- dispatcher looks up by this column to resolve "which user does
    -- this notification belong to". Empty = host can't deliver
    -- notifications (e.g. legacy rows; populated lazily on next
    -- provisioner-mediated start). Counterpart to auth_token (which
    -- guards INCOMING traffic) — pure mirror, no shared semantics.
    notifier_token TEXT NOT NULL DEFAULT '',
    auto_wake   INTEGER NOT NULL DEFAULT 0,
    -- provider_meta: provider-owned JSON key→value bag for resource
    -- handles the provider can't derive (e.g. flymachines' server-
    -- assigned volume ID). Written ONLY via the CASProviderMeta
    -- compare-and-set (UpsertHost never touches it) so concurrent
    -- provisioner instances can serialize resource claims on it.
    provider_meta TEXT NOT NULL DEFAULT '{}',
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL
);

-- One host per user per provider, enforced at the DB layer even if app
-- code has a bug. A named index rather than an inline UNIQUE constraint:
-- SQLite backs inline constraints with unnamed auto-indexes, which
-- Atlas's inspection can't round-trip stably (spurious drop/create
-- diffs). Upserts with ON CONFLICT (user_id, provider) match it.
CREATE UNIQUE INDEX hosts_user_id_provider_idx
ON hosts (user_id, provider);

-- The dispatcher uses notifier_token as a bearer-lookup key, so two
-- non-empty rows sharing one would route a host's notifications to
-- whichever user the lookup happens to return. UNIQUE rules that out;
-- the WHERE clause exempts the empty-string default so legacy rows
-- coexist.
CREATE UNIQUE INDEX hosts_notifier_token_idx
ON hosts (notifier_token)
WHERE notifier_token != '';

-- devices: per-user push delivery addresses. The mobile client
-- registers an Expo push token here on login and deregisters on
-- logout; the dispatcher fans notifications out to every row matching
-- the resolved user.
CREATE TABLE devices (
    user_id      TEXT NOT NULL,
    push_token   TEXT NOT NULL,
    -- platform is the OS the push_token belongs to, gated to ios or
    -- android so a typo can't silently accumulate a third "iOS"/"Ios"
    -- bucket. The Expo Push API treats both the same on the wire, but
    -- the column lets us emit platform-targeted variants later (Live
    -- Activities on iOS, Channels on Android) without a schema change.
    platform     TEXT NOT NULL CHECK (platform IN ('ios', 'android')),
    created_at   DATETIME NOT NULL,
    last_seen_at DATETIME NOT NULL,
    PRIMARY KEY (user_id, push_token)
);

CREATE INDEX devices_user_id_idx ON devices (user_id);

-- DeleteDeviceByPushToken filters on push_token alone (the
-- DeviceNotRegistered purge path), where the PK on (user_id,
-- push_token) can't be used. Index keeps that delete from degrading
-- into a table scan once the row count grows.
CREATE INDEX devices_push_token_idx ON devices (push_token);
