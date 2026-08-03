// Package store is the host's local SQLite for session metadata and
// the primary-agent cache. Owned by clank-host (the per-user host
// process), opened at the path specified by --data-dir.
//
// Compared to internal/store (provisioner-side, hosts table) this
// store lives on a different machine in the cloud topology — the
// host runs inside a sprite/sandbox, the provisioner runs in the
// gateway/clankd process. Same Go pattern, different file, different
// owner.
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

	"github.com/supaclank/clank/internal/host/store/hostsqlitedb"
	"github.com/supaclank/clank/internal/sqlmigrate"

	// Pure-Go SQLite driver (no CGo).
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsDir embed.FS

// Store wraps the host's SQLite database. The high-level methods
// in sessions.go delegate to the sqlc-generated Queries.
type Store struct {
	db *sql.DB
	q  *hostsqlitedb.Queries
}

// Open opens (or creates) the host's SQLite database at dbPath and
// applies any pending schema migrations.
func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}

	// SQLite supports a single concurrent writer. Limiting the pool
	// to one connection keeps PRAGMAs consistent and serialises
	// writes through Go's sql.DB.
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("exec %q: %w", pragma, err)
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

	return &Store{db: db, q: hostsqlitedb.New(db)}, nil
}

// Close releases the underlying database connection.
func (s *Store) Close() error { return s.db.Close() }

// withQ exposes the generated Queries to callers in the same package
// (e.g. sessions.go). External packages should use the high-level
// methods on Store.
func (s *Store) withQ(_ context.Context) *hostsqlitedb.Queries {
	return s.q
}
