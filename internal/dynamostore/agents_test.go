package dynamostore

import (
	"context"
	"slices"
	"sync"
	"testing"

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

func TestRegisterAgentRoundTrips(t *testing.T) {
	s := newTestStore(t)
	want := store.Agent{
		Alias: "a1", Role: "worker", ModelType: "claude",
		SocketPath: "/tmp/tmux-501/default", PaneID: "%1",
		SessionName: "muster-1", SessionID: "$1", SessionCreated: 1700000000,
		DeviceID: "dev-1", Project: "muster", Label: "backend", LabelManual: true,
	}
	if err := s.RegisterAgent(want); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	got, ok, err := s.GetAgent("a1")
	if err != nil || !ok {
		t.Fatalf("GetAgent: %v ok=%v", err, ok)
	}
	if got.Alias != want.Alias || got.Role != want.Role || got.ModelType != want.ModelType ||
		got.SocketPath != want.SocketPath || got.PaneID != want.PaneID ||
		got.SessionName != want.SessionName || got.SessionID != want.SessionID ||
		got.SessionCreated != want.SessionCreated || got.DeviceID != want.DeviceID ||
		got.Project != want.Project || got.Label != want.Label || got.LabelManual != want.LabelManual {
		t.Fatalf("round trip lost fields:\n got %+v\nwant %+v", got, want)
	}
	if got.RegisteredAt == 0 || got.LastSeen == 0 {
		t.Fatalf("RegisteredAt=%d LastSeen=%d, both should be stamped", got.RegisteredAt, got.LastSeen)
	}
}

func TestRegisterAgentPreservesRegisteredAtAndRevivesDeparted(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "a1", Role: "worker"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	first, _, err := s.GetAgent("a1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if err := s.DepartAgent("a1"); err != nil {
		t.Fatalf("depart: %v", err)
	}

	if err := s.RegisterAgent(store.Agent{Alias: "a1", Role: "producer"}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	got, ok, err := s.GetAgent("a1")
	if err != nil || !ok {
		t.Fatalf("GetAgent after re-register: %v ok=%v", err, ok)
	}
	if got.Departed {
		t.Error("re-registering must clear Departed — a returning session revives its row")
	}
	if got.Role != "producer" {
		t.Errorf("Role = %q, want producer — re-register must refresh the tuple", got.Role)
	}
	if got.RegisteredAt != first.RegisteredAt {
		t.Errorf("RegisteredAt changed on re-register: %d -> %d; it is insert-only",
			first.RegisteredAt, got.RegisteredAt)
	}
}

func TestListAgentsOrdersByAliasAndIncludesDeparted(t *testing.T) {
	s := newTestStore(t)
	for _, alias := range []string{"c", "a", "b"} {
		if err := s.RegisterAgent(store.Agent{Alias: alias}); err != nil {
			t.Fatalf("register %s: %v", alias, err)
		}
	}
	if err := s.DepartAgent("b"); err != nil {
		t.Fatalf("depart: %v", err)
	}

	got, err := s.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	var aliases []string
	for _, a := range got {
		aliases = append(aliases, a.Alias)
	}
	// Departed rows are history, not gone — they stay in the roster.
	if want := []string{"a", "b", "c"}; !slices.Equal(aliases, want) {
		t.Fatalf("aliases = %v, want %v", aliases, want)
	}
}

func TestGetAgentUnknownAliasIsNotAnError(t *testing.T) {
	s := newTestStore(t)
	_, ok, err := s.GetAgent("nobody")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if ok {
		t.Fatal("unknown alias must report ok=false")
	}
}

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

func TestDeleteAgentRemovesTheRow(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "a1"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.DeleteAgent("a1"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	_, ok, err := s.GetAgent("a1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if ok {
		t.Fatal("DeleteAgent must hard-delete, unlike DepartAgent's tombstone")
	}
	if err := s.DeleteAgent("a1"); err != nil {
		t.Fatalf("DeleteAgent on unknown alias must be a no-op: %v", err)
	}
}

func TestSetSessionLabelMovesSiblingsTogether(t *testing.T) {
	s := newTestStore(t)
	const sock, sess = "/tmp/tmux-501/default", "$1"
	for _, alias := range []string{"a1", "a2"} {
		if err := s.RegisterAgent(store.Agent{
			Alias: alias, SocketPath: sock, SessionID: sess,
		}); err != nil {
			t.Fatalf("register %s: %v", alias, err)
		}
	}
	// A different session must not move.
	if err := s.RegisterAgent(store.Agent{
		Alias: "other", SocketPath: sock, SessionID: "$2",
	}); err != nil {
		t.Fatalf("register other: %v", err)
	}
	// A departed sibling must not move either.
	if err := s.RegisterAgent(store.Agent{
		Alias: "gone", SocketPath: sock, SessionID: sess,
	}); err != nil {
		t.Fatalf("register gone: %v", err)
	}
	if err := s.DepartAgent("gone"); err != nil {
		t.Fatalf("depart: %v", err)
	}

	n, err := s.SetSessionLabel(sock, sess, "backend", true)
	if err != nil {
		t.Fatalf("SetSessionLabel: %v", err)
	}
	if n != 2 {
		t.Fatalf("labelled %d rows, want 2 (a1 and a2 only)", n)
	}
	for _, tc := range []struct {
		alias, want string
	}{{"a1", "backend"}, {"a2", "backend"}, {"other", ""}, {"gone", ""}} {
		got, _, err := s.GetAgent(tc.alias)
		if err != nil {
			t.Fatalf("GetAgent %s: %v", tc.alias, err)
		}
		if got.Label != tc.want {
			t.Errorf("%s label = %q, want %q", tc.alias, got.Label, tc.want)
		}
	}
}

func TestSetSessionLabelNoOpsOnEmptyTuple(t *testing.T) {
	s := newTestStore(t)
	for _, tc := range []struct{ sock, sess string }{{"", "$1"}, {"/tmp/s", ""}, {"", ""}} {
		n, err := s.SetSessionLabel(tc.sock, tc.sess, "x", true)
		if err != nil {
			t.Fatalf("SetSessionLabel(%q,%q): %v", tc.sock, tc.sess, err)
		}
		if n != 0 {
			t.Errorf("SetSessionLabel(%q,%q) = %d, want 0", tc.sock, tc.sess, n)
		}
	}
}

func TestDepartStaleSiblingsGhostGuard(t *testing.T) {
	s := newTestStore(t)
	const sock, sess = "/tmp/tmux-501/default", "$1"
	const nowCreated = 1700000200

	register := func(alias string, created int64) {
		t.Helper()
		if err := s.RegisterAgent(store.Agent{
			Alias: alias, SocketPath: sock, SessionID: sess, SessionCreated: created,
		}); err != nil {
			t.Fatalf("register %s: %v", alias, err)
		}
	}
	register("ghost", 1700000000) // older incarnation of a recycled session id
	register("keeper", nowCreated)
	register("sibling", nowCreated) // same live session — not a ghost
	register("preupgrade", 0)       // unknown creation time — must be SPARED
	if err := s.RegisterAgent(store.Agent{
		Alias: "elsewhere", SocketPath: sock, SessionID: "$2", SessionCreated: 1700000000,
	}); err != nil {
		t.Fatalf("register elsewhere: %v", err)
	}

	stale, err := s.DepartStaleSiblings(sock, sess, nowCreated, "keeper")
	if err != nil {
		t.Fatalf("DepartStaleSiblings: %v", err)
	}
	if want := []string{"ghost"}; !slices.Equal(stale, want) {
		t.Fatalf("departed %v, want %v — only a differing NON-ZERO session_created is a ghost", stale, want)
	}
	for _, alias := range []string{"keeper", "sibling", "preupgrade", "elsewhere"} {
		got, _, err := s.GetAgent(alias)
		if err != nil {
			t.Fatalf("GetAgent %s: %v", alias, err)
		}
		if got.Departed {
			t.Errorf("%s was departed but should have been spared", alias)
		}
	}
}

func TestDepartStaleSiblingsNoOpsOnEmptyTuple(t *testing.T) {
	s := newTestStore(t)
	for _, tc := range []struct {
		sock, sess string
		created    int64
	}{{"", "$1", 1}, {"/tmp/s", "", 1}, {"/tmp/s", "$1", 0}} {
		got, err := s.DepartStaleSiblings(tc.sock, tc.sess, tc.created, "keeper")
		if err != nil {
			t.Fatalf("DepartStaleSiblings(%q,%q,%d): %v", tc.sock, tc.sess, tc.created, err)
		}
		if got != nil {
			t.Errorf("DepartStaleSiblings(%q,%q,%d) = %v, want nil", tc.sock, tc.sess, tc.created, got)
		}
	}
}
