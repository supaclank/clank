-- name: GetSession :one
SELECT * FROM sessions WHERE id = ?;

-- name: FindSessionByExternalID :one
SELECT * FROM sessions WHERE external_id = ? LIMIT 1;

-- name: ListSessions :many
SELECT * FROM sessions ORDER BY updated_at DESC;

-- name: ListSessionsByWorktree :many
-- Used by session-sync to enumerate sessions in a worktree for export.
-- worktree_id is the clank-sync ULID; cross-machine stable identity.
SELECT * FROM sessions WHERE worktree_id = ? ORDER BY updated_at DESC;

-- name: SearchSessions :many
-- Filtered list. Empty filter values are treated as "no filter" by
-- the query (NULL/empty matches anything for that column). Visibility
-- filter is exact match.
SELECT * FROM sessions
WHERE
    (CAST(@q AS TEXT) = '' OR title LIKE '%' || @q || '%' OR prompt LIKE '%' || @q || '%' OR draft LIKE '%' || @q || '%' OR project_dir LIKE '%' || @q || '%')
    AND (CAST(@visibility AS TEXT) = '' OR visibility = @visibility)
    AND (@since IS NULL OR updated_at >= @since)
    AND (@until IS NULL OR updated_at <= @until)
ORDER BY updated_at DESC
LIMIT @lim;

-- name: UpsertSession :exec
INSERT INTO sessions (
    id, external_id, backend, status, visibility, follow_up,
    project_dir, worktree_id, worktree_branch, prompt, title,
    ticket_id, agent, draft, created_at, updated_at, last_read_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    external_id     = excluded.external_id,
    backend         = excluded.backend,
    status          = excluded.status,
    visibility      = excluded.visibility,
    follow_up       = excluded.follow_up,
    project_dir     = excluded.project_dir,
    worktree_id      = excluded.worktree_id,
    worktree_branch = excluded.worktree_branch,
    prompt          = excluded.prompt,
    title           = excluded.title,
    ticket_id       = excluded.ticket_id,
    agent           = excluded.agent,
    draft           = excluded.draft,
    updated_at      = excluded.updated_at,
    last_read_at    = excluded.last_read_at;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = ?;

-- name: ListPrimaryAgents :one
SELECT primary_agents_json FROM primary_agents
WHERE backend = ? AND project_dir = ? AND worktree_id = ?;

-- name: UpsertPrimaryAgents :exec
INSERT INTO primary_agents (
    backend, project_dir, worktree_id, primary_agents_json, updated_at
) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (backend, project_dir, worktree_id) DO UPDATE SET
    primary_agents_json = excluded.primary_agents_json,
    updated_at          = excluded.updated_at;
