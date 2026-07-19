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
		`ALTER TABLE threads ADD COLUMN intent TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN last_read_entry_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE threads ADD COLUMN origin_project TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN departed INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE agents ADD COLUMN session_created INTEGER NOT NULL DEFAULT 0`,
	}
	for _, ddl := range alters {
		if _, err := db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}

	// One-time entry-ID watermark backfill, run on every migrate() since
	// "column was just added" isn't detectable after the ALTER above: for any
	// agent whose watermark is still at its zero-value default and who has a
	// wall-clock read timestamp, initialize last_read_entry_id from the
	// highest entry visible as of that timestamp. Idempotent — once set
	// (non-zero) or with no prior read, the WHERE clause stops matching.
	if _, err := db.Exec(`
UPDATE agents SET last_read_entry_id =
  COALESCE((SELECT MAX(e.id) FROM entries e WHERE e.created_at <= agents.last_read_at), 0)
WHERE last_read_entry_id = 0 AND last_read_at > 0`); err != nil {
		return err
	}

	// One-time origin_project backfill (iteration-4 orphan-thread fix): a
	// thread row created before this migration has origin_project='' since
	// the ALTER above defaults it that way. For any such row whose sender
	// still resolves in the CURRENT roster, stamp its project; a sender that
	// no longer resolves (deregistered, or never registered) leaves the row
	// '' — station's "(unassigned)" bucket is the fallback for those.
	// Idempotent: only rows still at '' are touched, so a re-run after a
	// previous backfill (or after CreateThread starts stamping new rows
	// itself) is a no-op over already-stamped rows.
	if _, err := db.Exec(`
UPDATE threads SET origin_project = (SELECT project FROM agents WHERE alias = threads.from_agent)
WHERE origin_project = ''
  AND EXISTS (SELECT 1 FROM agents WHERE alias = threads.from_agent)`); err != nil {
		return err
	}
	return nil
}

// DB exposes the underlying handle (tests + store methods).
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }
