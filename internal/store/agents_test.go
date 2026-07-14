package store

import "testing"

func TestRegisterAgentUpsertAndList(t *testing.T) {
	s := newTestStore(t)

	if err := s.RegisterAgent(Agent{Alias: "backend", Role: "producer", ModelType: "claude", SocketPath: "/s", PaneID: "%1", SessionName: "bhw"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	firstList, err := s.ListAgents()
	if err != nil {
		t.Fatalf("list (first): %v", err)
	}
	if len(firstList) != 1 {
		t.Fatalf("expected 1 agent after first register, got %d", len(firstList))
	}
	firstRegisteredAt := firstList[0].RegisteredAt

	// Re-register (restart) with a new pane — upsert, not duplicate.
	if err := s.RegisterAgent(Agent{Alias: "backend", Role: "producer", ModelType: "claude", SocketPath: "/s2", PaneID: "%9", SessionName: "bhw"}); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	agents, err := s.ListAgents()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent after upsert, got %d", len(agents))
	}
	if agents[0].PaneID != "%9" || agents[0].SocketPath != "/s2" {
		t.Fatalf("upsert did not refresh tuple: %+v", agents[0])
	}
	if agents[0].RegisteredAt == 0 || agents[0].LastSeen == 0 {
		t.Fatalf("timestamps not set: %+v", agents[0])
	}
	if agents[0].RegisteredAt != firstRegisteredAt {
		t.Fatalf("RegisteredAt should be immutable across upsert: first=%d second=%d", firstRegisteredAt, agents[0].RegisteredAt)
	}
	if agents[0].LastSeen < firstList[0].LastSeen {
		t.Fatalf("LastSeen should not go backwards across upsert: first=%d second=%d", firstList[0].LastSeen, agents[0].LastSeen)
	}
}

func TestRegisterAgentRoundTripsSessionIDAndGetAgent(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "backend", Role: "producer", ModelType: "claude", SocketPath: "/s", PaneID: "%1", SessionName: "muster", SessionID: "$3"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok, err := s.GetAgent("backend")
	if err != nil || !ok {
		t.Fatalf("GetAgent: ok=%v err=%v", ok, err)
	}
	if got.SessionID != "$3" || got.SessionName != "muster" {
		t.Fatalf("session fields not round-tripped: %+v", got)
	}
	if _, ok, _ := s.GetAgent("nope"); ok {
		t.Fatalf("GetAgent should report ok=false for unknown alias")
	}
}

func TestAgentLabelAndDelete(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{
		Alias: "muster-2", Role: "peer", ModelType: "codex",
		SocketPath: "/tmp/tmux-0/proj-muster", SessionID: "$1",
		Project: "muster", Label: "frontend", LabelManual: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetAgent("muster-2")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Project != "muster" || got.Label != "frontend" || !got.LabelManual {
		t.Fatalf("round-trip=%+v", got)
	}

	// upsert refreshes label fields
	if err := s.RegisterAgent(Agent{Alias: "muster-2", Label: "backend", LabelManual: false}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetAgent("muster-2")
	if got.Label != "backend" || got.LabelManual {
		t.Fatalf("after upsert=%+v", got)
	}

	// delete removes the row, leaves the table usable
	if err := s.DeleteAgent("muster-2"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetAgent("muster-2"); ok {
		t.Fatal("agent should be gone after DeleteAgent")
	}
	if err := s.DeleteAgent("nonexistent"); err != nil {
		t.Fatalf("DeleteAgent of unknown alias must be a no-op, got %v", err)
	}
}
