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

func TestEventsAppendListFilterAndLimit(t *testing.T) {
	s := newTestStore(t)
	for _, e := range []Event{
		{Kind: "notify", Agent: "web", ThreadID: 1, Count: 1, Detail: "lit"},
		{Kind: "read", Agent: "web"},
		{Kind: "notify", Agent: "api", ThreadID: 1, Count: 2, Detail: "lit"},
	} {
		if err := s.AppendEvent(e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	all, err := s.RecentEvents("", 0)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 events, got %d", len(all))
	}
	if all[0].Agent != "api" || all[0].Count != 2 {
		t.Fatalf("events must be newest first, got %+v", all[0])
	}
	if all[0].TS == 0 {
		t.Fatalf("AppendEvent must stamp ts")
	}

	web, err := s.RecentEvents("web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(web) != 2 {
		t.Fatalf("agent filter: want 2 events for web, got %d", len(web))
	}

	one, err := s.RecentEvents("", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Agent != "api" {
		t.Fatalf("limit 1 should return only the newest event, got %+v", one)
	}
}
