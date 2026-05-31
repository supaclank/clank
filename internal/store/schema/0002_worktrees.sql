-- Schema for sqlc type-checking. NOT a migration — production migrations
-- live in store.go's migrate() function. Mirror the post-migration shape
-- exactly; every store.go migration that touches these tables must be
-- reflected here.

CREATE TABLE worktrees (
    id                          TEXT PRIMARY KEY,
    user_id                     TEXT NOT NULL,
    display_name                TEXT NOT NULL,
    -- origin_repo identifies the repo this worktree was created from
    -- (e.g. "acme/api" derived from the git remote, or the local dir
    -- basename when no remote is configured). Used by clients to group
    -- worktrees by repo in their pickers/sidebars. Set at registration;
    -- never updated. Empty for rows registered before this column existed.
    origin_repo                 TEXT NOT NULL DEFAULT '',
    latest_synced_checkpoint    TEXT NOT NULL DEFAULT '',
    created_at                  DATETIME NOT NULL,
    updated_at                  DATETIME NOT NULL
);
CREATE INDEX worktrees_user_id_idx ON worktrees(user_id);

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

-- head_bundles indexes the content-addressed head bundles (committed
-- history), shared across a user's checkpoints/worktrees. tip_sha is the
-- HEAD commit the bundle ends at; base_sha is the commit it was built
-- from ("" = a full bundle with no prerequisite). The (base_sha → tip)
-- links form the chain the server walks from a checkpoint's HEAD back to
-- a full baseline.
CREATE TABLE head_bundles (
    user_id    TEXT NOT NULL,
    tip_sha    TEXT NOT NULL,
    base_sha   TEXT NOT NULL DEFAULT '',
    blob_key   TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    PRIMARY KEY (user_id, tip_sha)
);
