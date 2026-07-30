// Package store provides SQLite-backed persistence for the provisioner's
// host registry (the `hosts` table) and push-notification devices (the
// `devices` table). Session metadata lives in the host's own store at
// internal/host/store; worktrees live on the host filesystem.
//
// schema.sql is the declarative source of truth (read by sqlc); the
// goose migrations in migrations/ are what actually run at Open(). See
// the `make migration` target for evolving the schema.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/acksell/clank/internal/sqlmigrate"
	"github.com/acksell/clank/internal/store/sqlitedb"
	"github.com/acksell/clank/pkg/provisioner/hoststore"

	// Pure-Go SQLite driver (no CGo).
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsDir embed.FS

// *Store satisfies the HostStore contract — keep the assertion close to
// the type definition so refactors can't silently break the interface.
var _ hoststore.HostStore = (*Store)(nil)

// Store wraps a SQLite database for persisting the host registry and
// device state. Tables are accessed via the sqlc-generated Queries in q;
// db stays around for the few statements sqlc's SQLite grammar can't
// express (see CASProviderMeta).
type Store struct {
	db *sql.DB
	q  *sqlitedb.Queries
}

// Open opens (or creates) a SQLite database at dbPath and applies any
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

	migrations, err := fs.Sub(migrationsDir, "migrations")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open embedded migrations: %w", err)
	}
	if err := sqlmigrate.Up(context.Background(), db, migrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", dbPath, err)
	}

	return &Store{db: db, q: sqlitedb.New(db)}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
