package dynamostore

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/schuettc/muster/internal/store"
)

func TestNextIDIsMonotonic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var prev int64
	for i := 0; i < 5; i++ {
		id, err := s.nextID(ctx, "entry")
		if err != nil {
			t.Fatalf("nextID: %v", err)
		}
		if id <= prev {
			t.Fatalf("nextID returned %d after %d — ids must increase", id, prev)
		}
		prev = id
	}
}

func TestNextIDCountersAreIndependent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Thread and entry ids are separate sequences; sharing one counter would
	// still be monotonic but would waste ids and confuse debugging.
	threadID, err := s.nextID(ctx, "thread")
	if err != nil {
		t.Fatalf("nextID(thread): %v", err)
	}
	entryID, err := s.nextID(ctx, "entry")
	if err != nil {
		t.Fatalf("nextID(entry): %v", err)
	}
	if threadID != 1 || entryID != 1 {
		t.Fatalf("first ids = thread %d, entry %d; want 1 and 1 — counters are not independent",
			threadID, entryID)
	}
}

func TestNextIDIsAtomicUnderConcurrency(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const n = 20

	var mu sync.Mutex
	got := make(map[int64]bool, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := s.nextID(ctx, "entry")
			if err != nil {
				t.Errorf("nextID: %v", err)
				return
			}
			mu.Lock()
			got[id] = true
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(got) != n {
		t.Fatalf("allocated %d distinct ids from %d concurrent calls — the counter is not atomic, "+
			"which would let two entries share an id", len(got), n)
	}
}

// TestTouchAndDepartUnknownAliasAreNoOps covers TouchAgent, which is not on
// store.API and so has no conformance case; DepartAgent is asserted here too
// because the two share the guard.
func TestTouchAndDepartUnknownAliasAreNoOps(t *testing.T) {
	s := newTestStore(t)
	// Both mirror the SQLite UPDATE-matches-no-rows contract. DynamoDB's
	// UpdateItem is an upsert, so without a guard these would CREATE a row
	// containing nothing but a timestamp or a departed flag.
	if err := s.TouchAgent("nobody"); err != nil {
		t.Fatalf("TouchAgent unknown: %v", err)
	}
	if err := s.DepartAgent("nobody"); err != nil {
		t.Fatalf("DepartAgent unknown: %v", err)
	}
	agents, err := s.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("no-op calls created %d phantom row(s): %+v", len(agents), agents)
	}
}

// TestSetSessionLabelDoesNotResurrectAPurgedAlias pins the ConditionExpression
// on SetSessionLabel's UpdateItem.
//
// SetSessionLabel iterates the ROSTER partition of gsi1 — an index, therefore
// eventually consistent — and then updates each member by primary key. A
// purge_agent that has already deleted the base item can still be lagging in
// that index, so the loop can name an alias that no longer exists. DynamoDB's
// UpdateItem is an upsert, so without the condition that write RECREATES the
// alias as a base item carrying nothing but label and label_manual: no
// gsi1pk, so ListAgents can never see it again to clean it up, while GetAgent
// answers ok=true with an empty identity — a name that is addressable, alive
// to one surface, invisible to the other.
//
// DynamoDB Local keeps its indexes in step, so the lag is constructed rather
// than waited for: a roster item whose pk does not match agentKey(alias) is
// exactly the state the loop sees mid-race — visible in the index, absent at
// the key the update will target.
func TestSetSessionLabelDoesNotResurrectAPurgedAlias(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RegisterAgent(store.Agent{
		Alias: "real", DeviceID: "dev-1", SocketPath: "/s", SessionID: "$1", SessionCreated: 100,
	}); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}

	// A roster member whose base item is gone: same session tuple, but its
	// item lives at a key agentKey("purged") does not name.
	if _, err := s.c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"pk": attrS(pkAgent("purged") + "#stale-index-copy"), "sk": attrN(metaSK),
			"alias": attrS("purged"), "device_id": attrS("dev-1"),
			"socket_path": attrS("/s"), "session_id": attrS("$1"), "session_created": attrN(100),
			"gsi1pk": attrS(rosterPartition), "gsi1sk": attrN(metaSK),
		},
	}); err != nil {
		t.Fatalf("seed the lagging index copy: %v", err)
	}

	n, err := s.SetSessionLabel("dev-1", "/s", "$1", 100, "renamed", true)
	if err != nil {
		t.Fatalf("SetSessionLabel: %v", err)
	}
	if n != 1 {
		t.Errorf("SetSessionLabel changed %d rows, want 1 — only the live alias exists", n)
	}

	if _, found, err := s.GetAgent("purged"); err != nil {
		t.Fatalf("GetAgent: %v", err)
	} else if found {
		t.Error("SetSessionLabel recreated the purged alias as a phantom base item: " +
			"GetAgent answers ok=true for a name ListAgents can never show")
	}

	// The live sibling still got its label.
	a, found, err := s.GetAgent("real")
	if err != nil || !found {
		t.Fatalf("GetAgent(real) = found %v, err %v", found, err)
	}
	if a.Label != "renamed" || !a.LabelManual {
		t.Errorf("live alias label = %q (manual %v), want \"renamed\" (manual true)", a.Label, a.LabelManual)
	}
}
