package store

import "fmt"

// migrateV15 collapses the (kind, url, path) GitRef triple into the
// two-column (project_dir, git_remote_url) shape that mirrors the
// flat agent.GitRef{LocalPath, RemoteURL}. Both columns may be set:
// see agent.GitRef godoc for resolution precedence on the host.
//
// On the sessions table:
//   - ADD COLUMN git_remote_url (renamed from git_ref_url).
//   - Re-add COLUMN project_dir (was dropped in v14): when set it
//     carries the absolute path; the host prefers it over cloning.
//   - DROP git_ref_kind, git_ref_path.
//
// On primary_agents we drop and recreate with the new PK
// (backend, host_id, project_dir, git_remote_url). The cache is a pure
// derivation and will be re-populated lazily; no data preservation.
func (s *Store) migrateV15() error {
	cols, err := s.sessionColumns()
	if err != nil {
		return err
	}

	// 1. Re-add project_dir if v14 dropped it.
	if !cols["project_dir"] {
		if _, err := s.db.Exec(`ALTER TABLE sessions ADD COLUMN project_dir TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add project_dir: %w", err)
		}
	}

	// 2. Rename git_ref_url → git_remote_url.
	if cols["git_ref_url"] && !cols["git_remote_url"] {
		if _, err := s.db.Exec(`ALTER TABLE sessions RENAME COLUMN git_ref_url TO git_remote_url`); err != nil {
			return fmt.Errorf("rename git_ref_url: %w", err)
		}
	}

	// 3. Backfill project_dir from git_ref_path for rows with kind='local'.
	if cols["git_ref_kind"] && cols["git_ref_path"] {
		if _, err := s.db.Exec(`
			UPDATE sessions
			SET project_dir = git_ref_path
			WHERE git_ref_kind = 'local' AND git_ref_path != ''
		`); err != nil {
			return fmt.Errorf("backfill project_dir: %w", err)
		}
		// Clear git_remote_url for local rows so the two columns remain
		// mutually exclusive (defensive — should already be empty).
		if _, err := s.db.Exec(`
			UPDATE sessions SET git_remote_url = ''
			WHERE git_ref_kind = 'local'
		`); err != nil {
			return fmt.Errorf("clear remote_url for local rows: %w", err)
		}
	}

	// 4. Drop deprecated columns.
	for _, col := range []string{"git_ref_kind", "git_ref_path"} {
		if !cols[col] {
			continue
		}
		if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE sessions DROP COLUMN %s`, col)); err != nil {
			return fmt.Errorf("drop column %s: %w", col, err)
		}
	}

	// 5. Recreate primary_agents with the new key shape.
	if _, err := s.db.Exec(`
		DROP TABLE IF EXISTS primary_agents;
		CREATE TABLE primary_agents (
			backend             TEXT NOT NULL,
			host_id             TEXT NOT NULL,
			project_dir         TEXT NOT NULL DEFAULT '',
			git_remote_url      TEXT NOT NULL DEFAULT '',
			primary_agents_json TEXT NOT NULL DEFAULT '[]',
			updated_at          DATETIME NOT NULL,
			PRIMARY KEY (backend, host_id, project_dir, git_remote_url)
		);
	`); err != nil {
		return fmt.Errorf("recreate primary_agents: %w", err)
	}

	if _, err := s.db.Exec(`PRAGMA user_version = 15`); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return nil
}
