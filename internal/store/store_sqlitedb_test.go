package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The store opens through tools-common/sqlitedb, adopting the unversioned
// database every muster before this one wrote (user_version 0, tables
// present). These pin that adoption and what the module adds.

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// legacyDB writes a database the way muster 0.22.1's Open left it: no
// user_version, no foreign keys, schema.sql plus every column ALTER, and rows.
func legacyDB(t *testing.T, withAlters bool) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bus.db")
	raw, err := sql.Open("sqlite", "file:"+p+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	if withAlters {
		for _, ddl := range columnAlters {
			if _, err := raw.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
				t.Fatal(err)
			}
		}
		if _, err := raw.Exec(`INSERT INTO agents (alias, registered_at, last_seen, project, harness_session_id) VALUES ('keeper', 1, 2, 'p', 'h-1')`); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := raw.Exec(`INSERT INTO agents (alias, registered_at, last_seen) VALUES ('keeper', 1, 2)`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO threads (kind, from_agent, to_kind, to_target, subject, created_at, updated_at) VALUES ('message', 'keeper', 'agent', 'other', 's', 10, 10)`); err != nil {
		t.Fatal(err)
	}
	return p
}

func rowSums(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT group_concat(alias || '|' || project || '|' || last_read_entry_id || '|' || harness_session_id, ';') FROM agents`,
		`SELECT group_concat(id || '|' || subject || '|' || origin_project, ';') FROM threads`,
	} {
		var s sql.NullString
		if err := db.QueryRow(q).Scan(&s); err != nil {
			t.Fatal(err)
		}
		b.WriteString(s.String + "\n")
	}
	return b.String()
}

func TestOpenAdoptsLiveUnversionedDatabase(t *testing.T) {
	p := legacyDB(t, true)
	s, err := Open(p)
	if err != nil {
		t.Fatalf("Open on a muster 0.22.1 database: %v", err)
	}
	defer func() { _ = s.Close() }()
	if v := userVersion(t, s.DB()); v != 1 {
		t.Fatalf("user_version %d, want 1", v)
	}
	sums := rowSums(t, s.DB())
	if !strings.Contains(sums, "keeper|p|0|h-1") || !strings.Contains(sums, "|s|") {
		t.Fatalf("rows not intact: %q", sums)
	}
}

func TestOpenAddsMissingColumns(t *testing.T) {
	s, err := Open(legacyDB(t, false))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for _, col := range []string{"harness_session_id", "transcript_path", "wake"} {
		table := "agents"
		if col == "wake" {
			table = "threads"
		}
		var n int
		if err := s.DB().QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s.%s missing after Open (%d, %v)", table, col, n, err)
		}
	}
}

func TestReopenIsNoOp(t *testing.T) {
	p := legacyDB(t, true)
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	first := rowSums(t, s.DB())
	_ = s.Close()
	s, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if v := userVersion(t, s.DB()); v != 1 {
		t.Fatalf("user_version %d after reopen, want 1", v)
	}
	if again := rowSums(t, s.DB()); again != first {
		t.Fatalf("reopen changed rows:\n%s\nvs\n%s", first, again)
	}
}

func TestStoreFilesArePrivate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bus.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.DB().Exec(`INSERT INTO agents (alias, registered_at, last_seen) VALUES ('a', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p, p + "-wal", p + "-shm"} {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode %v, want 0600", filepath.Base(f), got)
		}
	}
}

// TestForeignKeysStayOff: muster's schema was never written for enforcement;
// adopting the module must not turn it on.
func TestForeignKeysStayOff(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var on int
	if err := s.DB().QueryRow("PRAGMA foreign_keys").Scan(&on); err != nil || on != 0 {
		t.Fatalf("foreign_keys %d (%v), want 0", on, err)
	}
}

func TestConcurrentFirstOpenStore(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bus.db")
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release together, so the opens actually race
			s, err := Open(p)
			if err != nil {
				errs <- err
				return
			}
			errs <- s.Close()
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent first open: %v", err)
		}
	}
}
