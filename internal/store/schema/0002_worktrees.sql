-- Schema for sqlc type-checking. NOT a migration — production migrations
-- live in store.go's migrate() function (currently up to user_version=21
-- with these worktrees + checkpoints tables).
--
-- Mirror the post-migration shape exactly. Updates to v21 in store.go
-- must be reflected here.

CREATE TABLE worktrees (
    id                          TEXT PRIMARY KEY,
    user_id                     TEXT NOT NULL,
    display_name                TEXT NOT NULL,
    owner_kind                  TEXT NOT NULL DEFAULT 'laptop',
    owner_id                    TEXT NOT NULL DEFAULT '',
    latest_synced_checkpoint    TEXT NOT NULL DEFAULT '',
    created_at                  DATETIME NOT NULL,
    updated_at                  DATETIME NOT NULL
);
CREATE INDEX worktrees_user_id_idx ON worktrees(user_id);
CREATE INDEX worktrees_owner_idx   ON worktrees(owner_kind, owner_id);

CREATE TABLE checkpoints (
    id                  TEXT PRIMARY KEY,
    worktree_id         TEXT NOT NULL,
    head_commit         TEXT NOT NULL,
    head_ref            TEXT NOT NULL DEFAULT '',
    index_tree          TEXT NOT NULL,
    worktree_tree       TEXT NOT NULL,
    incremental_commit  TEXT NOT NULL,
    created_at          DATETIME NOT NULL,
    created_by          TEXT NOT NULL DEFAULT '',
    uploaded_at         DATETIME
);
CREATE INDEX checkpoints_worktree_idx ON checkpoints(worktree_id, created_at DESC);
