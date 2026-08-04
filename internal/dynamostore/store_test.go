package dynamostore

import (
	"context"
	"os"
	"strings"
	"testing"
)

// newTestStore returns a Store against a DynamoDB reachable at
// $MUSTER_DDB_ENDPOINT, or skips.
//
// The skip is deliberate rather than a failure: `just verify` is the gate CI
// and every developer runs, and it must stay fast and free of a container
// dependency. `just verify-dynamo` is the recipe that guarantees the endpoint
// is up, and CI runs these against a DynamoDB Local service container.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv(EndpointEnv) == "" {
		t.Skipf("%s unset; run `just verify-dynamo`", EndpointEnv)
	}
	s, err := Open(context.Background(), testTableName(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.DropTable(context.Background()); err != nil {
			t.Errorf("DropTable: %v", err)
		}
	})
	return s
}

// testTableName derives a unique table per test so tests never share state.
// Subtest names contain '/', which DynamoDB rejects in a table name.
func testTableName(t *testing.T) string {
	t.Helper()
	name := strings.NewReplacer("/", "-", " ", "-", "#", "-").Replace(t.Name())
	return TestTablePrefix + name
}

func TestOpenCreatesTable(t *testing.T) {
	s := newTestStore(t)
	ok, err := s.TableExists(context.Background())
	if err != nil {
		t.Fatalf("TableExists: %v", err)
	}
	if !ok {
		t.Fatal("Open did not create the table")
	}
}

func TestEnsureTableIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	// Open already created it; a second EnsureTable must be a no-op rather
	// than a ResourceInUseException surfacing to the caller.
	if err := s.EnsureTable(context.Background()); err != nil {
		t.Fatalf("second EnsureTable: %v", err)
	}
}

// TestDropTableRefusesNonTestTable pins the guard that keeps this test-only
// helper from ever deleting real data. It needs no endpoint, so it runs in
// `just verify` alongside everything else.
func TestDropTableRefusesNonTestTable(t *testing.T) {
	s := &Store{table: "muster-production"}
	err := s.DropTable(context.Background())
	if err == nil {
		t.Fatal("DropTable must refuse a table outside the test prefix")
	}
	if !strings.Contains(err.Error(), "test-only") {
		t.Fatalf("error should explain the guard, got: %v", err)
	}
}

func TestRcptPartitioning(t *testing.T) {
	// Broadcasts share one partition regardless of target; addressed threads
	// are partitioned by kind and target. Unread math depends on an entry
	// landing in the same partition its recipient queries.
	tests := []struct {
		toKind, toTarget, want string
	}{
		{"agent", "backend-2", "RCPT#agent#backend-2"},
		{"role", "worker", "RCPT#role#worker"},
		{"broadcast", "", "RCPT#broadcast"},
		{"broadcast", "ignored", "RCPT#broadcast"},
	}
	for _, tc := range tests {
		if got := rcpt(tc.toKind, tc.toTarget); got != tc.want {
			t.Errorf("rcpt(%q, %q) = %q, want %q", tc.toKind, tc.toTarget, got, tc.want)
		}
	}
}

func TestKeyBuildersAreDisjoint(t *testing.T) {
	// One table holds every entity, so a collision between two key builders
	// would silently overwrite unrelated data.
	keys := map[string]string{
		"thread":  pkThread(1),
		"agent":   pkAgent("1"),
		"kv":      pkKV("1"),
		"event":   pkEvent(1),
		"counter": pkCounter("1"),
		"idem":    pkIdem("1"),
	}
	seen := make(map[string]string, len(keys))
	for name, key := range keys {
		if prev, dup := seen[key]; dup {
			t.Errorf("key collision: %s and %s both produce %q", prev, name, key)
		}
		seen[key] = name
	}
}
