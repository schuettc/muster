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
