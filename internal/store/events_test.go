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

// TestEventsAgentFilterMatchesThreadConcern is the finding-1 regression: a
// reply row has empty target, so only the thread-concern join can match the
// originator.
func TestEventsAgentFilterMatchesThreadConcern(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(Thread{Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api"}, "req")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Event{
		{Kind: "send", Agent: "web", Target: "agent:api", ThreadID: id, Detail: "req"},
		{Kind: "reply", Agent: "api", ThreadID: id},
		{Kind: "nudge", Target: "web"},
		{Kind: "send", Agent: "x", Target: "agent:zzz", ThreadID: 999},
	} {
		if err := s.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Events(EventQuery{Agent: "web", Backlog: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 { // its send (actor), api's reply (thread concern), the nudge (bare target)
		t.Fatalf("agent=web should match 3 events, got %d: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Agent == "x" {
			t.Fatalf("unrelated event leaked through agent filter: %+v", e)
		}
	}
}

func TestEventsJoinsThreadSubject(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(Thread{Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api", Subject: "hello subj"}, "b")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(Event{Kind: "notify", Agent: "api", ThreadID: id, Count: 1, Detail: "lit"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(Event{Kind: "read", Agent: "api"}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Events(EventQuery{Backlog: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if evs[1].Subject != "hello subj" || evs[0].Subject != "" {
		t.Fatalf("subject join: notify=%q (want hello subj), read=%q (want empty)", evs[1].Subject, evs[0].Subject)
	}
}
