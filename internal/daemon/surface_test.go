package daemon

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

// snapAgentsTable reads every column of the agents table, ordered by alias,
// so a before/after byte-identical comparison can catch a stray write
// (read-state, last_seen, anything) list_threads must never make.
func snapAgentsTable(t *testing.T, s *store.Store) []store.Agent {
	t.Helper()
	agents, err := s.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	return agents
}

// TestListThreadsMarksNothingRead is the side-effect-free regression test
// (spec §1): list_threads must never mark anything read, never write a tmux
// badge, never journal. Snapshot the agents table (read-state included) and
// the fake notifier's call log before and after — both must be byte-
// identical.
func TestListThreadsMarksNothingRead(t *testing.T) {
	n := &fakeNotifier{}
	sock, s := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web", "role": "producer", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "api", "role": "consumer", "model_type": "claude", "socket_path": "/s", "session_id": "$2"})
	call(t, sock, "send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "hi", "body": "x"})

	before := snapAgentsTable(t, s)
	beforeLog := n.snapLog()
	evsBefore, err := s.Events(store.EventQuery{Backlog: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}

	resp := call(t, sock, "list_threads", map[string]any{"limit": 10})
	if !resp.OK {
		t.Fatalf("list_threads: %+v", resp)
	}
	var out struct {
		Threads []store.Thread `json:"threads"`
	}
	decode(t, resp, &out)
	if len(out.Threads) != 1 {
		t.Fatalf("expected 1 thread, got %+v", out.Threads)
	}

	after := snapAgentsTable(t, s)
	afterLog := n.snapLog()
	evsAfter, err := s.Events(store.EventQuery{Backlog: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(before, after) {
		t.Fatalf("agents table changed by list_threads:\nbefore=%+v\nafter=%+v", before, after)
	}
	if !slices.Equal(beforeLog, afterLog) {
		t.Fatalf("notifier log changed by list_threads:\nbefore=%+v\nafter=%+v", beforeLog, afterLog)
	}
	if !slices.Equal(evsBefore, evsAfter) {
		t.Fatalf("journal changed by list_threads:\nbefore=%+v\nafter=%+v", evsBefore, evsAfter)
	}
}

// TestIntentPassThroughAndRejection: send_message/task_create pass their
// "intent" arg into CreateThread, which validates it (spec §2) — a valid
// intent round-trips through get_thread, and an unknown one is rejected with
// the store's validation error surfacing as a failed response (never a
// silently-dropped/defaulted value).
func TestIntentPassThroughAndRejection(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "api", "model_type": "claude"})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "web", "to_kind": "agent", "to_target": "api", "subject": "s", "body": "b", "intent": "fyi",
	})
	if !resp.OK {
		t.Fatalf("send_message with valid intent should succeed: %+v", resp)
	}
	tid := threadIDOf(t, resp)

	thrResp := call(t, sock, "get_thread", map[string]any{"thread_id": tid})
	if !thrResp.OK {
		t.Fatalf("get_thread: %+v", thrResp)
	}
	var thrOut struct {
		Thread store.Thread `json:"thread"`
	}
	decode(t, thrResp, &thrOut)
	if thrOut.Thread.Intent != store.IntentFYI {
		t.Fatalf("intent did not pass through send_message: got %q, want %q", thrOut.Thread.Intent, store.IntentFYI)
	}

	// task_create with a valid intent.
	taskResp := call(t, sock, "task_create", map[string]any{
		"from": "web", "to_kind": "agent", "to_target": "api", "subject": "do it", "body": "b", "intent": store.IntentAction,
	})
	if !taskResp.OK {
		t.Fatalf("task_create with valid intent should succeed: %+v", taskResp)
	}
	taskTid := threadIDOf(t, taskResp)
	taskThrResp := call(t, sock, "get_thread", map[string]any{"thread_id": taskTid})
	var taskThrOut struct {
		Thread store.Thread `json:"thread"`
	}
	decode(t, taskThrResp, &taskThrOut)
	if taskThrOut.Thread.Intent != store.IntentAction {
		t.Fatalf("intent did not pass through task_create: got %q, want %q", taskThrOut.Thread.Intent, store.IntentAction)
	}

	// Rejection: an unknown intent must surface the store's validation error
	// as a failed response, not succeed with a defaulted/dropped value.
	badResp := call(t, sock, "send_message", map[string]any{
		"from": "web", "to_kind": "agent", "to_target": "api", "subject": "s", "body": "b", "intent": "urgent",
	})
	if badResp.OK {
		t.Fatalf("send_message with unknown intent should fail, got %+v", badResp)
	}
	badTaskResp := call(t, sock, "task_create", map[string]any{
		"from": "web", "to_kind": "agent", "to_target": "api", "subject": "s", "body": "b", "intent": "urgent",
	})
	if badTaskResp.OK {
		t.Fatalf("task_create with unknown intent should fail, got %+v", badTaskResp)
	}
}

// TestGetThreadPagination: no args returns every entry (back-compat); an
// {offset, limit} pair slices the oldest-first entry list.
func TestGetThreadPagination(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "api", "model_type": "claude"})
	resp := call(t, sock, "send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "s", "body": "e0"})
	tid := threadIDOf(t, resp)
	for _, body := range []string{"e1", "e2", "e3", "e4"} {
		call(t, sock, "reply", map[string]any{"thread_id": tid, "from": "api", "body": body})
	}
	// 5 entries total: e0..e4.

	allResp := call(t, sock, "get_thread", map[string]any{"thread_id": tid})
	all := decodeThreadEntries(t, allResp)
	if len(all) != 5 {
		t.Fatalf("no-args get_thread should return all 5 entries, got %d: %+v", len(all), all)
	}
	bodiesOf := func(entries []store.Entry) []string {
		out := make([]string, len(entries))
		for i, e := range entries {
			out[i] = e.Body
		}
		return out
	}
	if want := []string{"e0", "e1", "e2", "e3", "e4"}; !slices.Equal(bodiesOf(all), want) {
		t.Fatalf("all entries = %v, want %v", bodiesOf(all), want)
	}

	pageResp := call(t, sock, "get_thread", map[string]any{"thread_id": tid, "offset": 1, "limit": 2})
	page := decodeThreadEntries(t, pageResp)
	if want := []string{"e1", "e2"}; !slices.Equal(bodiesOf(page), want) {
		t.Fatalf("offset=1 limit=2 entries = %v, want %v", bodiesOf(page), want)
	}

	tailResp := call(t, sock, "get_thread", map[string]any{"thread_id": tid, "offset": 3})
	tail := decodeThreadEntries(t, tailResp)
	if want := []string{"e3", "e4"}; !slices.Equal(bodiesOf(tail), want) {
		t.Fatalf("offset=3 (no limit) entries = %v, want %v", bodiesOf(tail), want)
	}

	headResp := call(t, sock, "get_thread", map[string]any{"thread_id": tid, "limit": 2})
	head := decodeThreadEntries(t, headResp)
	if want := []string{"e0", "e1"}; !slices.Equal(bodiesOf(head), want) {
		t.Fatalf("limit=2 (no offset) entries = %v, want %v", bodiesOf(head), want)
	}

	beyondResp := call(t, sock, "get_thread", map[string]any{"thread_id": tid, "offset": 100})
	beyond := decodeThreadEntries(t, beyondResp)
	if len(beyond) != 0 {
		t.Fatalf("offset past the end should return empty, got %+v", beyond)
	}
}

// decodeThreadEntries unmarshals a get_thread response's Entries.
func decodeThreadEntries(t *testing.T, resp proto.Response) []store.Entry {
	t.Helper()
	var out struct {
		Entries []store.Entry `json:"entries"`
	}
	decode(t, resp, &out)
	return out.Entries
}

// TestReplyPreviewSanitized: a reply body carrying an ESC/CSI sequence and a
// newline must journal a clean, single-line Detail (spec §4) — the daemon
// pipes reply bodies through display.Sanitize before writing the event row.
func TestReplyPreviewSanitized(t *testing.T) {
	sock, s := startWithNotifierAndStore(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "api", "model_type": "claude"})
	resp := call(t, sock, "send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "s", "body": "x"})
	tid := threadIDOf(t, resp)

	dirtyBody := "\x1b[31mHello\nWorld\x1b[0m"
	call(t, sock, "reply", map[string]any{"thread_id": tid, "from": "api", "body": dirtyBody})

	evs, err := s.Events(store.EventQuery{Kind: "reply", ThreadID: tid, Backlog: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("expected 1 reply event, got %+v", evs)
	}
	want := "Hello World"
	if evs[0].Detail != want {
		t.Fatalf("reply Detail = %q, want %q (sanitized, no ESC/CSI, newline collapsed)", evs[0].Detail, want)
	}
}

// TestEffectiveIntentAcrossSurfaces: an old-style task row (stored intent ""
// — pre-migration shape) must read as action-requested via ALL THREE
// surfaces the daemon exposes: list_threads, get_inbox, and get_thread (the
// ledger decision in models.go/threads.go — one vocabulary everywhere).
func TestEffectiveIntentAcrossSurfaces(t *testing.T) {
	sock, s := startWithNotifierAndStore(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "reviewer", "role": "reviewer", "model_type": "claude"})

	res, err := s.DB().Exec(`
INSERT INTO threads (kind, from_agent, to_kind, to_target, subject, ref, status, intent, created_at, updated_at)
VALUES ('task', 'backend', 'role', 'reviewer', 'old task', '', 'open', '', 1, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := res.LastInsertId()
	if _, err := s.DB().Exec(`INSERT INTO entries (thread_id, from_agent, body, created_at) VALUES (?, 'backend', 'please review', 1)`, taskID); err != nil {
		t.Fatal(err)
	}

	// list_threads
	ltResp := call(t, sock, "list_threads", map[string]any{"limit": 50})
	var ltOut struct {
		Threads []store.Thread `json:"threads"`
	}
	decode(t, ltResp, &ltOut)
	var foundInList bool
	for _, th := range ltOut.Threads {
		if th.ID == taskID {
			foundInList = true
			if th.Intent != store.IntentAction {
				t.Fatalf("list_threads: old task intent = %q, want %q", th.Intent, store.IntentAction)
			}
		}
	}
	if !foundInList {
		t.Fatalf("list_threads did not return the old task row: %+v", ltOut.Threads)
	}

	// get_inbox
	inResp := call(t, sock, "get_inbox", map[string]any{"alias": "reviewer"})
	var inOut []store.Thread
	raw, err := json.Marshal(inResp.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &inOut); err != nil {
		t.Fatal(err)
	}
	var foundInInbox bool
	for _, th := range inOut {
		if th.ID == taskID {
			foundInInbox = true
			if th.Intent != store.IntentAction {
				t.Fatalf("get_inbox: old task intent = %q, want %q", th.Intent, store.IntentAction)
			}
		}
	}
	if !foundInInbox {
		t.Fatalf("get_inbox did not return the old task row: %+v", inOut)
	}

	// get_thread
	gtResp := call(t, sock, "get_thread", map[string]any{"thread_id": taskID})
	var gtOut struct {
		Thread store.Thread `json:"thread"`
	}
	decode(t, gtResp, &gtOut)
	if gtOut.Thread.Intent != store.IntentAction {
		t.Fatalf("get_thread: old task intent = %q, want %q", gtOut.Thread.Intent, store.IntentAction)
	}
}

// TestGetInboxUnreadSurvivesOwnMarkRead is the daemon-level defect
// regression: get_inbox's own MarkRead must not destroy the unread evidence
// it just returned. The handler computes Inbox() (which carries the
// caller-relative unread count) BEFORE calling MarkRead — this proves that
// ordering end to end: the FIRST get_inbox call, made after a peer reply,
// must show unread>0 on the replied thread, and a SECOND get_inbox call
// (which sees the watermark the first call's MarkRead just wrote) must show
// unread=0 on that same thread.
func TestGetInboxUnreadSurvivesOwnMarkRead(t *testing.T) {
	sock, _ := startWithNotifierAndStore(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "web", "role": "producer", "model_type": "claude"})
	call(t, sock, "register_agent", map[string]any{"alias": "api", "role": "consumer", "model_type": "claude"})

	sendResp := call(t, sock, "send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "hi", "body": "x"})
	var sendOut struct {
		ThreadID int64 `json:"thread_id"`
	}
	decode(t, sendResp, &sendOut)

	// api replies; web never reads its inbox yet, so the reply is unread
	// for web on the thread it originated (the defect: from_agent on the
	// thread row is web itself, so without per-row unread web can't tell a
	// peer answered).
	call(t, sock, "reply", map[string]any{"thread_id": sendOut.ThreadID, "from": "api", "body": "got it"})

	firstResp := call(t, sock, "get_inbox", map[string]any{"alias": "web"})
	var firstOut []store.Thread
	decode(t, firstResp, &firstOut)
	var first store.Thread
	for _, th := range firstOut {
		if th.ID == sendOut.ThreadID {
			first = th
		}
	}
	if first.LastFrom != "api" {
		t.Fatalf("first get_inbox: last_from = %q, want %q", first.LastFrom, "api")
	}
	if first.Unread != 1 {
		t.Fatalf("first get_inbox: unread = %d, want 1 (must survive this same call's own MarkRead)", first.Unread)
	}

	secondResp := call(t, sock, "get_inbox", map[string]any{"alias": "web"})
	var secondOut []store.Thread
	decode(t, secondResp, &secondOut)
	var second store.Thread
	for _, th := range secondOut {
		if th.ID == sendOut.ThreadID {
			second = th
		}
	}
	if second.Unread != 0 {
		t.Fatalf("second get_inbox: unread = %d, want 0 (first call's MarkRead must have advanced the watermark)", second.Unread)
	}
}
