package channel

import "testing"

func TestShouldPushPolicy(t *testing.T) {
	mine := map[string]bool{"me": true}
	cases := []struct {
		name string
		e    Event
		want bool
	}{
		{"direct non-fyi pushes", Event{Kind: "send", ToKind: "agent", Target: "agent:me", Intent: "action-requested"}, true},
		{"direct unspecified-intent pushes", Event{Kind: "send", ToKind: "agent", Target: "agent:me"}, true},
		{"direct fyi is polite", Event{Kind: "send", ToKind: "agent", Target: "agent:me", Intent: "fyi"}, false},
		{"direct reply pushes", Event{Kind: "reply", ToKind: "agent", Origin: "peer"}, true},

		{"broadcast opener is polite", Event{Kind: "send", ToKind: "broadcast", Target: "broadcast:web"}, false},
		{"broadcast opener action-requested still polite", Event{Kind: "send", ToKind: "broadcast", Intent: "action-requested"}, false},
		{"broadcast reply as audience is polite", Event{Kind: "reply", ToKind: "broadcast", Origin: "someone-else"}, false},
		{"broadcast reply to me-the-originator pushes", Event{Kind: "reply", ToKind: "broadcast", Origin: "me"}, true},
		{"broadcast reply to me-the-originator but fyi is polite", Event{Kind: "reply", ToKind: "broadcast", Origin: "me", Intent: "fyi"}, false},

		{"role opener is polite", Event{Kind: "send", ToKind: "role", Target: "role:builder"}, false},
		{"role reply to me-the-originator pushes", Event{Kind: "reply", ToKind: "role", Origin: "me"}, true},

		{"break-glass broadcast opener pushes", Event{Kind: "send", ToKind: "broadcast", Wake: true}, true},
		{"break-glass overrides fyi", Event{Kind: "send", ToKind: "broadcast", Wake: true, Intent: "fyi"}, true},
		{"wake on a reply is not a break-glass opener", Event{Kind: "reply", ToKind: "broadcast", Origin: "someone-else", Wake: true}, false},
	}
	for _, tc := range cases {
		if got := shouldPush(tc.e, mine); got != tc.want {
			t.Errorf("%s: shouldPush=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// A broadcast opener lands politely (no push) even though it concerns the
// session; a break-glass broadcast pushes.
func TestTickHoldsPoliteBroadcastPushesBreakGlass(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"me"}, threads: map[int64]string{1: "me", 2: "me"}}
	p := &pushes{}
	c := newCarrier(f, p)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	f.events = []Event{{ID: 1, Kind: "send", Agent: "lead", Target: "broadcast:web", ToKind: "broadcast", ThreadID: 1, Subject: "psa"}}
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 0 {
		t.Fatalf("a polite broadcast must not push: %v", p.got)
	}
	f.events = append(f.events, Event{ID: 2, Kind: "send", Agent: "lead", Target: "broadcast:web", ToKind: "broadcast", Wake: true, ThreadID: 2, Subject: "urgent"})
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 1 {
		t.Fatalf("a break-glass broadcast must push: %v", p.got)
	}
}

// A reply on a broadcast pushes to the originator's session but not to an
// audience session — the ack-storm fix.
func TestTickBroadcastReplyOnlyWakesOriginator(t *testing.T) {
	// audience session: originator is someone else → no push.
	fa := &fakeDaemon{aliases: []string{"audience"}, threads: map[int64]string{1: "audience"}}
	pa := &pushes{}
	ca := newCarrier(fa, pa)
	_ = ca.Start()
	fa.events = []Event{{ID: 1, Kind: "reply", Agent: "acker", ToKind: "broadcast", Origin: "opener", ThreadID: 1, Subject: "ack"}}
	_ = ca.Tick()
	if len(pa.got) != 0 {
		t.Fatalf("an audience session must not be woken by a broadcast ack: %v", pa.got)
	}

	// originator session: origin is mine → push.
	fo := &fakeDaemon{aliases: []string{"opener"}, threads: map[int64]string{1: "opener"}}
	po := &pushes{}
	co := newCarrier(fo, po)
	_ = co.Start()
	fo.events = []Event{{ID: 1, Kind: "reply", Agent: "acker", ToKind: "broadcast", Origin: "opener", ThreadID: 1, Subject: "ack"}}
	_ = co.Tick()
	if len(po.got) != 1 {
		t.Fatalf("the originator must hear a reply to their broadcast: %v", po.got)
	}
}
