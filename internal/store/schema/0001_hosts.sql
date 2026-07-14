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
    updated_at  DATETIME NOT NULL,
    UNIQUE (user_id, provider)
);

-- The dispatcher uses notifier_token as a bearer-lookup key, so two
-- non-empty rows sharing one would route a host's notifications to
-- whichever user the lookup happens to return. UNIQUE rules that out;
-- the WHERE clause exempts the empty-string default so legacy rows
-- coexist.
CREATE UNIQUE INDEX hosts_notifier_token_idx
ON hosts(notifier_token)
WHERE notifier_token != '';
