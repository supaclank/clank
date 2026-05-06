-- name: GetHostByUser :one
SELECT * FROM hosts
WHERE user_id = ? AND provider = ?;

-- name: GetHostByID :one
SELECT * FROM hosts
WHERE id = ?;

-- name: UpsertHost :exec
INSERT INTO hosts (
    id, user_id, provider, external_id, hostname, status,
    last_url, last_token, auth_token, auto_wake, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, provider) DO UPDATE SET
    external_id = excluded.external_id,
    hostname    = excluded.hostname,
    status      = excluded.status,
    last_url    = excluded.last_url,
    last_token  = excluded.last_token,
    auth_token   = excluded.auth_token,
    auto_wake   = excluded.auto_wake,
    updated_at  = excluded.updated_at;

-- name: DeleteHostByID :exec
DELETE FROM hosts WHERE id = ?;

-- name: DeleteHostByUser :exec
DELETE FROM hosts WHERE user_id = ? AND provider = ?;
