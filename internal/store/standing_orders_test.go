package store

import "testing"

func TestSetStandingOrderIsListableAndGreetsNewSession(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SetStandingOrder("web", "invariants", "author", "rebase before push"); err != nil {
		t.Fatal(err)
	}
	orders, err := s.ListStandingOrders("web")
	if err != nil || len(orders) != 1 {
		t.Fatalf("list = %d (%v), want 1", len(orders), err)
	}
	if orders[0].Key != "invariants" || orders[0].Body != "rebase before push" {
		t.Fatalf("order = %+v", orders[0])
	}
	// A session registering AFTER the order sees it once.
	if err := s.RegisterAgent(Agent{Alias: "newbie", Project: "web"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("newbie"); n != 1 {
		t.Fatalf("new session should be greeted by the standing order: unread=%d, want 1", n)
	}
}

func TestSetStandingOrderReplacesByKey(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SetStandingOrder("web", "invariants", "author", "v1 rules"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetStandingOrder("web", "invariants", "author", "v2 rules"); err != nil {
		t.Fatal(err)
	}
	orders, _ := s.ListStandingOrders("web")
	if len(orders) != 1 || orders[0].Body != "v2 rules" {
		t.Fatalf("replace should leave exactly one order (v2), got %+v", orders)
	}
	// A new session sees only the current (v2) order — one unread, not two.
	if err := s.RegisterAgent(Agent{Alias: "newbie", Project: "web"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("newbie"); n != 1 {
		t.Fatalf("new session should see only the current order: unread=%d, want 1", n)
	}
}

func TestRetractStandingOrderStopsGreetingAndDropsFromList(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SetStandingOrder("web", "invariants", "author", "rules"); err != nil {
		t.Fatal(err)
	}
	changed, err := s.RetractStandingOrder("web", "invariants")
	if err != nil || !changed {
		t.Fatalf("retract should report a change: changed=%v err=%v", changed, err)
	}
	if orders, _ := s.ListStandingOrders("web"); len(orders) != 0 {
		t.Fatalf("retracted order must drop from list, got %+v", orders)
	}
	if err := s.RegisterAgent(Agent{Alias: "newbie", Project: "web"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("newbie"); n != 0 {
		t.Fatalf("retracted order must not greet a new session: unread=%d, want 0", n)
	}
	// Idempotent: retracting again changes nothing.
	if changed, _ := s.RetractStandingOrder("web", "invariants"); changed {
		t.Fatal("second retract should be a no-op (false)")
	}
}

func TestRetractDoesNotDisturbAlreadyReadSession(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SetStandingOrder("web", "invariants", "author", "rules"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "seen", Project: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRead("seen", maxEntryID(t, s)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetractStandingOrder("web", "invariants"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("seen"); n != 0 {
		t.Fatalf("retract must not resurface as unread for a session that read it: unread=%d, want 0", n)
	}
}

func TestSetStandingOrderRejectsEmptyKey(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SetStandingOrder("web", "", "author", "x"); err == nil {
		t.Fatal("empty key must be rejected")
	}
}

func TestListStandingOrdersIgnoresAdHocStandingBroadcast(t *testing.T) {
	s := newTestStore(t)
	// An un-keyed standing broadcast (the v1 ad-hoc primitive) is not a
	// managed order and must never appear in the list.
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast", ToTarget: "web", Standing: true}, "ad-hoc"); err != nil {
		t.Fatal(err)
	}
	if orders, _ := s.ListStandingOrders("web"); len(orders) != 0 {
		t.Fatalf("ad-hoc standing broadcast must not be listed, got %+v", orders)
	}
}

func TestStandingOrderScopedToProject(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SetStandingOrder("web", "invariants", "author", "web rules"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "in-api", Project: "api"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("in-api"); n != 0 {
		t.Fatalf("other-project session must not see a scoped standing order: unread=%d, want 0", n)
	}
	if orders, _ := s.ListStandingOrders("api"); len(orders) != 0 {
		t.Fatalf("api has no orders, got %+v", orders)
	}
}
