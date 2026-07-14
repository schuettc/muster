package store

import (
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
