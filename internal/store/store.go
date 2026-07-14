// Package store provides SQLite-backed persistence for the provisioner's
// host registry (the `hosts` table) and push-notification devices (the
// `devices` table). Session metadata lives in the host's own store at
// internal/host/store; worktrees live on the host filesystem (the
// checkpoint-sync worktrees/checkpoints/head_bundles tables were
// dropped in migration v32).
package store

import (
	"database/sql"
	"fmt"

	"github.com/acksell/clank/internal/store/sqlitedb"
	"github.com/acksell/clank/pkg/provisioner/hoststore"

	// Pure-Go SQLite driver (no CGo).
	_ "modernc.org/sqlite"
)

// *Store satisfies the HostStore contract — keep the assertion close to
// the type definition so refactors can't silently break the interface.
var _ hoststore.HostStore = (*Store)(nil)

// Store wraps a SQLite database for persisting session metadata and host
// registry state. New tables are accessed via the sqlc-generated Queries
// in q; legacy tables (sessions, primary_agents, sync_state, etc.) still
// use raw SQL on db.
type Store struct {
	db *sql.DB
	q  *sqlitedb.Queries
}

// Open opens (or creates) a SQLite database at dbPath and runs any
// pending schema migrations. The caller must call Close when done.
func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}

	// SQLite only supports a single concurrent writer. Limiting the pool
	// to one connection ensures all PRAGMAs apply to every query (they are
	// per-connection state) and serialises writes through Go's sql.DB,
	// avoiding SQLITE_BUSY errors from pool-spawned connections that would
	// otherwise lack the busy_timeout PRAGMA.
	db.SetMaxOpenConns(1)

	// Configure SQLite for concurrent access and durability.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("exec %q: %w", p, err)
		}
	}

	s := &Store{db: db, q: sqlitedb.New(db)}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate applies schema migrations using PRAGMA user_version.
func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	if version < 1 {
		_, err := s.db.Exec(`
			CREATE TABLE IF NOT EXISTS sessions (
				id            TEXT PRIMARY KEY,
				external_id   TEXT NOT NULL DEFAULT '',
				backend       TEXT NOT NULL,
				status        TEXT NOT NULL DEFAULT 'idle',
				visibility    TEXT NOT NULL DEFAULT '',
				follow_up     INTEGER NOT NULL DEFAULT 0,
				project_dir   TEXT NOT NULL,
				project_name  TEXT NOT NULL,
				prompt        TEXT NOT NULL DEFAULT '',
				title         TEXT NOT NULL DEFAULT '',
				ticket_id     TEXT NOT NULL DEFAULT '',
				agent         TEXT NOT NULL DEFAULT '',
				draft         TEXT NOT NULL DEFAULT '',
				created_at    DATETIME NOT NULL,
				updated_at    DATETIME NOT NULL,
				last_read_at  DATETIME
			);
			CREATE INDEX IF NOT EXISTS idx_sessions_external_id ON sessions(external_id);
			PRAGMA user_version = 1;
		`)
		if err != nil {
			return fmt.Errorf("migration v1: %w", err)
		}
		version = 1
	}

	if version < 2 {
		_, err := s.db.Exec(`
			CREATE TABLE IF NOT EXISTS agents (
				backend      TEXT NOT NULL,
				project_dir  TEXT NOT NULL,
				agents_json  TEXT NOT NULL DEFAULT '[]',
				updated_at   DATETIME NOT NULL,
				PRIMARY KEY (backend, project_dir)
			);
			PRAGMA user_version = 2;
		`)
		if err != nil {
			return fmt.Errorf("migration v2: %w", err)
		}
		version = 2
	}

	if version < 3 {
		_, err := s.db.Exec(`
			ALTER TABLE agents RENAME TO primary_agents;
			ALTER TABLE primary_agents RENAME COLUMN agents_json TO primary_agents_json;
			PRAGMA user_version = 3;
		`)
		if err != nil {
			return fmt.Errorf("migration v3: %w", err)
		}
		version = 3
	}

	if version < 9 {
		_, err := s.db.Exec(`
			ALTER TABLE sessions ADD COLUMN branch TEXT NOT NULL DEFAULT '';
			ALTER TABLE sessions ADD COLUMN worktree_dir TEXT NOT NULL DEFAULT '';
			PRAGMA user_version = 9;
		`)
		if err != nil {
			return fmt.Errorf("migration v9: %w", err)
		}
		version = 9
	}

	if version < 10 {
		_, err := s.db.Exec(`
			ALTER TABLE sessions RENAME COLUMN branch TO worktree_branch;
			PRAGMA user_version = 10;
		`)
		if err != nil {
			return fmt.Errorf("migration v10: %w", err)
		}
		version = 10
	}
	if version < 11 {
		_, err := s.db.Exec(`
			ALTER TABLE sessions ADD COLUMN host_id TEXT NOT NULL DEFAULT 'local';
			ALTER TABLE sessions ADD COLUMN repo_remote_url TEXT NOT NULL DEFAULT '';
			PRAGMA user_version = 11;
		`)
		if err != nil {
			return fmt.Errorf("migration v11: %w", err)
		}
		version = 11
	}
	if version < 12 {
		_, err := s.db.Exec(`
			ALTER TABLE sessions RENAME COLUMN repo_remote_url TO git_ref_url;
			ALTER TABLE sessions ADD COLUMN git_ref_kind TEXT NOT NULL DEFAULT '';
			ALTER TABLE sessions ADD COLUMN git_ref_path TEXT NOT NULL DEFAULT '';
			UPDATE sessions SET git_ref_kind = 'remote' WHERE git_ref_url != '';
			PRAGMA user_version = 12;
		`)
		if err != nil {
			return fmt.Errorf("migration v12: %w", err)
		}
		version = 12
	}
	if version < 13 {
		_, err := s.db.Exec(`
			DROP TABLE IF EXISTS primary_agents;
			CREATE TABLE primary_agents (
				backend             TEXT NOT NULL,
				host_id             TEXT NOT NULL,
				git_ref_kind        TEXT NOT NULL,
				git_ref_url         TEXT NOT NULL DEFAULT '',
				git_ref_path        TEXT NOT NULL DEFAULT '',
				primary_agents_json TEXT NOT NULL DEFAULT '[]',
				updated_at          DATETIME NOT NULL,
				PRIMARY KEY (backend, host_id, git_ref_kind, git_ref_url, git_ref_path)
			);
			PRAGMA user_version = 13;
		`)
		if err != nil {
			return fmt.Errorf("migration v13: %w", err)
		}
		version = 13
	}
	if version < 14 {
		if err := s.dropMigrationV14(); err != nil {
			return fmt.Errorf("migration v14: %w", err)
		}
		version = 14
	}
	if version < 15 {
		// Step 8 of hub_host_refactor_code_review.md §7.8: collapse
		// (kind, url, path) → (project_dir, git_remote_url) on both
		// sessions and primary_agents. The new GitRef pointer-shape
		// (Local *LocalRef | Remote *RemoteRef) maps cleanly to two
		// optional columns: at most one is non-empty.
		if err := s.migrateV15(); err != nil {
			return fmt.Errorf("migration v15: %w", err)
		}
		version = 15
	}
	if version < 16 {
		// Hub-to-hub sync tables. See migrate_v16.go for shape rationale.
		if err := s.migrateV16(); err != nil {
			return fmt.Errorf("migration v16: %w", err)
		}
		version = 16
	}
	if version < 17 {
		// hosts: per-user persistent compute (Fly Sprite, Fly Machine,
		// k8s pod, …). UNIQUE (user_id, provider) enforces the
		// one-host-per-user-per-provider invariant at the DB layer
		// even if app code has a bug.
		// Schema mirrored in internal/store/schema/0001_hosts.sql for
		// sqlc; keep them in sync.
		_, err := s.db.Exec(`
			CREATE TABLE hosts (
				id          TEXT PRIMARY KEY,
				user_id     TEXT NOT NULL,
				provider    TEXT NOT NULL,
				external_id TEXT NOT NULL,
				hostname    TEXT NOT NULL,
				status      TEXT NOT NULL,
				last_url    TEXT NOT NULL DEFAULT '',
				last_token  TEXT NOT NULL DEFAULT '',
				auto_wake   INTEGER NOT NULL DEFAULT 0,
				created_at  DATETIME NOT NULL,
				updated_at  DATETIME NOT NULL,
				UNIQUE (user_id, provider)
			);
			PRAGMA user_version = 17;
		`)
		if err != nil {
			return fmt.Errorf("migration v17: %w", err)
		}
		version = 17
	}
	if version < 18 {
		// auth_token: clank-host's bearer-token, checked by the
		// require-bearer middleware on every HTTP request. Universal
		// across providers: it layers on top of any provider-edge
		// preview-token stored in last_token; Sprites use it as the
		// only auth layer. See PR 2 of the persistent-host roadmap.
		//
		// (Renamed from cap_token to auth_token in v19 — see below.
		// This migration uses the post-rename name so installs that
		// jump straight from v17 to v19 don't need the rename step.)
		_, err := s.db.Exec(`
			ALTER TABLE hosts ADD COLUMN auth_token TEXT NOT NULL DEFAULT '';
			PRAGMA user_version = 18;
		`)
		if err != nil {
			return fmt.Errorf("migration v18: %w", err)
		}
		version = 18
	}
	if version < 19 {
		// Rename auth_token's predecessor (cap_token) for installs
		// that ran an earlier draft of v18. SQLite raises "duplicate
		// column" on the ALTER below if cap_token doesn't exist; we
		// look for it first and skip the rename when it's already
		// auth_token.
		var exists int
		if err := s.db.QueryRow(`
			SELECT COUNT(*) FROM pragma_table_info('hosts') WHERE name = 'cap_token'
		`).Scan(&exists); err != nil {
			return fmt.Errorf("migration v19: probe for legacy column: %w", err)
		}
		if exists > 0 {
			if _, err := s.db.Exec(`ALTER TABLE hosts RENAME COLUMN cap_token TO auth_token;`); err != nil {
				return fmt.Errorf("migration v19: rename cap_token: %w", err)
			}
		}
		if _, err := s.db.Exec(`PRAGMA user_version = 19;`); err != nil {
			return fmt.Errorf("migration v19: bump version: %w", err)
		}
		version = 19
	}
	if version < 20 {
		// PR 3 deletes the hub. Sessions, primary_agents, and sync_state
		// were hub-owned tables; session metadata now lives in the
		// host's own SQLite (internal/host/store) and the hub-to-hub
		// sync mirror is gone. Drop the orphaned tables so clank.db
		// shrinks to provisioner state (just `hosts`).
		_, err := s.db.Exec(`
			DROP TABLE IF EXISTS sessions;
			DROP TABLE IF EXISTS primary_agents;
			DROP TABLE IF EXISTS sync_state;
			DROP TABLE IF EXISTS synced_repos;
			DROP TABLE IF EXISTS synced_branches;
			PRAGMA user_version = 20;
		`)
		if err != nil {
			return fmt.Errorf("migration v20: %w", err)
		}
		version = 20
	}
	if version < 21 {
		// Sync re-architecture: worktrees + checkpoints backed the
		// checkpoint-sync object-storage substrate (deleted; tables
		// dropped again in v32).
		// worktrees: per-user persistent unit of sync ownership.
		// owner_kind/owner_id track which actor (laptop device vs.
		// sandbox sprite) currently holds write authority.
		// checkpoints: per-push manifest pointer. Bundle bytes live in
		// object storage; the row is metadata only. uploaded_at stays
		// NULL until /v1/checkpoints/<id>/commit confirms upload.
		_, err := s.db.Exec(`
			CREATE TABLE worktrees (
				id                          TEXT PRIMARY KEY,
				user_id                     TEXT NOT NULL,
				display_name                TEXT NOT NULL,
				owner_kind                  TEXT NOT NULL DEFAULT 'local',
				owner_id                    TEXT NOT NULL DEFAULT '',
				latest_synced_checkpoint    TEXT NOT NULL DEFAULT '',
				created_at                  DATETIME NOT NULL,
				updated_at                  DATETIME NOT NULL
			);
			CREATE INDEX worktrees_user_id_idx ON worktrees(user_id);
			CREATE INDEX worktrees_owner_idx   ON worktrees(owner_kind, owner_id);

			-- TODO(coderabbit): worktree_id has no FK back to worktrees(id),
			-- so DeleteWorktree leaves orphan checkpoints rows. Next schema
			-- bump should add FOREIGN KEY (worktree_id) REFERENCES
			-- worktrees(id) ON DELETE CASCADE.
			-- https://github.com/Acksell/clank/pull/15#discussion_r3214891044
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

			PRAGMA user_version = 21;
		`)
		if err != nil {
			return fmt.Errorf("migration v21: %w", err)
		}
		version = 21
	}
	if version < 22 {
		// Earlier dev installs created worktrees.owner_kind with the
		// values 'laptop' and 'sprite'. Renamed to 'local' and 'remote'
		// to match the user-facing CLI verbs and to drop fly.io's
		// "sprite" jargon from the wire-format. Convert in place.
		_, err := s.db.Exec(`
			UPDATE worktrees SET owner_kind = 'local'  WHERE owner_kind = 'laptop';
			UPDATE worktrees SET owner_kind = 'remote' WHERE owner_kind = 'sprite';
			PRAGMA user_version = 22;
		`)
		if err != nil {
			return fmt.Errorf("migration v22: %w", err)
		}
		version = 22
	}
	if version < 23 {
		// notifier_token: per-host bearer the dispatcher uses to map an
		// inbound notification webhook back to its (host_id, user_id).
		// NULL-able via empty-string default — legacy rows take the
		// laptop default (no notifications) until the next provisioner
		// cold-create mints one.
		_, err := s.db.Exec(`
			ALTER TABLE hosts ADD COLUMN notifier_token TEXT NOT NULL DEFAULT '';
			CREATE UNIQUE INDEX hosts_notifier_token_idx ON hosts(notifier_token) WHERE notifier_token != '';
			PRAGMA user_version = 23;
		`)
		if err != nil {
			return fmt.Errorf("migration v23: %w", err)
		}
		version = 23
	}
	if version < 24 {
		// devices: per-user push delivery addresses. The mobile client
		// registers an Expo push token here on login and deregisters on
		// logout; the dispatcher fans notifications out to every row
		// matching the resolved user. Platform column is gated to ios
		// or android — the Expo Push API treats both the same on the
		// wire (it's an Expo Push Token), but the column lets us emit
		// platform-targeted variants later (Live Activities on iOS,
		// Channels on Android) without a schema change.
		_, err := s.db.Exec(`
			CREATE TABLE devices (
				user_id      TEXT NOT NULL,
				push_token   TEXT NOT NULL,
				platform     TEXT NOT NULL CHECK (platform IN ('ios', 'android')),
				created_at   DATETIME NOT NULL,
				last_seen_at DATETIME NOT NULL,
				PRIMARY KEY (user_id, push_token)
			);
			CREATE INDEX devices_user_id_idx ON devices(user_id);
			-- DeleteDeviceByPushToken filters on push_token alone (the
			-- DeviceNotRegistered purge path), where the PK on
			-- (user_id, push_token) can't be used. Index keeps that
			-- delete from degrading into a table scan once the row
			-- count grows.
			CREATE INDEX devices_push_token_idx ON devices(push_token);
			PRAGMA user_version = 24;
		`)
		if err != nil {
			return fmt.Errorf("migration v24: %w", err)
		}
		version = 24
	}
	if version < 25 {
		// origin_repo on worktrees: identifies the repo this worktree was
		// created from (e.g. "acme/api" derived from the git remote, or
		// the local dir basename when no remote is configured). Clients
		// (mobile picker, TUI sidebar) group worktrees by this so users
		// don't see a flat list of unrelated ULIDs. Empty default preserves
		// existing rows — those land under "Unknown repo" on grouping
		// clients until a future PR adds a one-shot backfill.
		_, err := s.db.Exec(`
			ALTER TABLE worktrees ADD COLUMN origin_repo TEXT NOT NULL DEFAULT '';
			PRAGMA user_version = 25;
		`)
		if err != nil {
			return fmt.Errorf("migration v25: %w", err)
		}
		version = 25
	}
	if version < 26 {
		// Drop worktree ownership. owner_kind/owner_id backed the
		// gateway's per-worktree local-vs-remote session router — a role
		// we cut. Sync is now asymmetric (laptop→remote autopush,
		// explicit `clank pull`) with drift guarded by git preconditions,
		// not a distributed ownership lock. The owner index must go first:
		// SQLite refuses DROP COLUMN on an indexed column.
		_, err := s.db.Exec(`
			DROP INDEX IF EXISTS worktrees_owner_idx;
			ALTER TABLE worktrees DROP COLUMN owner_kind;
			ALTER TABLE worktrees DROP COLUMN owner_id;
			PRAGMA user_version = 26;
		`)
		if err != nil {
			return fmt.Errorf("migration v26: %w", err)
		}
		version = 26
	}
	if version < 27 {
		// head_bundles: content-addressed head-bundle index for the
		// incremental sync chain. tip_sha = the HEAD commit a bundle ends
		// at; base_sha = the commit it's built from ("" = full baseline).
		// Lets the server skip re-uploads (already-stored tips), send only
		// new commits since a known base, and walk tip→base back to a full
		// baseline on download. Schema mirrored in
		// internal/store/schema/0002_worktrees.sql for sqlc.
		_, err := s.db.Exec(`
			CREATE TABLE head_bundles (
				user_id    TEXT NOT NULL,
				tip_sha    TEXT NOT NULL,
				base_sha   TEXT NOT NULL DEFAULT '',
				blob_key   TEXT NOT NULL,
				created_at DATETIME NOT NULL,
				PRIMARY KEY (user_id, tip_sha)
			);
			PRAGMA user_version = 27;
		`)
		if err != nil {
			return fmt.Errorf("migration v27: %w", err)
		}
		version = 27
	}
	if version < 28 {
		// Autosync materialization tracking. latest_synced_checkpoint
		// records what the laptop PUSHED; these columns record what the
		// sprite has MATERIALIZED (a display cache the gateway refreshes
		// from each apply outcome — see pkg/gateway/sync.go). sync_state is
		// the last outcome: "" (unknown) | up_to_date | behind | conflict |
		// busy. The two conflict-head columns name the diverged commits for
		// the mobile resolution UI; set only when sync_state = conflict.
		// Schema mirrored in internal/store/schema/0002_worktrees.sql.
		_, err := s.db.Exec(`
			ALTER TABLE worktrees ADD COLUMN materialized_checkpoint_id TEXT NOT NULL DEFAULT '';
			ALTER TABLE worktrees ADD COLUMN sync_state TEXT NOT NULL DEFAULT '';
			ALTER TABLE worktrees ADD COLUMN sync_conflict_local_head TEXT NOT NULL DEFAULT '';
			ALTER TABLE worktrees ADD COLUMN sync_conflict_remote_head TEXT NOT NULL DEFAULT '';
			PRAGMA user_version = 28;
		`)
		if err != nil {
			return fmt.Errorf("migration v28: %w", err)
		}
		version = 28
	}
	if version < 29 {
		// sessions_synced_hash: the content-digest of the session set the
		// sprite last imported for this worktree
		// (checkpoint.SessionManifest.ContentDigest). Session blobs upload
		// straight to object storage with no checkpoint bump or commit
		// callback, so the gateway can't be notified of a session-only push —
		// it compares this against the live manifest digest on each autosync
		// and re-imports only on a change (pkg/gateway/sync.go). Empty default
		// ⇒ the next autosync re-imports. Schema mirrored in
		// internal/store/schema/0002_worktrees.sql.
		_, err := s.db.Exec(`
			ALTER TABLE worktrees ADD COLUMN sessions_synced_hash TEXT NOT NULL DEFAULT '';
			PRAGMA user_version = 29;
		`)
		if err != nil {
			return fmt.Errorf("migration v29: %w", err)
		}
		version = 29
	}
	if version < 30 {
		// sessions_content_digest: the content-digest of the session set this
		// checkpoint's manifest describes (checkpoint.SessionManifest.
		// ContentDigest), persisted at presign time. Lets autosync skip the S3
		// manifest fetch when the sprite already holds this exact set — the
		// gateway compares it against the worktree's sessions_synced_hash
		// instead of fetching+parsing the manifest. Empty ⇒ the authoritative
		// fetch (old rows, code-only checkpoints, or a client that didn't send
		// it). Schema mirrored in internal/store/schema/0002_worktrees.sql.
		_, err := s.db.Exec(`
			ALTER TABLE checkpoints ADD COLUMN sessions_content_digest TEXT NOT NULL DEFAULT '';
			PRAGMA user_version = 30;
		`)
		if err != nil {
			return fmt.Errorf("migration v30: %w", err)
		}
		version = 30
	}
	if version < 31 {
		// materialized_host_id: the HostRef.HostID a worktree was last
		// materialized onto. autosync's early-exit trusts its recorded
		// materialization (materialized_checkpoint_id / sessions_synced_hash)
		// only when this still matches the current host generation — a cold
		// reprovision wipes ~/work and mints a new id, which forces a full re-
		// materialize. Empty default ⇒ never trusted until the next apply
		// records it. Schema mirrored in internal/store/schema/0002_worktrees.sql.
		_, err := s.db.Exec(`
			ALTER TABLE worktrees ADD COLUMN materialized_host_id TEXT NOT NULL DEFAULT '';
			PRAGMA user_version = 31;
		`)
		if err != nil {
			return fmt.Errorf("migration v31: %w", err)
		}
		version = 31
	}
	if version < 32 {
		// Checkpoint-sync is deleted: the repo-first model (bare
		// canonicals + linked worktrees on the host; GitHub as the
		// laptop↔sprite bridge) replaced it wholesale. The host
		// filesystem is the worktree registry now, so the gateway-side
		// worktrees/checkpoints/head_bundles tables have no readers or
		// writers left. Drop them; hosts + devices stay.
		_, err := s.db.Exec(`
			DROP TABLE IF EXISTS worktrees;
			DROP TABLE IF EXISTS checkpoints;
			DROP TABLE IF EXISTS head_bundles;
			PRAGMA user_version = 32;
		`)
		if err != nil {
			return fmt.Errorf("migration v32: %w", err)
		}
		version = 32
	}
	if version < 33 {
		// provider_meta: provider-owned resource handles the provider
		// can't derive (e.g. flymachines' server-assigned volume ID).
		// Written only via the CASProviderMeta compare-and-set so
		// concurrent provisioner instances serialize resource claims on
		// it. Mirrored in internal/store/schema/0001_hosts.sql.
		_, err := s.db.Exec(`
			ALTER TABLE hosts ADD COLUMN provider_meta TEXT NOT NULL DEFAULT '{}';
			PRAGMA user_version = 33;
		`)
		if err != nil {
			return fmt.Errorf("migration v33: %w", err)
		}
		version = 33
	}
	_ = version // suppress unused warning after last migration

	return nil
}
