-- name: UpsertDevice :exec
INSERT INTO devices (user_id, push_token, platform, created_at, last_seen_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (user_id, push_token) DO UPDATE SET
    platform     = excluded.platform,
    last_seen_at = excluded.last_seen_at;

-- name: ListDevicesByUser :many
SELECT * FROM devices
WHERE user_id = ?
ORDER BY last_seen_at DESC;

-- name: DeleteDevice :exec
DELETE FROM devices
WHERE user_id = ? AND push_token = ?;

-- name: DeleteDeviceByPushToken :exec
-- Used by the dispatcher when Expo returns DeviceNotRegistered for a
-- token; the user_id is irrelevant, the token itself is dead.
DELETE FROM devices
WHERE push_token = ?;
