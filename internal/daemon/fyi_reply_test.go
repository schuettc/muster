package daemon

import (
	"testing"
)

// An fyi reply appends its entry like any reply but must not push a badge to
// anyone: no Notify calls beyond those already caused by earlier activity.
func TestFYIReplySuppressesNotify(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "backend", "role": "producer", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex", "socket_path": "/s", "session_id": "$2"})
	call(t, sock, "send_message", map[string]any{"from": "backend", "to_kind": "agent", "to_target": "consumer", "subject": "hi", "body": "x"})
	before := len(n.snap(&n.notified))

	resp := call(t, sock, "reply", map[string]any{"thread_id": 1, "from": "consumer", "body": "done — no response needed", "fyi": true})
	if !resp.OK {
		t.Fatalf("fyi reply failed: %s", resp.Error)
	}
	if got := n.snap(&n.notified); len(got) != before {
		t.Fatalf("fyi reply must not notify anyone, got new notifies: %v", got[before:])
	}

	// The entry still landed on the thread.
	var thread struct {
		Entries []struct {
			Body string `json:"body"`
		} `json:"entries"`
	}
	decode(t, call(t, sock, "get_thread", map[string]any{"thread_id": 1}), &thread)
	if len(thread.Entries) != 2 {
		t.Fatalf("expected 2 entries after fyi reply, got %d", len(thread.Entries))
	}

	// A plain reply on the same thread still notifies — fyi is per-entry,
	// not sticky thread state.
	call(t, sock, "reply", map[string]any{"thread_id": 1, "from": "consumer", "body": "actually, one question"})
	if got := n.snap(&n.notified); len(got) != before+1 {
		t.Fatalf("plain reply after an fyi reply must still notify, got %v", got)
	}
}
