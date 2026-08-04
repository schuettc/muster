package dynamostore

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/schuettc/muster/internal/store"
)

// newTestStore returns a Store against a DynamoDB reachable at
// $MUSTER_DDB_ENDPOINT, or skips.
//
// The skip is deliberate rather than a failure: `just verify` is the gate CI
// and every developer runs, and it must stay fast and free of a container
// dependency. `just verify-dynamo` is the recipe that guarantees the endpoint
// is up.
//
// `just verify` compiles and vets these and then skips every one, so a green
// run of THAT recipe is no evidence they passed. The `dynamo` job in
// .github/workflows/ci.yml is what actually runs them: it stands a DynamoDB
// Local service container up and runs this package plus internal/storetest
// with an endpoint set. It is a separate job from the `just verify` gate, so
// check the right one before concluding this backend is green.
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
//
// DynamoDB accepts only [a-zA-Z0-9_.-] in a table name, and Go builds subtest
// names straight from the prose in t.Run — so '/', spaces and any punctuation
// the author happened to type all have to go. Filtering to the allowed set
// rather than listing the offenders keeps a new subtest from failing on a
// ValidationException that says nothing about apostrophes.
func testTableName(t *testing.T) string {
	t.Helper()
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '.', r == '-':
			return r
		default:
			return '-'
		}
	}, t.Name())
	return TestTablePrefix + name
}

// TestStoreSatisfiesAPI pins *Store to store.API at compile time — the twin of
// daemon's TestStoreSatisfiesAPI for the SQLite backend. With KV, events and
// the idempotency records in place this backend now implements the interface
// in full, so a method added to store.API that this package forgets is a build
// failure rather than a lambda-mode surprise. It needs no endpoint.
func TestStoreSatisfiesAPI(_ *testing.T) {
	var _ store.API = (*Store)(nil)
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

// TestEnsureTableEnablesTTLOnAPreexistingTable is the guard under PruneEvents'
// no-op. That no-op is only honest while DynamoDB's native TTL is actually
// reaping events, and EnsureTable used to return early the moment the table
// existed — so a table created by hand, by CloudFormation, or by an older
// build got its events stamped with a `ttl` nothing honoured and PruneEvents
// still reported (0, nil). The journal grew without bound and nothing said so.
//
// The second EnsureTable is not padding: UpdateTimeToLive against an
// already-enabled table is a ValidationException, not a no-op, so a check-free
// version of this fix would break every Open after the first.
func TestEnsureTableEnablesTTLOnAPreexistingTable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// A table that exists with no TTL: exactly what createTable leaves behind
	// before ensureTTL runs.
	pre := &Store{c: s.c, table: s.table + "-pre", writer: s.writer, eventTTL: s.eventTTL}
	if err := pre.createTable(ctx); err != nil {
		t.Fatalf("createTable: %v", err)
	}
	t.Cleanup(func() {
		if err := pre.DropTable(ctx); err != nil {
			t.Errorf("DropTable: %v", err)
		}
	})
	if st := ttlStatus(t, pre); st == types.TimeToLiveStatusEnabled {
		t.Fatalf("createTable must not enable ttl on its own; status = %s", st)
	}

	if err := pre.EnsureTable(ctx); err != nil {
		t.Fatalf("EnsureTable over a pre-existing table: %v", err)
	}
	if st := ttlStatus(t, pre); st != types.TimeToLiveStatusEnabled {
		t.Fatalf("ttl status = %s, want ENABLED — PruneEvents' no-op rests on this", st)
	}
	if err := pre.EnsureTable(ctx); err != nil {
		t.Fatalf("EnsureTable must stay idempotent once ttl is on: %v", err)
	}
	if st := ttlStatus(t, pre); st != types.TimeToLiveStatusEnabled {
		t.Fatalf("ttl status after the second EnsureTable = %s, want ENABLED", st)
	}
}

func ttlStatus(t *testing.T, s *Store) types.TimeToLiveStatus {
	t.Helper()
	out, err := s.c.DescribeTimeToLive(context.Background(),
		&dynamodb.DescribeTimeToLiveInput{TableName: &s.table})
	if err != nil {
		t.Fatalf("DescribeTimeToLive: %v", err)
	}
	if out.TimeToLiveDescription == nil {
		return ""
	}
	return out.TimeToLiveDescription.TimeToLiveStatus
}

// TestEnsureTableRejectsTTLOnAnotherAttribute pins the one case ensureTTL
// refuses rather than repairs: TTL already on, but expiring some other
// attribute. Re-pointing it needs a disable first and DynamoDB rate-limits
// that change, so opening anyway would mean running a bus whose journal
// nothing reaps — with PruneEvents cheerfully reporting 0.
func TestEnsureTableRejectsTTLOnAnotherAttribute(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	other := &Store{c: s.c, table: s.table + "-other", writer: s.writer, eventTTL: s.eventTTL}
	if err := other.createTable(ctx); err != nil {
		t.Fatalf("createTable: %v", err)
	}
	t.Cleanup(func() {
		if err := other.DropTable(ctx); err != nil {
			t.Errorf("DropTable: %v", err)
		}
	})
	if _, err := s.c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: &other.table,
		TimeToLiveSpecification: &types.TimeToLiveSpecification{
			AttributeName: aws.String("expires_at"),
			Enabled:       aws.Bool(true),
		},
	}); err != nil {
		t.Fatalf("seed ttl on the wrong attribute: %v", err)
	}

	err := other.EnsureTable(ctx)
	if err == nil {
		t.Fatal("EnsureTable must refuse a table whose ttl expires another attribute")
	}
	if !strings.Contains(err.Error(), "expires_at") {
		t.Fatalf("the error should name the offending attribute, got: %v", err)
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
	// Threads are partitioned by kind and target — INCLUDING broadcasts, whose
	// target is the project they are scoped to. Only the global broadcast
	// (empty target) gets a partition of its own shape. Unread math depends on
	// an entry landing in the same partition its recipient queries, so
	// collapsing scoped broadcasts here is what delivered them bus-wide.
	tests := []struct {
		toKind, toTarget, want string
	}{
		{"agent", "backend-2", "RCPT#agent#backend-2"},
		{"role", "worker", "RCPT#role#worker"},
		{"broadcast", "", "RCPT#broadcast"},
		{"broadcast", "web", "RCPT#broadcast#web"},
		{"broadcast", "api", "RCPT#broadcast#api"},
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
