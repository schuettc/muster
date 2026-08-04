package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/mustertest"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
	"github.com/schuettc/muster/internal/wake"
)

// fakeUpstream is a hosted bus that records what it was asked and answers from
// a canned table: one response per op, with a default for anything unlisted.
type fakeUpstream struct {
	mu   sync.Mutex
	reqs []proto.Request

	resp   proto.Response            // answer for any op without a byOp entry
	byOp   map[string]proto.Response // per-op answers
	err    error                     // transport failure, returned for every call
	errFor string                    // if set, err applies only to this op
}

func (f *fakeUpstream) Call(_ context.Context, req proto.Request) (proto.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil && (f.errFor == "" || f.errFor == req.Op) {
		return proto.Response{}, f.err
	}
	if r, ok := f.byOp[req.Op]; ok {
		return r, nil
	}
	return f.resp, nil
}

func (f *fakeUpstream) snap() []proto.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proto.Request(nil), f.reqs...)
}

// opsSeen returns the ops the upstream was asked for, in order.
func (f *fakeUpstream) opsSeen() []string {
	var ops []string
	for _, r := range f.snap() {
		ops = append(ops, r.Op)
	}
	return ops
}

// remoteHome sets up a short MUSTER_HOME and disables the client's autospawn.
// Without MUSTER_NO_AUTOSPAWN a failed dial spawns the compiled test binary
// with `serve`, which re-runs this suite recursively.
func remoteHome(t *testing.T) string {
	t.Helper()
	home, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", home)
	t.Setenv("MUSTER_NO_AUTOSPAWN", "1")
	return home
}

func startRemote(t *testing.T, up Upstream, n *fakeNotifier) string {
	t.Helper()
	home := remoteHome(t)
	sock := filepath.Join(home, "sock")
	// A typed nil *fakeNotifier would be a NON-nil wake.Notifier, which is
	// exactly the shape the nil-notifier guards are meant to catch.
	var notifier wake.Notifier
	if n != nil {
		notifier = n
	}
	d, err := ServeRemote(sock, up, notifier, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return sock
}

// TestRemoteModeForwardsRequestsUpstream: a read arrives at the upstream
// whole, and its response comes back to the local client untouched.
func TestRemoteModeForwardsRequestsUpstream(t *testing.T) {
	up := &fakeUpstream{resp: proto.Response{OK: true}}
	sock := startRemote(t, up, nil)

	resp, err := client.Call(sock, proto.Request{Op: "list_agents"})
	if err != nil {
		t.Fatalf("client.Call: %v", err)
	}
	if !resp.OK {
		t.Fatalf("response not OK: %s", resp.Error)
	}
	reqs := up.snap()
	if len(reqs) != 1 || reqs[0].Op != "list_agents" {
		t.Fatalf("upstream saw %+v, want one list_agents", reqs)
	}
}

// TestRemoteModeDoesNotDispatchLocally: a remote-mode daemon has no store, so
// a write that fell through to local dispatch would panic on a nil store
// rather than fail quietly. Seeing the write upstream proves the write path
// forwards too.
func TestRemoteModeDoesNotDispatchLocally(t *testing.T) {
	up := &fakeUpstream{resp: proto.Response{OK: true}}
	sock := startRemote(t, up, nil)

	if _, err := client.Call(sock, proto.Request{Op: "send_message", Args: map[string]any{
		"from": "a1", "to_kind": "agent", "to_target": "a2", "body": "x",
	}}); err != nil {
		t.Fatalf("client.Call: %v", err)
	}
	reqs := up.snap()
	if len(reqs) != 1 || reqs[0].Op != "send_message" {
		t.Fatalf("write did not go upstream: %+v", reqs)
	}
	if reqs[0].Args["body"] != "x" {
		t.Fatalf("args did not survive the forward: %+v", reqs[0].Args)
	}
}

// TestServeRemoteRequiresUpstreamAndDevice: both are structural. A nil
// upstream is a daemon that cannot answer anything, and an empty device id
// makes ReconcileLocalSessions unable to tell this device's sessions from
// another's — it would badge-write into sessions on other machines.
func TestServeRemoteRequiresUpstreamAndDevice(t *testing.T) {
	home := remoteHome(t)
	if _, err := ServeRemote(filepath.Join(home, "s1"), nil, nil, "dev-1"); err == nil {
		t.Fatal("ServeRemote accepted a nil upstream")
	}
	if _, err := ServeRemote(filepath.Join(home, "s2"), &fakeUpstream{}, nil, "  "); err == nil {
		t.Fatal("ServeRemote accepted an empty device id")
	}
}

// TestRemoteModeStampsDeviceID: the device id is known to the daemon and to
// nothing above it, so the forwarding path is the only place register_agent
// can acquire one. Without it the hosted roster cannot say which machine an
// agent is on, and reconcile/poll have nothing to filter by.
func TestRemoteModeStampsDeviceID(t *testing.T) {
	up := &fakeUpstream{resp: proto.Response{OK: true}}
	sock := startRemote(t, up, nil)

	if _, err := client.Call(sock, proto.Request{Op: "register_agent", Args: map[string]any{
		"alias": "a1", "socket_path": "/tmp/tmux-501/default", "session_id": "$1",
	}}); err != nil {
		t.Fatalf("client.Call: %v", err)
	}
	reqs := up.snap()
	if len(reqs) == 0 || reqs[0].Op != "register_agent" {
		t.Fatalf("upstream saw %+v", reqs)
	}
	if got := reqs[0].Args["device_id"]; got != "dev-1" {
		t.Fatalf("device_id = %v, want dev-1", got)
	}
}

// TestRegisterAgentRecordsDeviceID: the server half of the stamp — the arg has
// to land on the stored row, or the roster still cannot say which device an
// agent is on. Local clients send no device_id and are unaffected.
func TestRegisterAgentRecordsDeviceID(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil)
	resp := d.Dispatch(proto.Request{Op: "register_agent", Args: map[string]any{
		"alias": "a1", "device_id": "dev-9",
	}})
	if !resp.OK {
		t.Fatalf("register_agent: %s", resp.Error)
	}
	ag, found, err := s.GetAgent("a1")
	if err != nil || !found {
		t.Fatalf("GetAgent: %v found=%v", err, found)
	}
	if ag.DeviceID != "dev-9" {
		t.Fatalf("DeviceID = %q, want dev-9", ag.DeviceID)
	}
}

// remoteRoster is the list_agents answer used by the reconcile tests: two
// live local agents sharing one session, one live local agent in a second
// session, plus rows that must NOT produce a badge write.
func remoteRoster() []store.Agent {
	return []store.Agent{
		{Alias: "a1", DeviceID: "dev-1", SocketPath: "/s", SessionID: "$1"},
		{Alias: "a0", DeviceID: "dev-1", SocketPath: "/s", SessionID: "$1"},
		{Alias: "b1", DeviceID: "dev-1", SocketPath: "/s", SessionID: "$2"},
		{Alias: "other", DeviceID: "dev-2", SocketPath: "/s", SessionID: "$9"},
		{Alias: "gone", DeviceID: "dev-1", SocketPath: "/s", SessionID: "$3", Departed: true},
		{Alias: "notmux", DeviceID: "dev-1"},
		{Alias: "unknown", SocketPath: "/s", SessionID: "$8"},
	}
}

// TestReconcileLocalSessionsBadgesThisDeviceOnly: reconcile is the ONE wake
// operation in remote mode. It must fetch unread upstream for each of this
// device's live sessions — coalescing sibling aliases into one write — and
// touch no session belonging to another device, a departed agent, or an agent
// with no tmux identity.
func TestReconcileLocalSessionsBadgesThisDeviceOnly(t *testing.T) {
	up := &fakeUpstream{byOp: map[string]proto.Response{
		"list_agents":    {OK: true, Data: remoteRoster()},
		"session_unread": {OK: true, Data: map[string]any{"total": 3, "action": 1}},
	}}
	n := &fakeNotifier{}
	home := remoteHome(t)
	d, err := ServeRemote(filepath.Join(home, "sock"), up, n, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	d.ReconcileLocalSessions()

	got := n.snap(&n.notified)
	want := []string{"$1", "$2"}
	if len(got) != len(want) {
		t.Fatalf("notified %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("notified %v, want %v", got, want)
		}
	}
	for _, c := range n.snapLog() {
		if c.kind == "Notify" && c.count != 3 {
			t.Fatalf("badge count = %d, want the upstream total 3", c.count)
		}
	}
	// One session_unread per session, not per alias.
	unread := 0
	for _, op := range up.opsSeen() {
		if op == "session_unread" {
			unread++
		}
	}
	if unread != 2 {
		t.Fatalf("%d session_unread calls, want 2 (one per session)", unread)
	}
	// The agent badge is part of reconciling a session and costs no extra
	// round trip — the roster is already in hand.
	sets := n.snapAgentSets()
	if len(sets) != 2 {
		t.Fatalf("agent-badge pushes = %d, want 2: %+v", len(sets), sets)
	}
	if sets[0].session != "$1" || len(sets[0].aliases) != 2 || sets[0].aliases[0] != "a0" {
		t.Fatalf("agent badge for $1 = %+v, want sorted [a0 a1]", sets[0])
	}
}

// TestReconcileClearsWhenUpstreamReportsZero: a drained session must have its
// badge unset, not left lit.
func TestReconcileClearsWhenUpstreamReportsZero(t *testing.T) {
	up := &fakeUpstream{byOp: map[string]proto.Response{
		"list_agents":    {OK: true, Data: []store.Agent{{Alias: "a1", DeviceID: "dev-1", SocketPath: "/s", SessionID: "$1"}}},
		"session_unread": {OK: true, Data: map[string]any{"total": 0}},
	}}
	n := &fakeNotifier{}
	home := remoteHome(t)
	d, err := ServeRemote(filepath.Join(home, "sock"), up, n, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	d.ReconcileLocalSessions()
	if cleared := n.snap(&n.cleared); len(cleared) != 1 || cleared[0] != "$1" {
		t.Fatalf("cleared = %v, want [$1]", cleared)
	}
}

// TestReconcileIsSkippedWithoutANotifier: with no notifier there is no badge
// to write, so reconcile must not spend an upstream round trip at all. The
// forwarding tests rely on this — they assert an exact upstream call count.
func TestReconcileIsSkippedWithoutANotifier(t *testing.T) {
	up := &fakeUpstream{resp: proto.Response{OK: true}}
	home := remoteHome(t)
	d, err := ServeRemote(filepath.Join(home, "sock"), up, nil, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	d.ReconcileLocalSessions()
	if reqs := up.snap(); len(reqs) != 0 {
		t.Fatalf("reconcile with no notifier called upstream: %+v", reqs)
	}
}

// TestRemoteWriteTriggersReconcile: the inline trigger. Same-device messaging
// keeps today's latency because a local write reconciles immediately rather
// than waiting for the poller.
func TestRemoteWriteTriggersReconcile(t *testing.T) {
	up := &fakeUpstream{byOp: map[string]proto.Response{
		"send_message":   {OK: true},
		"list_agents":    {OK: true, Data: []store.Agent{{Alias: "a1", DeviceID: "dev-1", SocketPath: "/s", SessionID: "$1"}}},
		"session_unread": {OK: true, Data: map[string]any{"total": 2}},
	}}
	n := &fakeNotifier{}
	sock := startRemote(t, up, n)

	if _, err := client.Call(sock, proto.Request{Op: "send_message", Args: map[string]any{
		"from": "a1", "to_kind": "agent", "to_target": "a1", "body": "x",
	}}); err != nil {
		t.Fatalf("client.Call: %v", err)
	}
	waitFor(t, func() bool { return len(n.snap(&n.notified)) > 0 })
	if got := n.snap(&n.notified); got[0] != "$1" {
		t.Fatalf("notified %v, want [$1]", got)
	}
}

// TestRemoteReadDoesNotTriggerReconcile: a read changes nothing, so it must
// not spend a reconcile's worth of upstream calls.
func TestRemoteReadDoesNotTriggerReconcile(t *testing.T) {
	up := &fakeUpstream{resp: proto.Response{OK: true}}
	n := &fakeNotifier{}
	sock := startRemote(t, up, n)

	if _, err := client.Call(sock, proto.Request{Op: "list_threads"}); err != nil {
		t.Fatalf("client.Call: %v", err)
	}
	// Give a stray reconcile goroutine room to be wrong.
	time.Sleep(50 * time.Millisecond)
	if ops := up.opsSeen(); len(ops) != 1 || ops[0] != "list_threads" {
		t.Fatalf("a read triggered extra upstream work: %v", ops)
	}
}

// TestUpstreamTransportErrorBecomesAProtocolError: a client above the daemon
// speaks proto, not HTTP. A transport failure has to arrive as a failed
// Response rather than a dropped connection.
func TestUpstreamTransportErrorBecomesAProtocolError(t *testing.T) {
	up := &fakeUpstream{err: errors.New("dial tcp: no route to host")}
	sock := startRemote(t, up, nil)

	resp, err := client.Call(sock, proto.Request{Op: "list_agents"})
	if err != nil {
		t.Fatalf("client.Call: %v", err)
	}
	if resp.OK {
		t.Fatal("a transport failure must not answer OK")
	}
	if !strings.Contains(resp.Error, "no route to host") {
		t.Fatalf("error lost the cause: %q", resp.Error)
	}
}

// TestNewKeyReissueErrorSurfacesAsUnknownOutcome is the Task 12 review's
// carried fix. The two "reissue under a new key" outcomes mean the write MAY
// have committed. They ride an HTTP 200 (they are not same-key retryable), so
// remote.Call returns them as a successful call with a nil error, and a
// verbatim pass-through would read to the client as an ordinary refusal.
func TestNewKeyReissueErrorSurfacesAsUnknownOutcome(t *testing.T) {
	unknown := proto.Response{Error: idemNewKeyPrefix + "claim failed: injected"}
	if IsRetryableIdemError(unknown.Error) {
		t.Fatal("a new-key-reissue error must not read as same-key retryable")
	}
	if !IsNewKeyReissueError(unknown.Error) {
		t.Fatalf("IsNewKeyReissueError missed %q", unknown.Error)
	}
	up := &fakeUpstream{byOp: map[string]proto.Response{"send_message": unknown}}
	sock := startRemote(t, up, nil)

	resp, err := client.Call(sock, proto.Request{Op: "send_message", Args: map[string]any{
		"from": "a1", "to_kind": "agent", "to_target": "a1", "body": "x",
	}})
	if err != nil {
		t.Fatalf("client.Call: %v", err)
	}
	if resp.OK {
		t.Fatal("an unknown-outcome write must not answer OK")
	}
	if !strings.Contains(resp.Error, "unknown") {
		t.Fatalf("error does not say the outcome is unknown: %q", resp.Error)
	}
	// The daemon must NOT reissue on its own: a duplicate get_inbox loses
	// unread mail and a duplicate task_claim answers a wrong not-claimable.
	sends := 0
	for _, op := range up.opsSeen() {
		if op == "send_message" {
			sends++
		}
	}
	if sends != 1 {
		t.Fatalf("daemon reissued the write %d times; it must leave that to the caller", sends)
	}
}

// TestIdemPredicatesAreDisjoint: the two exported predicates classify the
// SAME string and mean opposite things (same-key retry vs new-key reissue). A
// string that satisfied both would make lambdamode's 409/200 split arbitrary.
func TestIdemPredicatesAreDisjoint(t *testing.T) {
	for _, s := range []string{
		idemRetryPrefix + "an identical request is in flight",
		idemNewKeyPrefix + "claim failed: boom",
		idemNewKeyPrefix + "corrupt record: boom",
		"some unrelated store error",
	} {
		if IsRetryableIdemError(s) && IsNewKeyReissueError(s) {
			t.Fatalf("both predicates claim %q", s)
		}
	}
}

// TestLocalModeStillDispatchesLocally: the whole point of the mode switch is
// that local mode is untouched. A daemon built by Serve has no upstream, and
// must answer from its own store.
func TestLocalModeStillDispatchesLocally(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	resp := call(t, sock, "register_agent", map[string]any{"alias": "a1"})
	if !resp.OK {
		t.Fatalf("register_agent: %s", resp.Error)
	}
	if resp = call(t, sock, "list_agents", nil); !resp.OK {
		t.Fatalf("list_agents: %s", resp.Error)
	}
	var agents []store.Agent
	decode(t, resp, &agents)
	if len(agents) != 1 || agents[0].Alias != "a1" {
		t.Fatalf("local mode did not answer from its own store: %+v", agents)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the expected state")
}
