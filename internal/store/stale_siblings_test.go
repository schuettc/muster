package store

import "testing"

// TestDepartStaleSiblings covers the ghost-reaper (see DepartStaleSiblings):
// tmux recycles session IDs across server restarts, so a registration left
// behind by a dead server incarnation can share a (socket_path, session_id)
// tuple with a live session — its differing session_created is the proof it
// is dead.
func TestDepartStaleSiblings(t *testing.T) {
	s := newTestStore(t)
	reg := func(alias string, created int64) {
		t.Helper()
		if err := s.RegisterAgent(Agent{Alias: alias, SocketPath: "/s", SessionID: "$0", SessionCreated: created}); err != nil {
			t.Fatalf("register %s: %v", alias, err)
		}
	}
	reg("ghost", 100)   // previous incarnation of $0
	reg("legacy", 0)    // pre-upgrade row: unknown creation time
	reg("sibling", 200) // current incarnation of $0
	reg("registrant", 200)
	if err := s.RegisterAgent(Agent{Alias: "elsewhere", SocketPath: "/s", SessionID: "$1", SessionCreated: 100}); err != nil {
		t.Fatalf("register elsewhere: %v", err)
	}

	stale, err := s.DepartStaleSiblings("/s", "$0", 200, "registrant")
	if err != nil {
		t.Fatalf("DepartStaleSiblings: %v", err)
	}
	if len(stale) != 1 || stale[0] != "ghost" {
		t.Fatalf("stale = %v, want exactly [ghost]", stale)
	}
	wantDeparted := map[string]bool{"ghost": true, "legacy": false, "sibling": false, "registrant": false, "elsewhere": false}
	agents, err := s.ListAgents()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, a := range agents {
		if a.Departed != wantDeparted[a.Alias] {
			t.Errorf("%s: departed=%v, want %v", a.Alias, a.Departed, wantDeparted[a.Alias])
		}
	}

	// created == 0 (registrant outside tmux / unknown) must never reap: it
	// carries no incarnation evidence.
	if stale, err := s.DepartStaleSiblings("/s", "$0", 0, "registrant"); err != nil || stale != nil {
		t.Fatalf("created=0 must be a no-op, got (%v, %v)", stale, err)
	}
	// Empty tuple components likewise.
	if stale, err := s.DepartStaleSiblings("", "$0", 200, "x"); err != nil || stale != nil {
		t.Fatalf("empty socket must be a no-op, got (%v, %v)", stale, err)
	}
}

// TestRegisterAgentRoundTripsSessionCreated pins the column through the
// upsert and both read paths.
func TestRegisterAgentRoundTripsSessionCreated(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "a", SocketPath: "/s", SessionID: "$0", SessionCreated: 1784000000}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetAgent("a")
	if err != nil || !ok || got.SessionCreated != 1784000000 {
		t.Fatalf("GetAgent = (%+v, %v, %v), want SessionCreated 1784000000", got, ok, err)
	}
	// Re-register after a server restart: the recycled tuple's NEW creation
	// time must replace the old one.
	if err := s.RegisterAgent(Agent{Alias: "a", SocketPath: "/s", SessionID: "$0", SessionCreated: 1784111111}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAgents()
	if err != nil || len(list) != 1 || list[0].SessionCreated != 1784111111 {
		t.Fatalf("ListAgents = (%+v, %v), want one row with SessionCreated 1784111111", list, err)
	}
}

// TestSetSessionLabel: the set_label op's store half updates every
// non-departed alias on the tuple and nothing else.
func TestSetSessionLabel(t *testing.T) {
	s := newTestStore(t)
	for _, a := range []Agent{
		{Alias: "a", SocketPath: "/s", SessionID: "$0", SessionCreated: 200},
		{Alias: "b", SocketPath: "/s", SessionID: "$0", SessionCreated: 200},
		{Alias: "other", SocketPath: "/s", SessionID: "$1", SessionCreated: 200},
	} {
		if err := s.RegisterAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DepartAgent("b"); err != nil {
		t.Fatal(err)
	}
	n, err := s.SetSessionLabel("/s", "$0", 200, "datalake", true)
	if err != nil || n != 1 {
		t.Fatalf("SetSessionLabel = (%d, %v), want 1 row (departed sibling and other session spared)", n, err)
	}
	for alias, want := range map[string]string{"a": "datalake", "b": "", "other": ""} {
		got, _, err := s.GetAgent(alias)
		if err != nil || got.Label != want {
			t.Errorf("%s: label=%q (err %v), want %q", alias, got.Label, err, want)
		}
	}
	if n, err := s.SetSessionLabel("", "$0", 200, "x", true); err != nil || n != 0 {
		t.Fatalf("empty socket must be a no-op, got (%d, %v)", n, err)
	}
}

// TestSetSessionLabelScopedToIncarnation: a label write only lands on rows
// of the proven incarnation — never on a recycled-ID ghost (created
// mismatch) or an unprovable legacy row (created 0).
func TestSetSessionLabelScopedToIncarnation(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "current", SocketPath: "/s", SessionID: "$1", SessionCreated: 200}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "ghost", SocketPath: "/s", SessionID: "$1", SessionCreated: 0}); err != nil {
		t.Fatal(err)
	}

	n, err := s.SetSessionLabel("/s", "$1", 200, "nfl-3", true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected exactly the current row labeled, got %d", n)
	}
	ghost, _, err := s.GetAgent("ghost")
	if err != nil {
		t.Fatal(err)
	}
	if ghost.Label != "" || ghost.LabelManual {
		t.Fatalf("ghost must be untouched, got label=%q manual=%v", ghost.Label, ghost.LabelManual)
	}
}
