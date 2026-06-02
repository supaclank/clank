-- name: GetWorktreeByID :one
SELECT * FROM worktrees
WHERE id = ?;

-- name: ListWorktreesByUser :many
SELECT * FROM worktrees
WHERE user_id = ?
ORDER BY updated_at DESC;

-- name: InsertWorktree :exec
INSERT INTO worktrees (
    id, user_id, display_name, origin_repo,
    latest_synced_checkpoint, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: UpdateWorktreePointer :exec
UPDATE worktrees
SET latest_synced_checkpoint = ?, updated_at = ?
WHERE id = ?;

-- name: UpdateWorktreeMaterialization :exec
UPDATE worktrees
SET materialized_checkpoint_id = ?, sync_state = ?,
    sync_conflict_local_head = ?, sync_conflict_remote_head = ?,
    updated_at = ?
WHERE id = ?;

-- name: GetHeadBundle :one
SELECT * FROM head_bundles
WHERE user_id = ? AND tip_sha = ?;

-- name: InsertHeadBundle :exec
-- Idempotent: a tip's first stored bundle wins, so re-pushing a HEAD the
-- server already has (already_stored) keeps the original base_sha link.
INSERT OR IGNORE INTO head_bundles (
    user_id, tip_sha, base_sha, blob_key, created_at
) VALUES (?, ?, ?, ?, ?);

-- name: DeleteWorktree :exec
DELETE FROM worktrees WHERE id = ?;

-- name: GetCheckpointByID :one
SELECT * FROM checkpoints
WHERE id = ?;

-- name: ListCheckpointsByWorktree :many
SELECT * FROM checkpoints
WHERE worktree_id = ?
ORDER BY created_at DESC
LIMIT ?;

-- name: InsertCheckpoint :exec
INSERT INTO checkpoints (
    id, worktree_id, head_commit, head_ref,
    index_tree, worktree_tree, incremental_commit,
    created_at, created_by
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: MarkCheckpointUploaded :exec
UPDATE checkpoints
SET uploaded_at = ?
WHERE id = ?;
