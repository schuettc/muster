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

func startRemote(t *testing.T, up Upstream, n *fakeNotifier) string {
	t.Helper()
	home := testHome(t)
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
	home := testHome(t)
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

// TestRemoteModeOverwritesACallerSuppliedDeviceID: the stamp is authoritative,
// not a default. A client above the socket that supplies its own device_id —
// by accident or to claim it is on another machine — must not be believed:
// the roster's device column is what reconcile filters on, so a forged one
// would let a session on one machine be badged from another.
func TestRemoteModeOverwritesACallerSuppliedDeviceID(t *testing.T) {
	up := &fakeUpstream{resp: proto.Response{OK: true}}
	sock := startRemote(t, up, nil)

	if _, err := client.Call(sock, proto.Request{Op: "register_agent", Args: map[string]any{
		"alias": "a1", "device_id": "someone-elses-laptop",
	}}); err != nil {
		t.Fatalf("client.Call: %v", err)
	}
	reqs := up.snap()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %+v, want one register_agent", reqs)
	}
	if got := reqs[0].Args["device_id"]; got != "dev-1" {
		t.Fatalf("device_id = %v, want dev-1 — a caller-supplied id was trusted", got)
	}
}

// TestStampDeviceDoesNotMutateTheCallersArgs: forward is handed a request that
// came off the wire, and stamping must copy rather than write into it.
func TestStampDeviceDoesNotMutateTheCallersArgs(t *testing.T) {
	d := &Daemon{deviceID: "dev-1"}
	args := map[string]any{"alias": "a1"}
	req := proto.Request{Op: "register_agent", Args: args}
	stamped := d.stampDevice(req)
	if stamped.Args["device_id"] != "dev-1" {
		t.Fatalf("stamped args = %+v", stamped.Args)
	}
	if _, found := args["device_id"]; found {
		t.Fatalf("stampDevice wrote into the caller's map: %+v", args)
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
	home := testHome(t)
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
	home := testHome(t)
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

// TestReconcileDoesNotMixDevicesSharingATuple is the tuple-collision case, and
// it is not exotic: tmux's default socket for the first user on a machine is
// /private/tmp/tmux-501/default on every macOS laptop, and every tmux server
// numbers its own sessions from $1. So two devices in one hosted roster
// routinely carry the SAME (socket_path, session_id) naming two different
// sessions. Only DeviceID tells them apart, and the agent badge — which
// matches on the tuple alone, correct for a single-device store — must be fed
// a roster already narrowed to this device. Otherwise device A advertises
// device B's aliases as addressable in A's own tmux status line.
func TestReconcileDoesNotMixDevicesSharingATuple(t *testing.T) {
	const shared = "/private/tmp/tmux-501/default"
	up := &fakeUpstream{byOp: map[string]proto.Response{
		"list_agents": {OK: true, Data: []store.Agent{
			{Alias: "mine", DeviceID: "dev-1", SocketPath: shared, SessionID: "$1"},
			{Alias: "theirs", DeviceID: "dev-2", SocketPath: shared, SessionID: "$1"},
		}},
		"session_unread": {OK: true, Data: map[string]any{"total": 1}},
	}}
	n := &fakeNotifier{}
	home := testHome(t)
	d, err := ServeRemote(filepath.Join(home, "sock"), up, n, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	d.ReconcileLocalSessions()

	sets := n.snapAgentSets()
	if len(sets) != 1 {
		t.Fatalf("agent-badge pushes = %d, want 1: %+v", len(sets), sets)
	}
	if len(sets[0].aliases) != 1 || sets[0].aliases[0] != "mine" {
		t.Fatalf("agent badge = %v, want [mine] — another device's alias leaked into this device's badge", sets[0].aliases)
	}
}

// TestReconcileIsSkippedWithoutANotifier: with no notifier there is no badge
// to write, so reconcile must not spend an upstream round trip at all. The
// forwarding tests rely on this — they assert an exact upstream call count.
func TestReconcileIsSkippedWithoutANotifier(t *testing.T) {
	up := &fakeUpstream{resp: proto.Response{OK: true}}
	home := testHome(t)
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

// TestOnlyBadgeMovingWritesTriggerReconcile: the trigger is movesBadge, not
// IsWriteOp. A read changes nothing, and a write like kv_set changes nothing a
// badge shows — neither may spend a reconcile's worth of upstream calls, which
// is a list_agents plus one session_unread per local session, every time.
//
// The proof is a positive control rather than a sleep: the send_message at the
// end is the one op that MUST reconcile, and waiting for its badge write gives
// the two earlier ops far more room to have been wrong than any fixed delay
// would. What the whole upstream saw is then asserted exactly, so a stray
// reconcile is caught wherever in the sequence it landed.
func TestOnlyBadgeMovingWritesTriggerReconcile(t *testing.T) {
	up := &fakeUpstream{byOp: map[string]proto.Response{
		"list_agents":    {OK: true, Data: []store.Agent{{Alias: "a1", DeviceID: "dev-1", SocketPath: "/s", SessionID: "$1"}}},
		"session_unread": {OK: true, Data: map[string]any{"total": 1}},
	}, resp: proto.Response{OK: true}}
	n := &fakeNotifier{}
	sock := startRemote(t, up, n)

	for _, req := range []proto.Request{
		{Op: "list_threads"},
		{Op: "kv_set", Args: map[string]any{"key": "k", "value": "v", "by": "a1"}},
		{Op: "send_message", Args: map[string]any{"from": "a1", "to_kind": "agent", "to_target": "a1", "body": "x"}},
	} {
		if _, err := client.Call(sock, req); err != nil {
			t.Fatalf("%s: %v", req.Op, err)
		}
	}
	waitFor(t, func() bool { return len(n.snap(&n.notified)) > 0 })

	want := []string{"list_threads", "kv_set", "send_message", "list_agents", "session_unread"}
	got := up.opsSeen()
	if len(got) != len(want) {
		t.Fatalf("upstream saw %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("upstream saw %v, want exactly %v", got, want)
		}
	}
}

// gatedUpstream is a fakeUpstream whose list_agents parks until release is
// closed, so a test can hold a reconcile mid-flight and observe what Close
// does about it.
type gatedUpstream struct {
	fakeUpstream
	entered chan struct{} // closed when the first list_agents arrives
	release chan struct{}
	once    sync.Once
}

func (g *gatedUpstream) Call(ctx context.Context, req proto.Request) (proto.Response, error) {
	if req.Op == "list_agents" {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	return g.fakeUpstream.Call(ctx, req)
}

// TestCloseStopsTheReconcileLoop: the reconcile goroutine outlives the request
// that spawned it, so without a shutdown path a closed daemon keeps calling
// upstream and writing tmux options — in tests, past t.Cleanup; in production,
// for as long as the poller keeps handing it work.
//
// Close must therefore both WAIT for an in-flight reconcile and refuse further
// ones, including through ReconcileLocalSessions, which the poller calls from
// outside this package and can still hold a reference to.
func TestCloseStopsTheReconcileLoop(t *testing.T) {
	up := &gatedUpstream{entered: make(chan struct{}), release: make(chan struct{})}
	up.byOp = map[string]proto.Response{
		"list_agents":    {OK: true, Data: []store.Agent{{Alias: "a1", DeviceID: "dev-1", SocketPath: "/s", SessionID: "$1"}}},
		"session_unread": {OK: true, Data: map[string]any{"total": 1}},
	}
	up.resp = proto.Response{OK: true}
	n := &fakeNotifier{}
	home := testHome(t)
	d, err := ServeRemote(filepath.Join(home, "sock"), up, n, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() }) // Close is safe to call twice

	if _, err := client.Call(filepath.Join(home, "sock"), proto.Request{Op: "send_message", Args: map[string]any{
		"from": "a1", "to_kind": "agent", "to_target": "a1", "body": "x",
	}}); err != nil {
		t.Fatalf("client.Call: %v", err)
	}
	<-up.entered // the reconcile is now parked inside upstream list_agents

	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	// Blocking cannot be observed except by waiting; this direction fails
	// OPEN (a slow machine passes) rather than flaking, and the assertions
	// after the release are the ones that carry the test.
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while a reconcile was still in flight upstream", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(up.release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the in-flight reconcile finished")
	}
	// Close waited for the whole reconcile, not just its upstream call: the
	// badge write it was on its way to make has already happened.
	if got := n.snap(&n.notified); len(got) == 0 {
		t.Fatal("Close returned before the in-flight reconcile finished its badge write")
	}

	before := len(up.snap())
	d.ReconcileLocalSessions()
	if after := len(up.snap()); after != before {
		t.Fatalf("ReconcileLocalSessions called upstream after Close (%d → %d)", before, after)
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

// --- the incarnation dimension in remote mode ------------------------------
//
// The hosted session_unread op deliberately does NOT resolve an incarnation
// (store.API: a caller answers for its own and must prove it). setSessionBadge
// is where resolution happens — and in remote mode it happens on the DEVICE,
// before the request goes upstream, because the local daemon is the one
// deciding which incarnation a (socket, session)-keyed tmux write belongs to.
// These pin that the resolved value actually leaves the machine, and that it
// is resolved against a roster narrowed to this device.

// sessionCreatedIn returns the session_created arg of every session_unread the
// fake upstream was asked for, in order.
func sessionCreatedIn(up *fakeUpstream) []int64 {
	var out []int64
	for _, r := range up.snap() {
		if r.Op != "session_unread" {
			continue
		}
		switch v := r.Args["session_created"].(type) {
		case int64:
			out = append(out, v)
		case float64:
			out = append(out, int64(v))
		default:
			out = append(out, -1) // absent: the failure this test exists to catch
		}
	}
	return out
}

// TestRemoteBadgeSendsTheProvenIncarnation: the whole point. A tuple holding a
// live row and a legacy 0-created ghost must badge under the LIVE row's
// incarnation. Sending 0 — which is what omitting the arg looks like on the
// wire — asks the hosted store for an incarnation nothing matches, and it
// answers an authoritative zero: the badge is CLEARED on a session with mail.
func TestRemoteBadgeSendsTheProvenIncarnation(t *testing.T) {
	const sock = "/private/tmp/tmux-501/default"
	up := &fakeUpstream{byOp: map[string]proto.Response{
		"list_agents": {OK: true, Data: []store.Agent{
			// The ghost sorts first, so a per-alias implementation picks it.
			{Alias: "aaa-ghost", DeviceID: "dev-1", SocketPath: sock, SessionID: "$1", SessionCreated: 0},
			{Alias: "zzz-live", DeviceID: "dev-1", SocketPath: sock, SessionID: "$1", SessionCreated: 1700000000},
		}},
		"session_unread": {OK: true, Data: map[string]any{"total": 2}},
	}}
	n := &fakeNotifier{}
	home := testHome(t)
	d, err := ServeRemote(filepath.Join(home, "sock"), up, n, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	d.ReconcileLocalSessions()

	got := sessionCreatedIn(up)
	if len(got) != 1 || got[0] != 1700000000 {
		t.Fatalf("session_created sent upstream = %v, want [1700000000] — the badge must name the "+
			"incarnation that OCCUPIES the tuple, not whichever row sorted first", got)
	}
}

// TestRemoteBadgeIgnoresAnotherDevicesIncarnation: the resolution roster must
// be narrowed to this device FIRST. A peer laptop sharing the tuple has its
// own unrelated creation time, and "highest non-zero created" would let the
// peer's win outright — so this device would ask the hosted store about an
// incarnation that does not exist on it and be told, authoritatively, zero.
func TestRemoteBadgeIgnoresAnotherDevicesIncarnation(t *testing.T) {
	const shared = "/private/tmp/tmux-501/default"
	up := &fakeUpstream{byOp: map[string]proto.Response{
		"list_agents": {OK: true, Data: []store.Agent{
			{Alias: "mine", DeviceID: "dev-1", SocketPath: shared, SessionID: "$1", SessionCreated: 1700000000},
			// Same tuple, another laptop, a LATER creation time.
			{Alias: "theirs", DeviceID: "dev-2", SocketPath: shared, SessionID: "$1", SessionCreated: 1900000000},
		}},
		"session_unread": {OK: true, Data: map[string]any{"total": 1}},
	}}
	n := &fakeNotifier{}
	home := testHome(t)
	d, err := ServeRemote(filepath.Join(home, "sock"), up, n, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	d.ReconcileLocalSessions()

	got := sessionCreatedIn(up)
	if len(got) != 1 || got[0] != 1700000000 {
		t.Fatalf("session_created sent upstream = %v, want [1700000000] — another device's "+
			"incarnation on a colliding tuple must not decide this device's badge", got)
	}
}

// TestPollReconcileResolvesTheIncarnation: device_poll answers with TUPLES
// (store.SessionRef carries no creation time), so the poller has to resolve
// one itself before it can badge. It is the only cross-device wake path, so
// getting a zero here is not a stale badge — it is a permanently dark one.
func TestPollReconcileResolvesTheIncarnation(t *testing.T) {
	const sock = "/private/tmp/tmux-501/default"
	up := &fakeUpstream{byOp: map[string]proto.Response{
		"list_agents": {OK: true, Data: []store.Agent{
			{Alias: "live", DeviceID: "dev-1", SocketPath: sock, SessionID: "$1", SessionCreated: 1700000000},
			{Alias: "peer", DeviceID: "dev-2", SocketPath: sock, SessionID: "$1", SessionCreated: 1900000000},
		}},
		"session_unread": {OK: true, Data: map[string]any{"total": 4}},
	}}
	n := &fakeNotifier{}
	home := testHome(t)
	d, err := ServeRemote(filepath.Join(home, "sock"), up, n, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	d.reconcileSessions([]store.SessionRef{{SocketPath: sock, SessionID: "$1"}})

	got := sessionCreatedIn(up)
	if len(got) != 1 || got[0] != 1700000000 {
		t.Fatalf("session_created sent upstream = %v, want [1700000000]", got)
	}
	if lit := n.snap(&n.notified); len(lit) != 1 || lit[0] != "$1" {
		t.Fatalf("notified = %v, want [$1]", lit)
	}
}

// TestPollReconcileSkipsTheBatchOnARosterError: a roster read failure must
// leave the previous badge alone, NOT badge with a zero incarnation. Zero
// seeds nothing, so it would not recompute the badge — it would clear it,
// telling a session it is caught up on the one tick the server just said it
// has mail.
func TestPollReconcileSkipsTheBatchOnARosterError(t *testing.T) {
	up := &fakeUpstream{
		byOp:   map[string]proto.Response{"session_unread": {OK: true, Data: map[string]any{"total": 0}}},
		err:    errors.New("upstream down"),
		errFor: "list_agents",
	}
	n := &fakeNotifier{}
	home := testHome(t)
	d, err := ServeRemote(filepath.Join(home, "sock"), up, n, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	d.reconcileSessions([]store.SessionRef{{SocketPath: "/s", SessionID: "$1"}})

	if cleared := n.snap(&n.cleared); len(cleared) != 0 {
		t.Fatalf("cleared %v after a roster read failure — an unresolvable incarnation must "+
			"leave the badge alone, never clear it", cleared)
	}
	for _, op := range up.opsSeen() {
		if op == "session_unread" {
			t.Fatal("asked upstream for unread with no incarnation to name")
		}
	}
}
