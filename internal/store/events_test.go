package store

import "testing"

func TestAppendEventPersistsTarget(t *testing.T) {
	s := newTestStore(t)
	if err := s.AppendEvent(Event{Kind: "send", Agent: "web", Target: "agent:api", ThreadID: 3, Detail: "subj"}); err != nil {
		t.Fatal(err)
	}
	var target string
	if err := s.DB().QueryRow(`SELECT target FROM events`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != "agent:api" {
		t.Fatalf("target = %q, want agent:api", target)
	}
}

// TestEventsBacklogAndFollowModes stays a SQLite test rather than moving into
// internal/storetest with its backlog-mode siblings: the FOLLOW half is a
// guarantee only this backend makes. Serialized writers plus an AUTOINCREMENT
// id mean a poll after id N can never miss a row, whereas the DynamoDB
// backend reads an eventually-consistent index with no cross-item ordering
// guarantee (see Events in internal/dynamostore/events.go). Follow-mode
// parity is therefore not a contract the conformance suite can assert.
func TestEventsBacklogAndFollowModes(t *testing.T) {
	s := newTestStore(t)
	for i, k := range []string{"send", "reply", "notify"} {
		if err := s.AppendEvent(Event{Kind: k, Agent: "web", ThreadID: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	back, err := s.Events(EventQuery{Backlog: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[0].Kind != "notify" || back[1].Kind != "reply" {
		t.Fatalf("backlog newest-first limit 2: %+v", back)
	}
	follow, err := s.Events(EventQuery{AfterID: back[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(follow) != 1 || follow[0].Kind != "notify" {
		t.Fatalf("follow after id %d: %+v", back[1].ID, follow)
	}
	if none, _ := s.Events(EventQuery{Backlog: true, Limit: 0}); len(none) != 0 {
		t.Fatalf("backlog limit 0 must return no rows, got %d", len(none))
	}
	if _, err := s.Events(EventQuery{AfterID: -1}); err == nil {
		t.Fatal("negative AfterID must error")
	}
	maxID, err := s.MaxEventID()
	if err != nil || maxID != back[0].ID {
		t.Fatalf("MaxEventID = %d (%v), want %d", maxID, err, back[0].ID)
	}
}

func TestPruneEventsExactBoundarySurvives(t *testing.T) {
	fakeTick(t) // from threads_test.go — strictly increasing clock
	s := newTestStore(t)
	for i := 0; i < 3; i++ { // rows at ts 1, 2, 3
		if err := s.AppendEvent(Event{Kind: "read", Agent: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneEvents(2) // DELETE WHERE ts < 2: only ts=1 goes
	if err != nil || n != 1 {
		t.Fatalf("pruned %d (%v), want 1", n, err)
	}
	left, _ := s.Events(EventQuery{Backlog: true, Limit: 10})
	if len(left) != 2 { // ts=2 (exactly at cutoff) must survive
		t.Fatalf("rows after prune = %d, want 2", len(left))
	}
}
