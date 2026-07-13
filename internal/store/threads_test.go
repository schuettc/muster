package store

import "testing"

func TestCreateThreadAppendAndGet(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(Thread{
		Kind: "task", FromAgent: "backend", ToKind: "role", ToTarget: "reviewer",
		Subject: "Review feat/wagers", Ref: "repo=bhw branch=feat/wagers", Status: "open",
	}, "please review the rename")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.AppendEntry(id, "reviewer", "looks good, one nit", "claimed"); err != nil {
		t.Fatalf("append: %v", err)
	}
	th, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if th.Subject != "Review feat/wagers" || len(entries) != 2 {
		t.Fatalf("unexpected thread/entries: %+v / %d", th, len(entries))
	}
	if th.UpdatedAt < th.CreatedAt {
		t.Fatalf("updated_at should advance on append")
	}
}

func TestInboxMatchesAgentRoleAndBroadcast(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "rev1", Role: "reviewer", ModelType: "codex"}); err != nil {
		t.Fatal(err)
	}
	mk := func(toKind, toTarget string) {
		if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "backend", ToKind: toKind, ToTarget: toTarget}, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	mk("agent", "rev1")        // direct
	mk("role", "reviewer")     // by role
	mk("broadcast", "")        // to everyone
	mk("agent", "someoneelse") // not for rev1

	in, err := s.Inbox("rev1")
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(in) != 3 {
		t.Fatalf("expected 3 inbox threads for rev1, got %d", len(in))
	}
}
