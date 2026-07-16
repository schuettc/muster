// Package store is muster's SQLite persistence layer.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

//go:embed schema.sql
var schemaSQL string

// Store wraps the SQLite database.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the database at dbPath, enables WAL, and
// applies the schema idempotently.
func Open(dbPath string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1) // serialize writers; WAL still allows concurrent readers via separate conns later
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate applies additive column migrations to pre-existing databases. Each
// ALTER is guarded so a re-run (column already present) is a no-op.
func migrate(db *sql.DB) error {
	alters := []string{
		`ALTER TABLE agents ADD COLUMN project TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN label TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN label_manual INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE agents ADD COLUMN last_read_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE events ADD COLUMN target TEXT NOT NULL DEFAULT ''`,
	}
	for _, ddl := range alters {
		if _, err := db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

// DB exposes the underlying handle (tests + store methods).
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }
