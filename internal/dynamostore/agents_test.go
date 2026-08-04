package dynamostore

import (
	"context"
	"sync"
	"testing"
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
