package store

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db := filepath.Join(t.TempDir(), "bus.db")
	s, err := Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenCreatesSchema(t *testing.T) {
	s := newTestStore(t)
	var n int
	err := s.DB().QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('agents','threads','entries','kv')`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 4 {
		t.Fatalf("expected 4 tables, got %d", n)
	}
}

func TestOpenMigrationIsIdempotent(t *testing.T) {
	db := filepath.Join(t.TempDir(), "bus.db")

	s1, err := Open(db)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Re-opening the same (already-migrated) DB re-runs migrate(); the
	// ADD COLUMN statements must be no-ops, not errors.
	s2, err := Open(db)
	if err != nil {
		t.Fatalf("second Open (re-migrate) failed: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	// The table must still be fully usable after the repeated migration.
	if err := s2.RegisterAgent(Agent{Alias: "a", Project: "p", Label: "l", LabelManual: true}); err != nil {
		t.Fatalf("RegisterAgent after re-migrate: %v", err)
	}
	got, ok, err := s2.GetAgent("a")
	if err != nil || !ok {
		t.Fatalf("GetAgent after re-migrate: ok=%v err=%v", ok, err)
	}
	if got.Project != "p" || got.Label != "l" || !got.LabelManual {
		t.Fatalf("round-trip after re-migrate=%+v", got)
	}
}

func TestSchemaHasStandingColumns(t *testing.T) {
	s := newTestStore(t)
	for _, c := range []struct{ table, col string }{
		{"agents", "last_read_standing_entry_id"},
		{"threads", "standing"},
		{"threads", "standing_key"},
		{"threads", "standing_retracted"},
	} {
		var n int
		if err := s.DB().QueryRow(
			`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, c.table, c.col,
		).Scan(&n); err != nil {
			t.Fatalf("pragma %s: %v", c.table, err)
		}
		if n != 1 {
			t.Fatalf("%s.%s missing (found %d)", c.table, c.col, n)
		}
	}
}

func TestMigrateNormalizesSocketPaths(t *testing.T) {
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	realDir := filepath.Join(home, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(realDir, "s")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(home, "bus.db")

	s, err := Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.RegisterAgent(Agent{Alias: "a", SocketPath: filepath.Join(link, "s"), SessionID: "$1"}); err != nil {
		t.Fatalf("RegisterAgent a: %v", err)
	}
	if err := s.RegisterAgent(Agent{Alias: "b", SocketPath: "/nonexistent/s", SessionID: "$1"}); err != nil {
		t.Fatalf("RegisterAgent b: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(db) // migrate runs again
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	a, _, _ := s.GetAgent("a")
	b, _, _ := s.GetAgent("b")
	if a.SocketPath != sock {
		t.Fatalf("a = %q, want %q", a.SocketPath, sock)
	}
	if b.SocketPath != "/nonexistent/s" {
		t.Fatalf("unresolvable path must be left alone: %q", b.SocketPath)
	}
}
