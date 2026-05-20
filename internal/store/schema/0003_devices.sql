-- Schema for sqlc type-checking. NOT a migration — production migrations
-- live in store.go's migrate() function. Mirror the post-migration shape
-- here.

CREATE TABLE devices (
    user_id      TEXT NOT NULL,
    push_token   TEXT NOT NULL,
    -- platform is the OS the push_token belongs to. Constrained at the
    -- schema layer so a typo can't accumulate a third "iOS"/"Ios" bucket
    -- silently.
    platform     TEXT NOT NULL CHECK (platform IN ('ios', 'android')),
    created_at   DATETIME NOT NULL,
    last_seen_at DATETIME NOT NULL,
    PRIMARY KEY (user_id, push_token)
);

CREATE INDEX devices_user_id_idx ON devices(user_id);
