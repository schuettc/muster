// Package store is muster's SQLite persistence layer.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/schuettc/tools-common/sqlitedb"
)

//go:embed schema.sql
var schemaSQL string

// Store wraps the SQLite database.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the database at dbPath through
// tools-common/sqlitedb: WAL, a 5 s busy timeout, one connection, files 0600,
// and schema changes as user_version steps.
//
// Every muster before this one wrote an UNVERSIONED database (user_version 0,
// tables present). Step 1 adopts it: the embedded schema and the guarded
// column ALTERs, idempotent by construction, and they must stay so. Every
// future schema change is a new step 2, 3, ..., never an edit to step 1.
// Foreign keys stay off: the schema was never written for enforcement.
//
// repairOnOpen then runs on EVERY open, as it always has: its backfills are
// idempotent repairs, not one-time migrations (RegisterAgent stores a socket
// path as given, and the per-open canonicalization is what matches it).
func Open(dbPath string) (*Store, error) {
	ctx := context.Background()
	d, err := sqlitedb.Open(ctx, dbPath, sqlitedb.Options{
		Migrations:       []sqlitedb.Step{adoptLegacySchema},
		NoForeignKeys:    true,
		AdoptUnversioned: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := d.Tx(ctx, func(tx *sql.Tx) error { return repairOnOpen(ctx, tx) }); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("repair: %w", err)
	}
	return &Store{db: d.DB}, nil
}

// columnAlters are the additive column migrations every pre-sqlitedb muster
// replayed on each open. Each one is guarded (a "duplicate column name" error
// means it already ran), so replaying the list is a no-op on a current
// database and adds only the missing columns on an older one.
var columnAlters = []string{
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
	`ALTER TABLE agents ADD COLUMN device_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agents ADD COLUMN harness_session_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agents ADD COLUMN superseded_by TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agents ADD COLUMN device_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agents ADD COLUMN transcript_path TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agents ADD COLUMN last_read_standing_entry_id INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE threads ADD COLUMN standing INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE threads ADD COLUMN standing_key TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE threads ADD COLUMN standing_retracted INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE threads ADD COLUMN wake INTEGER NOT NULL DEFAULT 0`,
}

// adoptLegacySchema is step 1 (user_version 0 -> 1): the schema and additive
// column migrations muster applied on every open before it adopted sqlitedb.
func adoptLegacySchema(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	for _, ddl := range columnAlters {
		if _, err := tx.ExecContext(ctx, ddl); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

// repairOnOpen holds the idempotent backfills that run on every open.
func repairOnOpen(ctx context.Context, tx *sql.Tx) error {

	// One-time entry-ID watermark backfill, run on every migrate() since
	// "column was just added" isn't detectable after the ALTER above: for any
	// agent whose watermark is still at its zero-value default and who has a
	// wall-clock read timestamp, initialize last_read_entry_id from the
	// highest entry visible as of that timestamp. Idempotent — once set
	// (non-zero) or with no prior read, the WHERE clause stops matching.
	if _, err := tx.ExecContext(ctx, `
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
	if _, err := tx.ExecContext(ctx, `
UPDATE threads SET origin_project = (SELECT project FROM agents WHERE alias = threads.from_agent)
WHERE origin_project = ''
  AND EXISTS (SELECT 1 FROM agents WHERE alias = threads.from_agent)`); err != nil {
		return err
	}

	// Canonicalize socket paths (tmux/OS symlinks, e.g. macOS /tmp ->
	// /private/tmp) so tuple matching against a freshly-captured, already
	// canonical socket path (tmuxenv.SocketFromEnv) doesn't miss existing
	// rows recorded before this normalization existed. Idempotent: an
	// already-canonical path round-trips through EvalSymlinks unchanged, and
	// an unresolvable path (dead socket, no longer on disk) is left as-is.
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT socket_path FROM agents WHERE socket_path != ''`)
	if err != nil {
		return err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return err
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, p := range paths {
		r, err := filepath.EvalSymlinks(p)
		if err != nil || r == p {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET socket_path = ? WHERE socket_path = ?`, r, p); err != nil {
			return err
		}
	}
	return nil
}

// DB exposes the underlying handle (tests + store methods).
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }
