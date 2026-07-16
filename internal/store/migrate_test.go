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
