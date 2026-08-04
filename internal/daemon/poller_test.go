package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
	"github.com/schuettc/muster/internal/wake"
)

// scriptedUpstream is a hosted bus that answers device_poll from a script and
// everything else with a bare OK, recording what it was asked. All state is
// mutex-guarded: the poller runs on its own goroutine and these tests run
// under -race.
type scriptedUpstream struct {
	mu          sync.Mutex
	pollResults []store.DevicePollResult // answers in order; the last repeats
	sessionData map[string]any           // Data for session_unread
	polls       int
	lastSince   int64
	ops         []string
}

func (s *scriptedUpstream) Call(_ context.Context, req proto.Request) (proto.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, req.Op)
	switch req.Op {
	case "device_poll":
		if since, ok := req.Args["since_entry_id"].(int64); ok {
			s.lastSince = since
		}
		i := s.polls
		s.polls++
		if len(s.pollResults) == 0 {
			return proto.Response{OK: true, Data: store.DevicePollResult{}}, nil
		}
		if i >= len(s.pollResults) {
			i = len(s.pollResults) - 1
		}
		return proto.Response{OK: true, Data: s.pollResults[i]}, nil
	case "session_unread":
		return proto.Response{OK: true, Data: s.sessionData}, nil
	}
	return proto.Response{OK: true}, nil
}

func (s *scriptedUpstream) PollCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.polls
}

func (s *scriptedUpstream) LastSince() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSince
}

func (s *scriptedUpstream) countOp(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, seen := range s.ops {
		if seen == op {
			n++
		}
	}
	return n
}

// startRemoteDaemon is startRemote's variant for tests that drive the daemon
// object itself (StartPoller, Close) rather than only its socket.
func startRemoteDaemon(t *testing.T, up Upstream, n wake.Notifier) (*Daemon, string) {
	t.Helper()
	sock := filepath.Join(testHome(t), "sock")
	d, err := ServeRemote(sock, up, n, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, sock
}

// registerLocal registers an agent through the socket, which is the only way
// this device's roster learns it has anybody to wake.
func registerLocal(t *testing.T, sock, alias, sessionID string) {
	t.Helper()
	resp, err := client.Call(sock, proto.Request{Op: "register_agent", Args: map[string]any{
		"alias": alias, "socket_path": "/tmp/s", "session_id": sessionID,
	}})
	if err != nil || !resp.OK {
		t.Fatalf("register_agent: %v %s", err, resp.Error)
	}
}

// TestPollerReconcilesOnNewMail: the poller carries the watermark forward and
// badges exactly the sessions the server named. This is the cross-device wake
// — nothing on this device wrote anything, so no inline reconcile can fire.
func TestPollerReconcilesOnNewMail(t *testing.T) {
	up := &scriptedUpstream{
		pollResults: []store.DevicePollResult{
			{MaxEntryID: 0},
			{MaxEntryID: 7, Sessions: []store.SessionRef{{SocketPath: "/tmp/s", SessionID: "$1"}}},
		},
		sessionData: map[string]any{"total": 2},
	}
	n := &fakeNotifier{}
	d, sock := startRemoteDaemon(t, up, n)
	registerLocal(t, sock, "a1", "$1")

	d.StartPoller(5 * time.Millisecond)

	waitFor(t, func() bool {
		for _, c := range n.snapLog() {
			if c.kind == "Notify" && c.session == "$1" && c.count == 2 {
				return true
			}
		}
		return false
	})
	// The watermark the server handed back must be what the NEXT poll resumes
	// from; a poller that always asked from 0 would re-wake for old mail for
	// ever.
	waitFor(t, func() bool { return up.LastSince() == 7 })
}

// TestPollerSkipsWhenNoLocalAgents: a device with an empty local roster has
// nobody to wake and must not poll at all — this is what keeps an idle device
// free. It may seed its roster from upstream once (a daemon restarted under
// live agents would otherwise never poll again), and that is the whole budget.
func TestPollerSkipsWhenNoLocalAgents(t *testing.T) {
	up := &scriptedUpstream{}
	d, _ := startRemoteDaemon(t, up, &fakeNotifier{})

	// No register_agent call, so the local roster is empty.
	d.StartPoller(5 * time.Millisecond)
	time.Sleep(60 * time.Millisecond) // several tick intervals

	if n := up.PollCount(); n != 0 {
		t.Fatalf("idle device made %d upstream poll calls, want 0", n)
	}
	if n := up.countOp("list_agents"); n > 1 {
		t.Fatalf("idle device asked upstream for the roster %d times, want at most the one seed", n)
	}
}

// TestPollerStopsOnClose: the poller is owned by Close like the reconcile loop
// is. A goroutine that outlived it would keep calling upstream and writing
// tmux options for a daemon its owner believes is shut down.
func TestPollerStopsOnClose(t *testing.T) {
	up := &scriptedUpstream{}
	d, sock := startRemoteDaemon(t, up, &fakeNotifier{})
	registerLocal(t, sock, "a1", "$1")

	d.StartPoller(time.Millisecond)
	waitFor(t, func() bool { return up.PollCount() > 0 })

	// Close must not block for a whole tick either — it wakes the sleeper.
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := up.PollCount()
	time.Sleep(50 * time.Millisecond) // many tick intervals
	if got := up.PollCount(); got != after {
		t.Fatalf("poller made %d more calls after Close", got-after)
	}
}

// TestStartPollerIsANoOpInLocalMode: local mode has no upstream to poll, and
// starting a goroutine that would nil-pointer on the first tick is worse than
// starting none.
func TestStartPollerIsANoOpInLocalMode(t *testing.T) {
	d := New(nil, nil)
	d.StartPoller(time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestPollIntervalBacksOffAndSnapsBack: quiet ticks widen the interval up to
// the cap; a tick that found mail drops straight back to base, because
// cross-device traffic arrives in conversations.
func TestPollIntervalBacksOffAndSnapsBack(t *testing.T) {
	base := 10 * time.Second
	got := nextPollInterval(base, base, false)
	if got != 20*time.Second {
		t.Fatalf("a quiet tick gave %v, want the interval doubled", got)
	}
	if got := nextPollInterval(maxPollInterval, base, false); got != maxPollInterval {
		t.Fatalf("backoff ran past its cap: %v", got)
	}
	if got := nextPollInterval(maxPollInterval, base, true); got != base {
		t.Fatalf("a tick that found mail gave %v, want a snap back to %v", got, base)
	}
}

// TestDevicePollOpWakesAnOriginator drives the op through the socket over a
// real store, proving the wiring end to end AND that "concern" is the full
// four-arm predicate: the local agent here is neither the recipient nor a role
// or broadcast target — it STARTED the thread, and a peer answered. That reply
// lands in its inbox, so it has to light its pane too.
func TestDevicePollOpWakesAnOriginator(t *testing.T) {
	sock, s := startWithNotifierAndStore(t, &fakeNotifier{})
	resp := call(t, sock, "register_agent", map[string]any{
		"alias": "asker", "device_id": "dev-1", "socket_path": "/tmp/s", "session_id": "$1",
	})
	if !resp.OK {
		t.Fatalf("register_agent: %s", resp.Error)
	}
	id, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "asker", ToKind: "agent", ToTarget: "answerer",
	}, "question")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	var mark store.DevicePollResult
	decode(t, call(t, sock, "device_poll", map[string]any{"device_id": "dev-1"}), &mark)

	if _, err := s.AppendEntry(id, "answerer", "the answer", ""); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	var got store.DevicePollResult
	decode(t, call(t, sock, "device_poll", map[string]any{
		"device_id": "dev-1", "since_entry_id": mark.MaxEntryID,
	}), &got)

	if len(got.Sessions) != 1 || got.Sessions[0].SessionID != "$1" {
		t.Fatalf("sessions = %+v, want one $1", got.Sessions)
	}
	if got.MaxEntryID <= mark.MaxEntryID {
		t.Fatalf("watermark did not advance: %d -> %d", mark.MaxEntryID, got.MaxEntryID)
	}
}

// TestForwardStampsDeviceOnSessionScopedOps: every op whose args NAME a
// session must arrive upstream carrying this device's id. Unstamped, each
// would address whichever machine's colliding (socket_path, session_id)
// matched first in the shared roster.
func TestForwardStampsDeviceOnSessionScopedOps(t *testing.T) {
	up := &fakeUpstream{resp: proto.Response{OK: true}}
	sock := startRemote(t, up, nil)

	tuple := map[string]any{"socket_path": "/tmp/s", "session_id": "$1"}
	for _, op := range []string{"session_unread", "session_aliases", "set_label", "register_agent"} {
		args := map[string]any{"alias": "a1"}
		for k, v := range tuple {
			args[k] = v
		}
		// A client claiming another machine must be overwritten, not trusted.
		args["device_id"] = "somebody-elses-laptop"
		if _, err := client.Call(sock, proto.Request{Op: op, Args: args}); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	// A message names an AGENT, not a session, so it carries no device id.
	if _, err := client.Call(sock, proto.Request{Op: "send_message", Args: map[string]any{
		"from": "a1", "to_kind": "agent", "to_target": "a2", "body": "x",
	}}); err != nil {
		t.Fatalf("send_message: %v", err)
	}

	for _, req := range up.snap() {
		got, _ := req.Args["device_id"].(string)
		if needsDevice(req.Op) {
			if got != "dev-1" {
				t.Errorf("%s forwarded with device_id %q, want dev-1", req.Op, got)
			}
			continue
		}
		if got != "" {
			t.Errorf("%s carried an unnecessary device_id %q", req.Op, got)
		}
	}
}
