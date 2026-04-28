package store

import (
	"database/sql"
	"fmt"
	"time"
)

// SyncedRepo is one row of the cloud-hub's synced_repos table — the
// repos for which we hold a mirror.
type SyncedRepo struct {
	RepoKey    string
	RemoteURL  string
	MirrorPath string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// SyncedBranch is one row of synced_branches — a branch tip we've
// received from a laptop hub and unbundled into the mirror.
type SyncedBranch struct {
	RepoKey   string
	Branch    string
	TipSHA    string
	BaseSHA   string
	UpdatedAt time.Time
}

// UpsertSyncedRepo inserts or updates a synced repo entry. Idempotent.
func (s *Store) UpsertSyncedRepo(repo SyncedRepo) error {
	now := time.Now()
	if repo.CreatedAt.IsZero() {
		repo.CreatedAt = now
	}
	repo.UpdatedAt = now
	_, err := s.db.Exec(`
		INSERT INTO synced_repos (repo_key, remote_url, mirror_path, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repo_key) DO UPDATE SET
			remote_url  = excluded.remote_url,
			mirror_path = excluded.mirror_path,
			updated_at  = excluded.updated_at
	`, repo.RepoKey, repo.RemoteURL, repo.MirrorPath, repo.CreatedAt, repo.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert synced repo %s: %w", repo.RepoKey, err)
	}
	return nil
}

// UpsertSyncedBranch inserts or updates a synced branch entry. The
// repo_key must exist in synced_repos (FK constraint).
func (s *Store) UpsertSyncedBranch(b SyncedBranch) error {
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = time.Now()
	}
	_, err := s.db.Exec(`
		INSERT INTO synced_branches (repo_key, branch, tip_sha, base_sha, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repo_key, branch) DO UPDATE SET
			tip_sha    = excluded.tip_sha,
			base_sha   = excluded.base_sha,
			updated_at = excluded.updated_at
	`, b.RepoKey, b.Branch, b.TipSHA, b.BaseSHA, b.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert synced branch %s/%s: %w", b.RepoKey, b.Branch, err)
	}
	return nil
}

// LoadSyncedRepos returns all synced repos.
func (s *Store) LoadSyncedRepos() ([]SyncedRepo, error) {
	rows, err := s.db.Query(`
		SELECT repo_key, remote_url, mirror_path, created_at, updated_at
		FROM synced_repos
		ORDER BY repo_key
	`)
	if err != nil {
		return nil, fmt.Errorf("query synced repos: %w", err)
	}
	defer rows.Close()
	var out []SyncedRepo
	for rows.Next() {
		var r SyncedRepo
		if err := rows.Scan(&r.RepoKey, &r.RemoteURL, &r.MirrorPath, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan synced repo: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FindSyncedRepo returns the synced repo matching repoKey, or nil if not
// present.
func (s *Store) FindSyncedRepo(repoKey string) (*SyncedRepo, error) {
	var r SyncedRepo
	err := s.db.QueryRow(`
		SELECT repo_key, remote_url, mirror_path, created_at, updated_at
		FROM synced_repos
		WHERE repo_key = ?
	`, repoKey).Scan(&r.RepoKey, &r.RemoteURL, &r.MirrorPath, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find synced repo %s: %w", repoKey, err)
	}
	return &r, nil
}

// LoadSyncedBranches returns all synced branches for a repo.
func (s *Store) LoadSyncedBranches(repoKey string) ([]SyncedBranch, error) {
	rows, err := s.db.Query(`
		SELECT repo_key, branch, tip_sha, base_sha, updated_at
		FROM synced_branches
		WHERE repo_key = ?
		ORDER BY branch
	`, repoKey)
	if err != nil {
		return nil, fmt.Errorf("query synced branches: %w", err)
	}
	defer rows.Close()
	var out []SyncedBranch
	for rows.Next() {
		var b SyncedBranch
		if err := rows.Scan(&b.RepoKey, &b.Branch, &b.TipSHA, &b.BaseSHA, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan synced branch: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// LoadSyncStateTip returns the last-pushed tip SHA for (repoKey, branch),
// or empty string if no record exists. Used by the laptop sync agent to
// avoid re-pushing identical state.
func (s *Store) LoadSyncStateTip(repoKey, branch string) (string, error) {
	var sha string
	err := s.db.QueryRow(`
		SELECT last_pushed_sha FROM sync_state
		WHERE repo_key = ? AND branch = ?
	`, repoKey, branch).Scan(&sha)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load sync state %s/%s: %w", repoKey, branch, err)
	}
	return sha, nil
}

// UpsertSyncStateTip records the SHA most recently pushed for
// (repoKey, branch). Used by the laptop sync agent.
func (s *Store) UpsertSyncStateTip(repoKey, branch, sha string) error {
	_, err := s.db.Exec(`
		INSERT INTO sync_state (repo_key, branch, last_pushed_sha, pushed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(repo_key, branch) DO UPDATE SET
			last_pushed_sha = excluded.last_pushed_sha,
			pushed_at       = excluded.pushed_at
	`, repoKey, branch, sha, time.Now())
	if err != nil {
		return fmt.Errorf("upsert sync state %s/%s: %w", repoKey, branch, err)
	}
	return nil
}
