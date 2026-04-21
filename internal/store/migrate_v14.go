package store

import "fmt"

// dropMigrationV14 implements the v14 migration: drop the now-unused
// project_dir, project_name, and worktree_dir columns from sessions.
//
// Lives in its own file because legacy databases (from experimental
// branches) carry FTS5 search infrastructure on the sessions table that
// references project_name. SQLite refuses ALTER TABLE DROP COLUMN when a
// trigger references the column, so we must tear down the FTS objects
// first. Some branches also already removed project_dir, so we drop
// columns conditionally.
func (s *Store) dropMigrationV14() error {
	// Tear down any leftover FTS5 search infrastructure that references
	// the columns we're about to drop. These objects are not created by
	// the current codebase but exist in databases migrated through other
	// branches.
	ftsCleanup := []string{
		`DROP TRIGGER IF EXISTS sessions_fts_insert`,
		`DROP TRIGGER IF EXISTS sessions_fts_delete`,
		`DROP TRIGGER IF EXISTS sessions_fts_update`,
		`DROP TABLE IF EXISTS sessions_fts`,
	}
	for _, stmt := range ftsCleanup {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("fts cleanup %q: %w", stmt, err)
		}
	}

	// Drop columns conditionally — DROP COLUMN has no IF EXISTS form.
	cols, err := s.sessionColumns()
	if err != nil {
		return err
	}
	drops := []string{"project_dir", "project_name", "worktree_dir"}
	for _, col := range drops {
		if !cols[col] {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE sessions DROP COLUMN %s", col)
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("drop column %s: %w", col, err)
		}
	}

	if _, err := s.db.Exec(`PRAGMA user_version = 14`); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return nil
}

// sessionColumns returns the set of column names currently present on
// the sessions table.
func (s *Store) sessionColumns() (map[string]bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		return nil, fmt.Errorf("table_info(sessions): %w", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("scan table_info: %w", err)
		}
		cols[name] = true
	}
	return cols, rows.Err()
}
