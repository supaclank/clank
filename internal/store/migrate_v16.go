package store

// Migration v16 adds the tables that back hub-to-hub git sync.
//
//   - synced_repos / synced_branches: cloud-hub state. One row per repo
//     (keyed by agent.RepoKey hash) and one per (repo, branch). Populated
//     when a laptop hub pushes a bundle; consumed when a sandbox needs
//     to know what's been synced.
//
//   - sync_state: laptop-hub state. Tracks the last-pushed tip SHA per
//     (repo, branch) so the agent only re-bundles when local state
//     actually moved.
//
// Same schema lives in both hubs because they share a binary; each hub
// only writes to the tables that match its role.
func (s *Store) migrateV16() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS synced_repos (
			repo_key      TEXT PRIMARY KEY,
			remote_url    TEXT NOT NULL,
			mirror_path   TEXT NOT NULL,
			created_at    DATETIME NOT NULL,
			updated_at    DATETIME NOT NULL
		);
		CREATE TABLE IF NOT EXISTS synced_branches (
			repo_key   TEXT NOT NULL,
			branch     TEXT NOT NULL,
			tip_sha    TEXT NOT NULL,
			base_sha   TEXT NOT NULL DEFAULT '',
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (repo_key, branch),
			FOREIGN KEY (repo_key) REFERENCES synced_repos(repo_key) ON DELETE CASCADE
		);
		CREATE TABLE IF NOT EXISTS sync_state (
			repo_key        TEXT NOT NULL,
			branch          TEXT NOT NULL,
			last_pushed_sha TEXT NOT NULL,
			pushed_at       DATETIME NOT NULL,
			PRIMARY KEY (repo_key, branch)
		);
		PRAGMA user_version = 16;
	`)
	return err
}
