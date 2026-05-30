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
