-- name: GetHostByUser :one
SELECT * FROM hosts
WHERE user_id = ? AND provider = ?;

-- name: GetHostByID :one
SELECT * FROM hosts
WHERE id = ?;

-- name: GetHostByNotifierToken :one
SELECT * FROM hosts
WHERE notifier_token = ? AND notifier_token != '';

-- name: UpsertHost :exec
INSERT INTO hosts (
    id, user_id, provider, external_id, hostname, status,
    last_url, last_token, auth_token, notifier_token, auto_wake, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, provider) DO UPDATE SET
    external_id    = excluded.external_id,
    hostname       = excluded.hostname,
    status         = excluded.status,
    last_url       = excluded.last_url,
    last_token     = excluded.last_token,
    auth_token     = excluded.auth_token,
    notifier_token = excluded.notifier_token,
    auto_wake      = excluded.auto_wake,
    updated_at     = excluded.updated_at;

-- name: InsertHostIfAbsent :execrows
-- The cross-instance claim: exactly one concurrent caller inserts;
-- everyone else reads the winner's row back. provider_meta is left to
-- its default (CASProviderMeta is its only writer). ASCII only here:
-- sqlc slices query text by byte offset but counts chars, so a
-- multi-byte character in a comment corrupts every later query.
INSERT INTO hosts (
    id, user_id, provider, external_id, hostname, status,
    last_url, last_token, auth_token, notifier_token, auto_wake, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, provider) DO NOTHING;

-- name: DeleteHostByID :exec
DELETE FROM hosts WHERE id = ?;

-- name: DeleteHostByUser :exec
DELETE FROM hosts WHERE user_id = ? AND provider = ?;
