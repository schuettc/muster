package daemon

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/clock"
	"github.com/schuettc/muster/internal/mustertest"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

// threadIDOf unmarshals resp.Data.thread_id from a send_message/task_create
// response.
func threadIDOf(t *testing.T, resp proto.Response) int64 {
	t.Helper()
	data, _ := json.Marshal(resp.Data)
	var created struct {
		ThreadID int64 `json:"thread_id"`
	}
	if err := json.Unmarshal(data, &created); err != nil || created.ThreadID == 0 {
		t.Fatalf("thread_id result: %v (%s)", err, data)
	}
	return created.ThreadID
}

type notifierCall struct {
	kind    string // "Notify" or "Clear"
	session string
	count   int // only populated for Notify
}

// agentSet records one SetAgents push: which session, with which alias list
// (nil/empty = unset).
type agentSet struct {
	session string
	aliases []string
}

type fakeNotifier struct {
	mu        sync.Mutex
	notified  []string       // session IDs Notify'd
	counts    []int          // unread counts carried, aligned with notified
	cleared   []string       // session IDs Clear'd
	log       []notifierCall // combined ordered log of all Notify/Clear calls
	agentSets []agentSet     // recorded SetAgents pushes
}

func (f *fakeNotifier) Notify(_, sessionID string, count int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notified = append(f.notified, sessionID)
	f.counts = append(f.counts, count)
	f.log = append(f.log, notifierCall{kind: "Notify", session: sessionID, count: count})
	return nil
}

func (f *fakeNotifier) Clear(_, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, sessionID)
	f.log = append(f.log, notifierCall{kind: "Clear", session: sessionID})
	return nil
}

func (f *fakeNotifier) SetAgents(_, sessionID string, aliases []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agentSets = append(f.agentSets, agentSet{session: sessionID, aliases: append([]string(nil), aliases...)})
	return nil
}

func (f *fakeNotifier) snap(which *[]string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(*which))
	copy(out, *which)
	return out
}

func (f *fakeNotifier) snapLog() []notifierCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]notifierCall, len(f.log))
	copy(out, f.log)
	return out
}

func (f *fakeNotifier) snapAgentSets() []agentSet {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]agentSet, len(f.agentSets))
	copy(out, f.agentSets)
	return out
}

func startWithNotifier(t *testing.T, n *fakeNotifier) string {
	t.Helper()
	sock, _ := startWithNotifierAndStore(t, n)
	return sock
}

func startWithNotifierAndStore(t *testing.T, n *fakeNotifier) (string, *store.Store) {
	t.Helper()
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := Serve(paths.SocketPath(), s, n)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return paths.SocketPath(), s
}

func call(t *testing.T, sock, op string, args map[string]any) proto.Response {
	t.Helper()
	resp, err := client.Call(sock, proto.Request{Op: op, Args: args})
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	return resp
}

// decode re-marshals resp.Data (already a map[string]any/[]any from the wire)
// into out, matching the approach used elsewhere for typed daemon responses.
func decode(t *testing.T, resp proto.Response, out any) {
	t.Helper()
	raw, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatalf("marshal resp.Data: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal resp.Data: %v (%s)", err, raw)
	}
}

func TestNotifyDirectedExcludesActorBySession(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "backend", "role": "producer", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex", "socket_path": "/s", "session_id": "$2"})
	call(t, sock, "send_message", map[string]any{"from": "backend", "to_kind": "agent", "to_target": "consumer", "subject": "hi", "body": "x"})
	got := n.snap(&n.notified)
	if len(got) != 1 || got[0] != "$2" {
		t.Fatalf("expected only consumer session $2 notified, got %v", got)
	}
}

func TestNotifySkipsAgentsWithoutSession(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	// no session_id → not notifiable
	call(t, sock, "register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex", "socket_path": "/s"})
	call(t, sock, "send_message", map[string]any{"from": "backend", "to_kind": "agent", "to_target": "consumer", "subject": "hi", "body": "x"})
	if got := n.snap(&n.notified); len(got) != 0 {
		t.Fatalf("agent without session_id must not be notified, got %v", got)
	}
}

func TestNilNotifierIsSafe(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := Serve(paths.SocketPath(), s, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	call(t, paths.SocketPath(), "register_agent", map[string]any{"alias": "a", "role": "r", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	if resp := call(t, paths.SocketPath(), "send_message", map[string]any{"from": "a", "to_kind": "broadcast", "subject": "s", "body": "b"}); !resp.OK {
		t.Fatalf("op should succeed with nil notifier: %+v", resp)
	}
}

func TestGetInboxClearsFlag(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "reviewer", "role": "reviewer", "model_type": "codex", "socket_path": "/s", "session_id": "$5"})
	// register_agent itself now reconciles the fresh session's badge (spec
	// §3), so it already emits one Clear($5) before get_inbox runs — assert
	// the DELTA get_inbox contributes, not the raw total.
	before := len(n.snap(&n.cleared))
	call(t, sock, "get_inbox", map[string]any{"alias": "reviewer"})
	got := n.snap(&n.cleared)
	if len(got) != before+1 || got[len(got)-1] != "$5" {
		t.Fatalf("get_inbox should clear reviewer's session $5, got %v (before=%d)", got, before)
	}
}

// TestGetInboxMarksRead ensures get_inbox marks the alias's inbox as read, so
// a subsequent UnreadCount is 0. UnreadCount/MarkRead compare
// clock.NowMillis() timestamps with a strict ">", so this drives a fake,
// strictly-increasing clock to avoid same-millisecond flakiness (see
// internal/store TestUnreadCountAndMarkRead).
func TestGetInboxMarksRead(t *testing.T) {
	var tick int64
	clock.SetForTesting(func() int64 {
		tick++
		return tick
	})
	t.Cleanup(clock.ResetForTesting)

	n := &fakeNotifier{}
	sock, s := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "reviewer", "role": "reviewer", "model_type": "codex", "socket_path": "/s", "session_id": "$5"})
	call(t, sock, "send_message", map[string]any{"from": "backend", "to_kind": "agent", "to_target": "reviewer", "subject": "hi", "body": "x"})
	if n, err := s.UnreadCount("reviewer"); err != nil || n != 1 {
		t.Fatalf("unread before get_inbox = %d (%v), want 1", n, err)
	}
	call(t, sock, "get_inbox", map[string]any{"alias": "reviewer"})
	if got, err := s.UnreadCount("reviewer"); err != nil || got != 0 {
		t.Fatalf("unread after get_inbox = %d (%v), want 0", got, err)
	}
}

// TestReplyNotifiesOriginatorWithUnread is the regression test for originator
// blindness (live incident, 2026-07-16): a reply on a thread must light the
// ORIGINATOR's mailbox with a real unread count — before the participation
// predicate was shared, UnreadCount(originator) was 0 and the notify cleared
// the mailbox instead, leaving the reply invisible on every surface.
func TestReplyNotifiesOriginatorWithUnread(t *testing.T) {
	var tick int64
	clock.SetForTesting(func() int64 {
		tick++
		return tick
	})
	t.Cleanup(clock.ResetForTesting)

	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "api", "model_type": "claude", "socket_path": "/s", "session_id": "$2"})
	resp := call(t, sock, "send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "req", "body": "x"})
	tid := threadIDOf(t, resp)
	call(t, sock, "reply", map[string]any{"thread_id": tid, "from": "api", "body": "done"})

	f := struct {
		sessions []string
		counts   []int
	}{n.snap(&n.notified), func() []int {
		n.mu.Lock()
		defer n.mu.Unlock()
		out := make([]int, len(n.counts))
		copy(out, n.counts)
		return out
	}()}
	// First notify: api on the send. Second: web on the reply, count >= 1.
	if len(f.sessions) != 2 || f.sessions[1] != "$1" {
		t.Fatalf("reply must notify the originator's session $1, got %v", f.sessions)
	}
	if f.counts[1] < 1 {
		t.Fatalf("originator notified with count %d — a reply must LIGHT the mailbox, not clear it", f.counts[1])
	}
}

// TestEventsLogged: notify outcomes and inbox reads land in the event log and
// come back via the list_events op.
func TestEventsLogged(t *testing.T) {
	var tick int64
	clock.SetForTesting(func() int64 {
		tick++
		return tick
	})
	t.Cleanup(clock.ResetForTesting)

	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "api", "model_type": "claude", "socket_path": "/s", "session_id": "$2"})
	call(t, sock, "send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "req", "body": "x"})
	call(t, sock, "get_inbox", map[string]any{"alias": "api"})

	resp := call(t, sock, "list_events", map[string]any{"backlog": true, "limit": 50})
	var out struct {
		Events []store.Event `json:"events"`
		MaxID  int64         `json:"max_id"`
	}
	decode(t, resp, &out)
	var sawLit, sawRead bool
	for _, e := range out.Events {
		if e.Kind == "notify" && e.Agent == "api" && e.Detail == "lit" && e.Count == 1 {
			sawLit = true
		}
		if e.Kind == "read" && e.Agent == "api" {
			sawRead = true
		}
	}
	if !sawLit || !sawRead {
		t.Fatalf("event log missing notify-lit or read (lit=%v read=%v): %+v", sawLit, sawRead, out.Events)
	}
}

// TestBusActionsJournalInOrder: one send + reply must produce an interleaved
// journal — send row BEFORE its notify row, reply row BEFORE its notify row —
// and task claim must both journal and notify.
func TestBusActionsJournalInOrder(t *testing.T) {
	n := &fakeNotifier{}
	sock, s := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "api", "model_type": "claude", "socket_path": "/s", "session_id": "$2"})
	resp := call(t, sock, "send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "subj", "body": "x"})
	tid := threadIDOf(t, resp) // helper: unmarshal resp.Data.thread_id (extract from TestReplyNotifiesOriginatorWithUnread)
	call(t, sock, "reply", map[string]any{"thread_id": tid, "from": "api", "body": "done"})

	evs, err := s.Events(store.EventQuery{Backlog: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	// oldest-first for assertion readability
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"send", "notify", "reply", "notify"}
	if len(kinds) != 4 || !slices.Equal(kinds, want) {
		t.Fatalf("journal order = %v, want %v", kinds, want)
	}
	if evs[0].Target != "agent:api" || evs[0].Detail != "subj" || evs[0].Agent != "web" {
		t.Fatalf("send row: %+v", evs[0])
	}
}

func TestTaskClaimJournalsAndNotifies(t *testing.T) {
	n := &fakeNotifier{}
	sock, s := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "api", "model_type": "claude", "socket_path": "/s", "session_id": "$2"})
	resp := call(t, sock, "task_create", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "do it", "body": "x"})
	tid := threadIDOf(t, resp)
	before := len(n.snap(&n.notified))
	call(t, sock, "task_claim", map[string]any{"thread_id": tid, "by": "api"})
	if got := n.snap(&n.notified); len(got) != before+1 || got[len(got)-1] != "$1" {
		t.Fatalf("claim must notify the originator's session, notified=%v", got)
	}
	evs, _ := s.Events(store.EventQuery{Kind: "claim", Backlog: true, Limit: 5})
	if len(evs) != 1 || evs[0].Agent != "api" || evs[0].ThreadID != tid {
		t.Fatalf("claim journal row: %+v", evs)
	}
}

// countsSnap is snap's int-slice counterpart (fakeNotifier.counts has no
// dedicated snap helper — several tests below need it).
func countsSnap(n *fakeNotifier) []int {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]int, len(n.counts))
	copy(out, n.counts)
	return out
}

// TestNotifyCoalescesSiblingAliases: two aliases sharing one exact
// (socket_path, session_id) tuple are ONE actor identity (spec §3) — a
// broadcast that would otherwise fan out to both must produce exactly one
// Notify call and one journaled notify row for their shared session, with
// Count equal to the session's distinct-thread total (1), never 2.
func TestNotifyCoalescesSiblingAliases(t *testing.T) {
	n := &fakeNotifier{}
	sock, s := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "sess-name", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	call(t, sock, "register_agent", map[string]any{"alias": "chosen", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})

	call(t, sock, "send_message", map[string]any{"from": "operator", "to_kind": "broadcast", "subject": "s", "body": "b"})

	notified := n.snap(&n.notified)
	counts := countsSnap(n)
	if len(notified) != 1 || notified[0] != "$9" || counts[0] != 1 {
		t.Fatalf("sibling aliases must coalesce into ONE notify to $9 count 1, got sessions=%v counts=%v", notified, counts)
	}

	evs, err := s.Events(store.EventQuery{Kind: "notify", Backlog: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var litRows int
	for _, e := range evs {
		if e.Detail == "lit" && e.Count == 1 {
			litRows++
		}
	}
	if litRows != 1 {
		t.Fatalf("expected exactly one lit notify journal row, got %d (%+v)", litRows, evs)
	}
}

// TestLit2Regression is the lit(2) fix (spec §3): a peer's messages address
// TWO sibling aliases of the same session on two distinct threads, lighting
// the badge to a distinct-thread count of 2. Draining only ONE alias's inbox
// must rewrite the badge to the remainder (1) — not blind-Clear it, which is
// exactly the bug session-level recompute closes.
func TestLit2Regression(t *testing.T) {
	n := &fakeNotifier{}
	sock, s := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "aliasA", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	call(t, sock, "register_agent", map[string]any{"alias": "aliasB", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	call(t, sock, "register_agent", map[string]any{"alias": "peer", "role": "other", "model_type": "codex", "socket_path": "/p", "session_id": "$1"})

	call(t, sock, "send_message", map[string]any{"from": "peer", "to_kind": "agent", "to_target": "aliasA", "subject": "a", "body": "x"})
	call(t, sock, "send_message", map[string]any{"from": "peer", "to_kind": "agent", "to_target": "aliasB", "subject": "b", "body": "y"})

	total, _, err := s.SessionUnread("/s", "$9")
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("expected 2 distinct unread threads before drain, got %d", total)
	}

	// Drain ONLY aliasA.
	call(t, sock, "get_inbox", map[string]any{"alias": "aliasA"})

	remainder, _, err := s.SessionUnread("/s", "$9")
	if err != nil {
		t.Fatal(err)
	}
	if remainder != 1 {
		t.Fatalf("draining one alias must leave the OTHER alias's thread unread (remainder=1), got %d", remainder)
	}

	notified := n.snap(&n.notified)
	counts := countsSnap(n)
	if len(notified) == 0 || notified[len(notified)-1] != "$9" || counts[len(counts)-1] != 1 {
		t.Fatalf("get_inbox drain must re-Notify $9 with the remainder (1), got notified=%v counts=%v", notified, counts)
	}
}

// TestGetInboxFailsWhenMarkReadFails: if Inbox succeeds but the MarkRead
// persist fails, get_inbox must fail outright — no read event journaled, no
// badge touch — because a read that didn't persist must not report success
// (spec §3). markReadFailingStore is the injectable seam: it wraps a real
// *store.Store and fails MarkRead on command while every other method
// passes through untouched.
type markReadFailingStore struct {
	*store.Store
	failMarkRead bool
}

func (m *markReadFailingStore) MarkRead(alias string) error {
	if m.failMarkRead {
		return fmt.Errorf("injected MarkRead failure")
	}
	return m.Store.MarkRead(alias)
}

func TestGetInboxFailsWhenMarkReadFails(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	realStore, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = realStore.Close() })
	fs := &markReadFailingStore{Store: realStore}

	n := &fakeNotifier{}
	d, err := Serve(paths.SocketPath(), fs, n)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	sock := paths.SocketPath()

	call(t, sock, "register_agent", map[string]any{"alias": "reviewer", "role": "reviewer", "model_type": "codex", "socket_path": "/s", "session_id": "$5"})
	call(t, sock, "send_message", map[string]any{"from": "backend", "to_kind": "agent", "to_target": "reviewer", "subject": "hi", "body": "x"})

	beforeCleared := len(n.snap(&n.cleared))
	beforeNotified := len(n.snap(&n.notified))

	fs.failMarkRead = true
	resp := call(t, sock, "get_inbox", map[string]any{"alias": "reviewer"})
	if resp.OK {
		t.Fatalf("get_inbox must fail when MarkRead fails, got %+v", resp)
	}
	if got := len(n.snap(&n.cleared)); got != beforeCleared {
		t.Fatalf("badge must be untouched when MarkRead fails: cleared grew from %d to %d", beforeCleared, got)
	}
	if got := len(n.snap(&n.notified)); got != beforeNotified {
		t.Fatalf("badge must be untouched when MarkRead fails: notified grew from %d to %d", beforeNotified, got)
	}
	evs, err := realStore.Events(store.EventQuery{Kind: "read", Backlog: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("no read event must be journaled when MarkRead fails, got %+v", evs)
	}
}

// blockingNotifier lets a test hold a session's lock open across a Notify
// call, so a concurrent get_inbox drain can be driven mid-flight
// (TestNotifyDrainInterleaving) — Notify for the armed session blocks until
// the test closes proceed, signaling entered once it starts blocking.
type blockingNotifier struct {
	fakeNotifier
	blockSession string
	proceed      chan struct{}
	entered      chan struct{}
	enteredOnce  sync.Once
}

func (b *blockingNotifier) Notify(socketPath, sessionID string, count int) error {
	if sessionID == b.blockSession {
		b.enteredOnce.Do(func() { close(b.entered) })
		<-b.proceed
	}
	return b.fakeNotifier.Notify(socketPath, sessionID, count)
}

// TestNotifyDrainInterleaving drives the race the per-session lock exists to
// close (spec §3): a reply's notify computes its session recompute and then
// blocks inside Notify (still holding the session lock); while it's blocked,
// a get_inbox drain of the same session runs its Inbox+MarkRead (no lock
// needed) and then blocks on the SAME lock for its own recompute+push.
// Releasing the block lets the reply's (now-stale) Notify land first, but
// the get_inbox recompute — which acquires the lock strictly afterward and
// so sees the drain already applied — must be the one that lands last, i.e.
// the true post-drain state must win, not the stale count.
func TestNotifyDrainInterleaving(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	n := &blockingNotifier{proceed: make(chan struct{}), entered: make(chan struct{})}
	d, err := Serve(paths.SocketPath(), s, n)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	sock := paths.SocketPath()

	call(t, sock, "register_agent", map[string]any{"alias": "solo", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	call(t, sock, "register_agent", map[string]any{"alias": "author", "role": "other", "model_type": "codex", "socket_path": "/p", "session_id": "$1"})

	// Seed one unread thread while the block is still disarmed.
	resp := call(t, sock, "send_message", map[string]any{"from": "author", "to_kind": "agent", "to_target": "solo", "subject": "s1", "body": "x"})
	tid := threadIDOf(t, resp)

	// Arm the block for solo's session: the reply below will compute its
	// (stale, pre-drain) recompute, then block inside Notify holding $9's
	// session lock.
	n.blockSession = "$9"
	replyDone := make(chan struct{})
	go func() {
		call(t, sock, "reply", map[string]any{"thread_id": tid, "from": "author", "body": "y"})
		close(replyDone)
	}()
	<-n.entered // reply's Notify($9, ...) is now blocked, holding the lock

	getInboxDone := make(chan struct{})
	go func() {
		call(t, sock, "get_inbox", map[string]any{"alias": "solo"})
		close(getInboxDone)
	}()
	// Give get_inbox's Inbox+MarkRead (unlocked, fast local DB ops) time to
	// complete and reach the session lock before we release the reply.
	time.Sleep(50 * time.Millisecond)
	close(n.proceed)
	<-replyDone
	<-getInboxDone

	remainder, _, err := s.SessionUnread("/s", "$9")
	if err != nil {
		t.Fatal(err)
	}
	if remainder != 0 {
		t.Fatalf("test setup: expected solo fully drained by get_inbox, SessionUnread=%d", remainder)
	}

	// Check combined log to ensure the last operation for $9 is a Clear
	// (not a stale Notify that landed after). This guards against lock removal.
	log := n.snapLog()
	var lastFor9 *notifierCall
	for i := len(log) - 1; i >= 0; i-- {
		if log[i].session == "$9" {
			lastFor9 = &log[i]
			break
		}
	}
	if lastFor9 == nil || lastFor9.kind != "Clear" {
		t.Fatalf("final $9 badge must be the get_inbox drain's Clear (post-drain remainder 0), log=%v", log)
	}
}

// TestSessionAliasesRejectsEmptyTuple: session_aliases requires BOTH
// socket_path and session_id non-empty (spec §3), and on success returns the
// session's aliases sorted.
func TestSessionAliasesRejectsEmptyTuple(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "solo", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	call(t, sock, "register_agent", map[string]any{"alias": "twin", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})

	if resp := call(t, sock, "session_aliases", map[string]any{"socket_path": "", "session_id": "$9"}); resp.OK {
		t.Fatal("session_aliases must reject an empty socket_path")
	}
	if resp := call(t, sock, "session_aliases", map[string]any{"socket_path": "/s", "session_id": ""}); resp.OK {
		t.Fatal("session_aliases must reject an empty session_id")
	}

	resp := call(t, sock, "session_aliases", map[string]any{"socket_path": "/s", "session_id": "$9"})
	if !resp.OK {
		t.Fatalf("session_aliases: %+v", resp)
	}
	var out struct {
		Aliases []string `json:"aliases"`
	}
	decode(t, resp, &out)
	want := []string{"solo", "twin"}
	if !slices.Equal(out.Aliases, want) {
		t.Fatalf("aliases = %v, want %v", out.Aliases, want)
	}
}

// TestSessionUnreadOpRejectsEmptyTupleAndReturnsCounts: the session_unread op
// (spec §3/§4, added for the Stop hook's multi-alias drain wording) requires
// both fields non-empty like session_aliases, and on success returns the
// store's {total, action} pair as-is.
func TestSessionUnreadOpRejectsEmptyTupleAndReturnsCounts(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "worker", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	call(t, sock, "register_agent", map[string]any{"alias": "other", "role": "peer", "model_type": "claude", "socket_path": "/p", "session_id": "$2"})
	call(t, sock, "send_message", map[string]any{"from": "other", "to_kind": "agent", "to_target": "worker", "subject": "s", "body": "b", "intent": "action-requested"})

	if resp := call(t, sock, "session_unread", map[string]any{"socket_path": "", "session_id": "$9"}); resp.OK {
		t.Fatal("session_unread must reject an empty socket_path")
	}
	if resp := call(t, sock, "session_unread", map[string]any{"socket_path": "/s", "session_id": ""}); resp.OK {
		t.Fatal("session_unread must reject an empty session_id")
	}

	resp := call(t, sock, "session_unread", map[string]any{"socket_path": "/s", "session_id": "$9"})
	if !resp.OK {
		t.Fatalf("session_unread: %+v", resp)
	}
	var out struct {
		Total  int `json:"total"`
		Action int `json:"action"`
	}
	decode(t, resp, &out)
	if out.Total != 1 || out.Action != 1 {
		t.Fatalf("session_unread = %+v, want total=1 action=1", out)
	}
}

// TestNotifyForThreadSkipsDepartedAgent: a departed (tombstoned) recipient
// keeps its last-known (socket_path, session_id) tuple on the row (DepartAgent
// never clears it), so without an explicit check it would look exactly like a
// live tmux-identified recipient to notifyForThread. It must instead be
// skipped — no Notify call, no badge write — with its own journaled reason
// distinct from the "no tmux identity" case, since the identity IS known,
// it's just departed.
func TestNotifyForThreadSkipsDepartedAgent(t *testing.T) {
	n := &fakeNotifier{}
	sock, s := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "left", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	call(t, sock, "deregister_agent", map[string]any{"alias": "left"})

	call(t, sock, "send_message", map[string]any{"from": "peer", "to_kind": "agent", "to_target": "left", "subject": "s", "body": "x"})

	if got := n.snap(&n.notified); len(got) != 0 {
		t.Fatalf("a departed agent must never be Notify'd, got %v", got)
	}

	evs, err := s.Events(store.EventQuery{Kind: "notify", Agent: "left", Backlog: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var sawSkip bool
	for _, e := range evs {
		if e.Detail == "skipped: departed" {
			sawSkip = true
		}
	}
	if !sawSkip {
		t.Fatalf("expected a 'skipped: departed' notify journal row for left, got %+v", evs)
	}
}

// TestReRegisterReconcilesOldSessionBadge: re-registering an agent under a
// NEW session tuple must rewrite the OLD tuple's badge too (spec §3), so a
// stale lit flag doesn't survive the move.
func TestReRegisterReconcilesOldSessionBadge(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "roamer", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "author", "role": "other", "model_type": "codex", "socket_path": "/p", "session_id": "$2"})
	call(t, sock, "send_message", map[string]any{"from": "author", "to_kind": "agent", "to_target": "roamer", "subject": "s", "body": "x"})

	notified := n.snap(&n.notified)
	if len(notified) == 0 || notified[len(notified)-1] != "$1" {
		t.Fatalf("setup: expected roamer's session $1 lit, notified=%v", notified)
	}

	// Move roamer to a new session — its OLD session ($1) must be reconciled.
	call(t, sock, "register_agent", map[string]any{"alias": "roamer", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})

	cleared := n.snap(&n.cleared)
	if !slices.Contains(cleared, "$1") {
		t.Fatalf("re-register must reconcile the OLD session $1's badge (Clear, no agents left there), cleared=%v", cleared)
	}
}

// lastAgentSetFor returns the most recent SetAgents push for session, or nil.
func lastAgentSetFor(sets []agentSet, session string) *agentSet {
	for i := len(sets) - 1; i >= 0; i-- {
		if sets[i].session == session {
			return &sets[i]
		}
	}
	return nil
}

// TestRegisterPushesAgentBadge: registering pushes the session's sorted alias
// list; a sibling alias on the same tuple re-pushes the combined list.
func TestRegisterPushesAgentBadge(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "solo", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	got := lastAgentSetFor(n.snapAgentSets(), "$9")
	if got == nil || !slices.Equal(got.aliases, []string{"solo"}) {
		t.Fatalf("register must push [solo] to $9, got %+v", got)
	}
	call(t, sock, "register_agent", map[string]any{"alias": "chosen", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	got = lastAgentSetFor(n.snapAgentSets(), "$9")
	if got == nil || !slices.Equal(got.aliases, []string{"chosen", "solo"}) {
		t.Fatalf("sibling register must push sorted [chosen solo] to $9, got %+v", got)
	}
}

// TestDeregisterUnsetsAgentBadgeWhenLastAliasLeaves: tombstoning the last
// alias pushes an empty list (unset); a surviving sibling keeps the badge
// with the remainder.
func TestDeregisterUnsetsAgentBadgeWhenLastAliasLeaves(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "solo", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	call(t, sock, "register_agent", map[string]any{"alias": "twin", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	call(t, sock, "deregister_agent", map[string]any{"alias": "twin"})
	got := lastAgentSetFor(n.snapAgentSets(), "$9")
	if got == nil || !slices.Equal(got.aliases, []string{"solo"}) {
		t.Fatalf("deregister of one sibling must push remainder [solo], got %+v", got)
	}
	call(t, sock, "deregister_agent", map[string]any{"alias": "solo"})
	got = lastAgentSetFor(n.snapAgentSets(), "$9")
	if got == nil || len(got.aliases) != 0 {
		t.Fatalf("deregister of the last alias must push empty (unset), got %+v", got)
	}
}

// TestPurgeAgentUpdatesAgentBadge: hard-delete reconciles the badge exactly
// like deregister.
func TestPurgeAgentUpdatesAgentBadge(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "gone", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$4"})
	call(t, sock, "purge_agent", map[string]any{"alias": "gone"})
	got := lastAgentSetFor(n.snapAgentSets(), "$4")
	if got == nil || len(got.aliases) != 0 {
		t.Fatalf("purge of the last alias must push empty (unset), got %+v", got)
	}
}

// TestReRegisterMovesAgentBadgeBetweenSessions: an alias moving to a new
// tuple updates BOTH sessions' badges — the old one loses it, the new one
// gains it.
func TestReRegisterMovesAgentBadgeBetweenSessions(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "roamer", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "roamer", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	sets := n.snapAgentSets()
	if got := lastAgentSetFor(sets, "$1"); got == nil || len(got.aliases) != 0 {
		t.Fatalf("old session $1 must end empty after the move, got %+v", got)
	}
	if got := lastAgentSetFor(sets, "$9"); got == nil || !slices.Equal(got.aliases, []string{"roamer"}) {
		t.Fatalf("new session $9 must end with [roamer], got %+v", got)
	}
}

// TestRegisterWithoutTmuxTuplePushesNoAgentBadge: no tuple → nothing to push.
func TestRegisterWithoutTmuxTuplePushesNoAgentBadge(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "headless", "role": "peer", "model_type": "claude"})
	if sets := n.snapAgentSets(); len(sets) != 0 {
		t.Fatalf("tuple-less register must push no agent badge, got %+v", sets)
	}
}
