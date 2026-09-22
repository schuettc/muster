package daemon

import (
	"testing"

	"github.com/schuettc/muster/internal/store"
)

// TestGetInboxOwnedMarksRead: a caller who proves the tmux tuple that owns
// alias gets a real read — the watermark moves.
func TestGetInboxOwnedMarksRead(t *testing.T) {
	sock, s := startWithNotifierAndStore(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "me", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1"})
	call(t, sock, "register_agent", map[string]any{"alias": "peer", "socket_path": "/s", "session_id": "$2", "session_created": 5, "pane_id": "%2"})
	call(t, sock, "send_message", map[string]any{"from": "peer", "to_kind": "agent", "to_target": "me", "subject": "s", "body": "b"})

	resp := call(t, sock, "get_inbox", map[string]any{
		"alias": "me", "caller_socket_path": "/s", "caller_session_id": "$1", "caller_session_created": 5,
	})
	if !resp.OK {
		t.Fatalf("get_inbox: %+v", resp)
	}
	m, _ := resp.Data.(map[string]any)
	if m["marked_read"] != true {
		t.Fatalf("owned read must mark: %v", m)
	}
	if n, err := s.UnreadCount("me"); err != nil || n != 0 {
		t.Fatalf("unread after owned read = %d, err=%v", n, err)
	}
}

// TestGetInboxUnownedIsAPeek: no proof, another session's proof, or an
// unproven incarnation (session_created=0) must never move the watermark —
// each is a peek that still returns the threads, and the last one leaves a
// journal artifact so a sweep is visible after the fact.
func TestGetInboxUnownedIsAPeek(t *testing.T) {
	sock, s := startWithNotifierAndStore(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "me", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1"})
	call(t, sock, "register_agent", map[string]any{"alias": "peer", "socket_path": "/s", "session_id": "$2", "session_created": 5, "pane_id": "%2"})
	call(t, sock, "send_message", map[string]any{"from": "peer", "to_kind": "agent", "to_target": "me", "subject": "s", "body": "b"})

	for _, args := range []map[string]any{
		{"alias": "me"}, // no proof
		{"alias": "me", "caller_socket_path": "/s", "caller_session_id": "$2", "caller_session_created": 5}, // peer's session
		{"alias": "me", "caller_socket_path": "/s", "caller_session_id": "$1", "caller_session_created": 0}, // unproven incarnation
	} {
		resp := call(t, sock, "get_inbox", args)
		if !resp.OK {
			t.Fatalf("%v: get_inbox: %+v", args, resp)
		}
		m, _ := resp.Data.(map[string]any)
		if m["marked_read"] != false {
			t.Fatalf("%v: must be a peek: %v", args, m)
		}
		threads, _ := m["threads"].([]any)
		if len(threads) != 1 {
			t.Fatalf("%v: peek still returns the threads: %v", args, m)
		}
		if n, err := s.UnreadCount("me"); err != nil || n != 1 {
			t.Fatalf("%v: watermark moved, unread=%d err=%v", args, n, err)
		}
	}
	evs, err := s.Events(store.EventQuery{Backlog: true, Limit: 100})
	if err != nil || len(evs) == 0 {
		t.Fatalf("Events: %v %v", evs, err)
	}
	if evs[0].Kind != "peek" {
		t.Fatalf("a peek must leave a journal artifact, newest event %+v", evs[0])
	}
}

// TestGetInboxPeekJournalsCallerIdentity covers Finding 5: the peek event's
// Agent field stays the PEEKED alias (station's per-alias event filter needs
// that), so the caller who actually did the peeking must live in Detail, or
// the artifact never names the sweeper — a sweep of many inboxes would be
// indistinguishable from each peeked alias's own activity.
func TestGetInboxPeekJournalsCallerIdentity(t *testing.T) {
	sock, s := startWithNotifierAndStore(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "me", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1"})
	call(t, sock, "register_agent", map[string]any{"alias": "peer", "socket_path": "/s", "session_id": "$2", "session_created": 5, "pane_id": "%2"})

	resp := call(t, sock, "get_inbox", map[string]any{
		"alias": "me", "caller_socket_path": "/s", "caller_session_id": "$2", "caller_session_created": 5,
	})
	if !resp.OK {
		t.Fatalf("get_inbox: %+v", resp)
	}
	evs, err := s.Events(store.EventQuery{Backlog: true, Limit: 100})
	if err != nil || len(evs) == 0 {
		t.Fatalf("Events: %v %v", evs, err)
	}
	if evs[0].Kind != "peek" || evs[0].Agent != "me" {
		t.Fatalf("peek event must keep Agent=peeked alias, got %+v", evs[0])
	}
	if evs[0].Detail != "peer" {
		t.Fatalf("peek Detail must name the caller's resolved alias, got %q", evs[0].Detail)
	}
}

// TestGetInboxChainSeedIsOwned: a become-retired seed row is still the
// caller's to drain — session lineage includes a departed row that is part
// of a become-chain.
func TestGetInboxChainSeedIsOwned(t *testing.T) {
	sock, _ := startWithNotifierAndStore(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "seed", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1"})
	call(t, sock, "become", map[string]any{"from": "seed", "to": "me"})

	resp := call(t, sock, "get_inbox", map[string]any{
		"alias": "seed", "caller_socket_path": "/s", "caller_session_id": "$1", "caller_session_created": 5,
	})
	if !resp.OK {
		t.Fatalf("get_inbox: %+v", resp)
	}
	m, _ := resp.Data.(map[string]any)
	if m["marked_read"] != true {
		t.Fatal("a become-retired seed is still the caller's to drain")
	}
}

// TestGetInboxPanelessOwnedByHarnessID: a paneless session (empty
// socket_path) proves ownership with its harness UUID instead of a tmux
// tuple.
func TestGetInboxPanelessOwnedByHarnessID(t *testing.T) {
	sock, _ := startWithNotifierAndStore(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "pl", "session_id": "uuid-1", "harness_session_id": "uuid-1"})

	resp := call(t, sock, "get_inbox", map[string]any{"alias": "pl", "caller_harness_session_id": "uuid-1"})
	if !resp.OK {
		t.Fatalf("get_inbox: %+v", resp)
	}
	m, _ := resp.Data.(map[string]any)
	if m["marked_read"] != true {
		t.Fatal("paneless ownership is the harness UUID")
	}
}

// TestMarkReadOperatorDrainClearsWithoutOwnership: the mark_read op is the
// operator surface (station's 'c') — it drains a mailbox for a caller who does
// NOT own it (no caller proof at all), which get_inbox's owned path can never
// do. It returns the count it cleared, moves the watermark to 0, and is
// idempotent (a second drain clears nothing).
func TestMarkReadOperatorDrainClearsWithoutOwnership(t *testing.T) {
	sock, s := startWithNotifierAndStore(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "me", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1"})
	call(t, sock, "register_agent", map[string]any{"alias": "peer", "socket_path": "/s", "session_id": "$2", "session_created": 5, "pane_id": "%2"})
	call(t, sock, "send_message", map[string]any{"from": "peer", "to_kind": "agent", "to_target": "me", "subject": "s", "body": "b"})
	if n, _ := s.UnreadCount("me"); n != 1 {
		t.Fatalf("precondition: unread should be 1, got %d", n)
	}

	// No caller proof — an operator, not 'me' — still drains.
	resp := call(t, sock, "mark_read", map[string]any{"alias": "me"})
	if !resp.OK {
		t.Fatalf("mark_read: %+v", resp)
	}
	m, _ := resp.Data.(map[string]any)
	if cleared, _ := m["cleared"].(float64); cleared != 1 {
		t.Fatalf("cleared = %v, want 1", m["cleared"])
	}
	if n, err := s.UnreadCount("me"); err != nil || n != 0 {
		t.Fatalf("unread after operator drain = %d, err=%v", n, err)
	}

	// Idempotent: a second drain clears nothing.
	resp = call(t, sock, "mark_read", map[string]any{"alias": "me"})
	m, _ = resp.Data.(map[string]any)
	if cleared, _ := m["cleared"].(float64); cleared != 0 {
		t.Fatalf("second drain cleared = %v, want 0", m["cleared"])
	}

	// An unknown alias is refused, not silently a no-op.
	if resp := call(t, sock, "mark_read", map[string]any{"alias": "ghost"}); resp.OK {
		t.Fatalf("mark_read of an unregistered alias must fail, got %+v", resp)
	}
}
