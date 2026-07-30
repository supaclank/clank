// Package sqlmigrate applies embedded goose migrations to a SQLite
// database. Every store (internal/store, internal/host/store) funnels
// through here so the repo has exactly one migration mechanism.
//
// Migrations are goose-format .sql files generated from each store's
// declarative schema.sql via `make migration` (Atlas computes the diff);
// this package only ever applies them.
package sqlmigrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
)

// Up applies every pending migration to db, in order. migrations must be
// a filesystem whose root contains the goose-format .sql files. Safe to
// call on every open: applied versions are tracked per-migration in the
// goose_db_version table, so reruns are no-ops.
func Up(ctx context.Context, db *sql.DB, migrations fs.FS) error {
	provider, err := goose.NewProvider(database.DialectSQLite3, db, migrations)
	if err != nil {
		return fmt.Errorf("init goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
