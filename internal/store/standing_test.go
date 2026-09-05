package store

import "testing"

func bcast(t *testing.T, s *Store, standing bool, body string) {
	t.Helper()
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "sender", ToKind: "broadcast", Standing: standing}, body); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
}

func maxEntryID(t *testing.T, s *Store) int64 {
	t.Helper()
	var id int64
	if err := s.DB().QueryRow(`SELECT COALESCE(MAX(id),0) FROM entries`).Scan(&id); err != nil {
		t.Fatalf("maxEntryID: %v", err)
	}
	return id
}

func TestRegisterSeedsLiveWatermarkToMax(t *testing.T) {
	s := newTestStore(t)
	bcast(t, s, false, "a")
	bcast(t, s, false, "b")
	want := maxEntryID(t, s)
	if err := s.RegisterAgent(Agent{Alias: "newbie"}); err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.GetAgent("newbie")
	if err != nil || !ok {
		t.Fatalf("GetAgent: ok=%v err=%v", ok, err)
	}
	if a.LastReadEntryID != want {
		t.Fatalf("live watermark = %d, want %d (seeded to max)", a.LastReadEntryID, want)
	}
	if a.LastReadStandingEntryID != 0 {
		t.Fatalf("standing watermark = %d, want 0 (never seeded)", a.LastReadStandingEntryID)
	}
}

func TestNewSessionSkipsPlainBroadcastBacklog(t *testing.T) {
	s := newTestStore(t)
	bcast(t, s, false, "old hold") // sent BEFORE newbie exists
	if err := s.RegisterAgent(Agent{Alias: "newbie"}); err != nil {
		t.Fatal(err)
	}
	n, err := s.UnreadCount("newbie")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("plain broadcast backlog leaked to new session: unread=%d, want 0", n)
	}
}

func TestNewSessionSeesStandingBroadcastOnce(t *testing.T) {
	s := newTestStore(t)
	bcast(t, s, true, "standing order") // sent BEFORE newbie exists
	if err := s.RegisterAgent(Agent{Alias: "newbie"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("newbie"); n != 1 {
		t.Fatalf("standing broadcast should reach new session: unread=%d, want 1", n)
	}
	if err := s.MarkRead("newbie", maxEntryID(t, s)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("newbie"); n != 0 {
		t.Fatalf("after MarkRead standing should be quiet: unread=%d, want 0", n)
	}
	bcast(t, s, true, "second standing order")
	if n, _ := s.UnreadCount("newbie"); n != 1 {
		t.Fatalf("later standing broadcast should reappear: unread=%d, want 1", n)
	}
}

func TestPlainBroadcastAfterRegisterIsLive(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "live"}); err != nil {
		t.Fatal(err)
	}
	bcast(t, s, false, "hold now") // sent AFTER register
	if n, _ := s.UnreadCount("live"); n != 1 {
		t.Fatalf("live session must receive a live broadcast: unread=%d, want 1", n)
	}
}

func TestMarkReadAdvancesBothWatermarks(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "a"}); err != nil {
		t.Fatal(err)
	}
	bcast(t, s, false, "live")
	bcast(t, s, true, "standing")
	want := maxEntryID(t, s)
	if err := s.MarkRead("a", want); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetAgent("a")
	if got.LastReadEntryID != want || got.LastReadStandingEntryID != want {
		t.Fatalf("MarkRead watermarks = live %d / standing %d, want both %d", got.LastReadEntryID, got.LastReadStandingEntryID, want)
	}
}

func TestReviveKeepsBothWatermarks(t *testing.T) {
	s := newTestStore(t)
	bcast(t, s, false, "x")
	if err := s.RegisterAgent(Agent{Alias: "back"}); err != nil {
		t.Fatal(err)
	}
	bcast(t, s, true, "standing")
	if err := s.MarkRead("back", maxEntryID(t, s)); err != nil {
		t.Fatal(err)
	}
	want := maxEntryID(t, s)
	if err := s.DepartAgent("back"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "back"}); err != nil { // revive
		t.Fatal(err)
	}
	got, _, _ := s.GetAgent("back")
	if got.LastReadEntryID != want || got.LastReadStandingEntryID != want {
		t.Fatalf("revive must preserve watermarks: live %d / standing %d, want both %d", got.LastReadEntryID, got.LastReadStandingEntryID, want)
	}
}

func TestCreateThreadRejectsNonBroadcastStanding(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "sender", ToKind: "agent", ToTarget: "x", Standing: true}, "b"); err == nil {
		t.Fatal("standing on a non-broadcast target must be rejected")
	}
}

func TestStandingRoundTrips(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(Thread{Kind: "message", FromAgent: "sender", ToKind: "broadcast", Standing: true}, "b")
	if err != nil {
		t.Fatal(err)
	}
	th, _, err := s.GetThread(id)
	if err != nil {
		t.Fatal(err)
	}
	if !th.Standing {
		t.Fatal("GetThread lost Standing")
	}
}

func TestStandingComposesWithProjectScope(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "sender", ToKind: "broadcast", ToTarget: "web", Standing: true}, "web only"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "in-web", Project: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "in-api", Project: "api"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("in-web"); n != 1 {
		t.Fatalf("same-project new session should see scoped standing: unread=%d, want 1", n)
	}
	if n, _ := s.UnreadCount("in-api"); n != 0 {
		t.Fatalf("other-project session must not see scoped standing: unread=%d, want 0", n)
	}
}
