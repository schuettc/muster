package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeDaemon answers the three ops the carrier uses, from in-memory state.
// threads maps a thread id to the alias concerned in it — the fake's stand-in
// for the daemon's threadConcerns join, which is how a reply with an empty
// target still reaches the thread's owner.
type fakeDaemon struct {
	aliases []string
	unread  int
	events  []Event // the whole journal, ascending ids
	threads map[int64]string
	fail    map[string]error
}

func (f *fakeDaemon) call(op string, args map[string]any) (json.RawMessage, error) {
	if err := f.fail[op]; err != nil {
		return nil, err
	}
	switch op {
	case "session_aliases":
		return json.Marshal(map[string]any{"aliases": f.aliases})
	case "session_unread":
		return json.Marshal(map[string]any{"total": f.unread, "action": 0})
	case "list_events":
		maxID := int64(0)
		for _, e := range f.events {
			if e.ID > maxID {
				maxID = e.ID
			}
		}
		if b, _ := args["backlog"].(bool); b {
			return json.Marshal(map[string]any{"events": []Event{}, "max_id": maxID})
		}
		after, _ := args["after_id"].(int64)
		agent, _ := args["agent"].(string)
		var out []Event
		for _, e := range f.events {
			concerned := e.Agent == agent || e.Target == "agent:"+agent ||
				(e.ThreadID > 0 && f.threads[e.ThreadID] == agent)
			if e.ID > after && concerned {
				out = append(out, e)
			}
		}
		return json.Marshal(map[string]any{"events": out, "max_id": maxID})
	}
	return nil, fmt.Errorf("unexpected op %s", op)
}

type pushes struct{ got []string }

func (p *pushes) notify(content string, _ map[string]string) error {
	p.got = append(p.got, content)
	return nil
}

func newCarrier(f *fakeDaemon, p *pushes) *Carrier {
	return &Carrier{
		Call: f.call, Notify: p.notify,
		Ident: Identity{SocketPath: "/tmp/s", SessionID: "$1", PaneID: "%1", SessionCreated: 100},
		Errw:  &strings.Builder{},
	}
}

func TestStartSetsCursorAtMaxAndSummarizesUnread(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker"}, unread: 2, events: []Event{{ID: 5, Kind: "send", Agent: "lead", Target: "agent:worker", ThreadID: 1}}}
	p := &pushes{}
	c := newCarrier(f, p)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 1 || !strings.Contains(p.got[0], "2 unread") || !strings.Contains(p.got[0], "get_inbox") {
		t.Fatalf("startup summary: %v", p.got)
	}
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 1 {
		t.Fatalf("events before the start cursor must never be replayed: %v", p.got)
	}
}

func TestTickPushesNewMailAndAdvancesCursor(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker"}}
	p := &pushes{}
	c := newCarrier(f, p)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	f.events = append(f.events, Event{ID: 1, Kind: "send", Agent: "lead", Target: "agent:worker", ToKind: "agent", ThreadID: 7, Subject: "hi", Intent: "action-requested"})
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 1 || !strings.Contains(p.got[0], "thread #7") {
		t.Fatalf("first tick: %v", p.got)
	}
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 1 {
		t.Fatalf("a quiet tick must push nothing: %v", p.got)
	}
}

func TestTickIgnoresSelfAuthoredAndNonMailKinds(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker"}, threads: map[int64]string{1: "worker"}}
	p := &pushes{}
	c := newCarrier(f, p)
	_ = c.Start()
	f.events = []Event{
		{ID: 1, Kind: "send", Agent: "worker", Target: "agent:lead", ToKind: "agent", ThreadID: 1},    // I sent it
		{ID: 2, Kind: "read", Agent: "worker", Target: "", ThreadID: 1},                               // not mail
		{ID: 3, Kind: "notify", Agent: "", Target: "agent:worker", ThreadID: 1},                       // wake-layer noise
		{ID: 4, Kind: "reply", Agent: "lead", Target: "", ToKind: "agent", ThreadID: 1, Subject: "s"}, // mail
	}
	_ = c.Tick()
	if len(p.got) != 1 || !strings.HasPrefix(p.got[0], "muster: reply from lead") {
		t.Fatalf("only the reply is mail for me: %v", p.got)
	}
}

func TestTickCoalescesAcrossAliasesWithoutDuplicates(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker", "backend"}}
	p := &pushes{}
	c := newCarrier(f, p)
	_ = c.Start()
	f.events = []Event{
		{ID: 1, Kind: "send", Agent: "lead", Target: "agent:worker", ToKind: "agent", ThreadID: 1, Subject: "a"},
		{ID: 2, Kind: "send", Agent: "lead", Target: "agent:backend", ToKind: "agent", ThreadID: 2, Subject: "b"},
	}
	_ = c.Tick()
	if len(p.got) != 1 || !strings.HasPrefix(p.got[0], "muster: 2 new") {
		t.Fatalf("one push per tick across aliases: %v", p.got)
	}
}

func TestNoRegistrationIdlesWithoutError(t *testing.T) {
	f := &fakeDaemon{}
	p := &pushes{}
	c := newCarrier(f, p)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	f.events = []Event{{ID: 1, Kind: "send", Agent: "lead", Target: "agent:worker", ToKind: "agent", ThreadID: 1}}
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 0 {
		t.Fatalf("unregistered session must push nothing: %v", p.got)
	}
	if !strings.Contains(c.Status(), "not registered") {
		t.Errorf("status must say why it is idle: %q", c.Status())
	}
	f.aliases = []string{"worker"}
	_ = c.Tick()
	if len(p.got) != 1 {
		t.Fatalf("registration picked up on a later tick: %v", p.got)
	}
}

func TestNoPaneIdles(t *testing.T) {
	c := &Carrier{Call: (&fakeDaemon{}).call, Notify: (&pushes{}).notify, Errw: &strings.Builder{}}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Status(), "no tmux pane") {
		t.Errorf("status: %q", c.Status())
	}
}

func TestDaemonErrorIsReportedNotFatal(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker"}, fail: map[string]error{"list_events": fmt.Errorf("socket gone")}}
	p := &pushes{}
	c := newCarrier(f, p)
	if err := c.Start(); err == nil {
		t.Fatal("Start must surface a daemon failure")
	}
	f.fail = nil
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	f.fail = map[string]error{"list_events": fmt.Errorf("socket gone")}
	if err := c.Tick(); err == nil {
		t.Fatal("Tick must surface a daemon failure")
	}
	if !strings.Contains(c.Status(), "socket gone") {
		t.Errorf("status carries the last error: %q", c.Status())
	}
}

func TestRunPollsUntilContextEnds(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker"}}
	p := &pushes{}
	c := newCarrier(f, p)
	c.Interval = MinInterval
	ctx, cancel := context.WithCancel(context.Background())
	ticks := 0
	c.Sleep = func(time.Duration) {
		ticks++
		if ticks == 4 {
			cancel()
		}
	}
	c.Run(ctx)
	if ticks != 4 {
		t.Fatalf("expected 4 sleeps before cancellation stopped the loop, got %d", ticks)
	}
}

func TestStatusReportsAliasesAndCursor(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker"}, events: []Event{{ID: 12, Kind: "send", Agent: "x", Target: "agent:y"}}}
	c := newCarrier(f, &pushes{})
	_ = c.Start()
	// Aliases resolve on ticks, not at Start — Status reflects the last tick.
	_ = c.Tick()
	s := c.Status()
	for _, want := range []string{"worker", "%1", "cursor 12"} {
		if !strings.Contains(s, want) {
			t.Errorf("status %q lacks %q", s, want)
		}
	}
}
