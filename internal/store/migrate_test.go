package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrateAddsTargetToV04Events builds the exact v0.4 events schema by
// hand (current Open's CREATE IF NOT EXISTS would not alter it), inserts a
// row, then opens the store twice: the ALTER must apply once, idempotently,
// and the old row must read back with target ”.
func TestMigrateAddsTargetToV04Events(t *testing.T) {
	dir := t.TempDir() // no sockets here; plain file DB is fine
	dbPath := filepath.Join(dir, "bus.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL, kind TEXT NOT NULL,
		agent TEXT NOT NULL DEFAULT '', thread_id INTEGER NOT NULL DEFAULT 0,
		count INTEGER NOT NULL DEFAULT 0, detail TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO events (ts, kind, agent, thread_id, count, detail)
		VALUES (1, 'notify', 'api', 7, 1, 'lit')`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	for i := 0; i < 2; i++ { // reopen twice: migration must be idempotent
		s, err := Open(dbPath)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		evs, err := s.Events(EventQuery{Backlog: true, Limit: 10})
		if err != nil {
			t.Fatalf("events %d: %v", i, err)
		}
		if len(evs) != 1 || evs[0].Target != "" || evs[0].Agent != "api" {
			t.Fatalf("v0.4 row after migrate: %+v", evs)
		}
		_ = s.Close()
	}
}

// TestMigrateBackfillsEntryWatermarkFromV05Schema builds the exact v0.5
// schema by hand: agents already has last_read_at (an earlier migration) but
// not last_read_entry_id; threads has no intent column. Agent "a" last read
// at ts=100; entry 1 (ts=50) predates that read, entry 2 (ts=200) postdates
// it. Opening twice must backfill last_read_entry_id to exactly entry 1's id
// (idempotently — the second open must not move it), leaving entry 2 unread.
func TestMigrateBackfillsEntryWatermarkFromV05Schema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "bus.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE agents (
		alias TEXT PRIMARY KEY, role TEXT NOT NULL DEFAULT '', model_type TEXT NOT NULL DEFAULT '',
		socket_path TEXT NOT NULL DEFAULT '', pane_id TEXT NOT NULL DEFAULT '',
		session_name TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '',
		project TEXT NOT NULL DEFAULT '', label TEXT NOT NULL DEFAULT '', label_manual INTEGER NOT NULL DEFAULT 0,
		last_read_at INTEGER NOT NULL DEFAULT 0, registered_at INTEGER NOT NULL, last_seen INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE threads (
		id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, from_agent TEXT NOT NULL,
		to_kind TEXT NOT NULL, to_target TEXT NOT NULL DEFAULT '', subject TEXT NOT NULL DEFAULT '',
		ref TEXT NOT NULL DEFAULT '', status TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE entries (
		id INTEGER PRIMARY KEY AUTOINCREMENT, thread_id INTEGER NOT NULL, from_agent TEXT NOT NULL,
		body TEXT NOT NULL DEFAULT '', status_change TEXT, created_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_by TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL, kind TEXT NOT NULL,
		agent TEXT NOT NULL DEFAULT '', target TEXT NOT NULL DEFAULT '', thread_id INTEGER NOT NULL DEFAULT 0,
		count INTEGER NOT NULL DEFAULT 0, detail TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}

	if _, err := raw.Exec(`INSERT INTO threads (id, kind, from_agent, to_kind, to_target, created_at, updated_at)
		VALUES (1, 'message', 'x', 'agent', 'a', 50, 200)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO entries (id, thread_id, from_agent, created_at) VALUES (1, 1, 'x', 50)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO entries (id, thread_id, from_agent, created_at) VALUES (2, 1, 'x', 200)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO agents (alias, last_read_at, registered_at, last_seen) VALUES ('a', 100, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	for i := 0; i < 2; i++ { // reopen twice: backfill must be idempotent
		s, err := Open(dbPath)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		got, ok, err := s.GetAgent("a")
		if err != nil || !ok {
			t.Fatalf("GetAgent %d: ok=%v err=%v", i, ok, err)
		}
		if got.LastReadEntryID != 1 {
			t.Fatalf("open %d: watermark backfill = %d, want 1 (only entry 1 predates last_read_at=100)", i, got.LastReadEntryID)
		}
		n, err := s.UnreadCount("a")
		if err != nil {
			t.Fatalf("UnreadCount %d: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("open %d: unread = %d, want 1 (entry 2 postdates the backfilled watermark)", i, n)
		}
		_ = s.Close()
	}
}

// TestMigrateBackfillsOriginProjectFromCurrentRoster is the iteration-4
// orphan-thread fix's own migration test (spec queue item 4c): builds the
// exact pre-origin_project threads schema by hand (current Open's CREATE
// TABLE IF NOT EXISTS would not add the column to an existing table),
// inserts a thread from a sender who's still in the CURRENT roster and one
// from a sender who no longer resolves, then opens the store twice: the
// ALTER must apply once, idempotently, and the backfill must stamp only the
// resolvable sender's row, leaving the unresolvable one at ”. A third row
// inserted with origin_project ALREADY set (simulating a post-migration
// CreateThread stamp) must survive a later re-open untouched — proving the
// backfill only ever fills ”-rows, never overwrites an existing stamp even
// when the roster could compute a different value for it.
func TestMigrateBackfillsOriginProjectFromCurrentRoster(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "bus.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE agents (
		alias TEXT PRIMARY KEY, role TEXT NOT NULL DEFAULT '', model_type TEXT NOT NULL DEFAULT '',
		socket_path TEXT NOT NULL DEFAULT '', pane_id TEXT NOT NULL DEFAULT '',
		session_name TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '',
		project TEXT NOT NULL DEFAULT '', label TEXT NOT NULL DEFAULT '', label_manual INTEGER NOT NULL DEFAULT 0,
		last_read_at INTEGER NOT NULL DEFAULT 0, last_read_entry_id INTEGER NOT NULL DEFAULT 0,
		registered_at INTEGER NOT NULL, last_seen INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE threads (
		id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, from_agent TEXT NOT NULL,
		to_kind TEXT NOT NULL, to_target TEXT NOT NULL DEFAULT '', subject TEXT NOT NULL DEFAULT '',
		ref TEXT NOT NULL DEFAULT '', status TEXT, intent TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE entries (
		id INTEGER PRIMARY KEY AUTOINCREMENT, thread_id INTEGER NOT NULL, from_agent TEXT NOT NULL,
		body TEXT NOT NULL DEFAULT '', status_change TEXT, created_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_by TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL, kind TEXT NOT NULL,
		agent TEXT NOT NULL DEFAULT '', target TEXT NOT NULL DEFAULT '', thread_id INTEGER NOT NULL DEFAULT 0,
		count INTEGER NOT NULL DEFAULT 0, detail TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}

	if _, err := raw.Exec(`INSERT INTO agents (alias, project, registered_at, last_seen) VALUES ('resolvable', 'alpha', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO threads (id, kind, from_agent, to_kind, to_target, created_at, updated_at)
		VALUES (1, 'message', 'resolvable', 'agent', 'x', 10, 10)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO threads (id, kind, from_agent, to_kind, to_target, created_at, updated_at)
		VALUES (2, 'message', 'ghost', 'agent', 'x', 20, 20)`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	queryOrigin := func(t *testing.T, s *Store, id int64) string {
		t.Helper()
		var op string
		if err := s.DB().QueryRow(`SELECT origin_project FROM threads WHERE id=?`, id).Scan(&op); err != nil {
			t.Fatalf("query origin_project for thread %d: %v", id, err)
		}
		return op
	}

	for i := 0; i < 2; i++ { // reopen twice: migration + backfill must be idempotent
		s, err := Open(dbPath)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if got := queryOrigin(t, s, 1); got != "alpha" {
			t.Fatalf("open %d: thread 1 (resolvable sender) origin_project = %q, want alpha", i, got)
		}
		if got := queryOrigin(t, s, 2); got != "" {
			t.Fatalf("open %d: thread 2 (unresolvable sender) origin_project = %q, want '' (unassigned)", i, got)
		}
		_ = s.Close()
	}

	// A row already stamped (simulating a post-migration CreateThread) must
	// never be overwritten, even though the roster could compute a different
	// value for its sender.
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO threads (id, kind, from_agent, to_kind, to_target, created_at, updated_at, origin_project)
		VALUES (3, 'message', 'resolvable', 'agent', 'x', 30, 30, 'custom-stamp')`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if got := queryOrigin(t, s2, 3); got != "custom-stamp" {
		t.Fatalf("thread 3's pre-stamped origin_project = %q, want it untouched (custom-stamp)", got)
	}
}
