package daemon

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"sync"
	"testing"

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

type fakeNotifier struct {
	mu       sync.Mutex
	notified []string // session IDs Notify'd
	counts   []int    // unread counts carried, aligned with notified
	cleared  []string // session IDs Clear'd
}

func (f *fakeNotifier) Notify(_, sessionID string, count int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notified = append(f.notified, sessionID)
	f.counts = append(f.counts, count)
	return nil
}

func (f *fakeNotifier) Clear(_, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, sessionID)
	return nil
}

func (f *fakeNotifier) snap(which *[]string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(*which))
	copy(out, *which)
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
	call(t, sock, "get_inbox", map[string]any{"alias": "reviewer"})
	got := n.snap(&n.cleared)
	if len(got) != 1 || got[0] != "$5" {
		t.Fatalf("get_inbox should clear reviewer's session $5, got %v", got)
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
