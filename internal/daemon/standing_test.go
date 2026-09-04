package daemon

import (
	"strings"
	"testing"
)

// A standing broadcast is accepted and the standing flag is persisted on the
// thread; live wake fan-out is unchanged (the recipient is notified as for any
// broadcast).
func TestSendStandingBroadcastPersistsFlag(t *testing.T) {
	n := &fakeNotifier{}
	sock, s := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1", "session_created": 100})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "web1", "to_kind": "broadcast", "standing": true, "subject": "order", "body": "read CONTRACT.md", "confirm": true,
	})
	if !resp.OK {
		t.Fatalf("standing broadcast should succeed: %s", resp.Error)
	}
	var out struct {
		ThreadID int64 `json:"thread_id"`
	}
	decode(t, resp, &out)

	th, _, err := s.GetThread(out.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !th.Standing {
		t.Fatal("daemon did not persist Standing on the broadcast thread")
	}
}

// standing is broadcast-only: a standing flag on a directed (agent) message is
// rejected at the daemon before any thread is created.
func TestSendStandingRejectedOnNonBroadcast(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1", "session_created": 100})
	call(t, sock, "register_agent", map[string]any{"alias": "web2", "project": "web", "socket_path": "/s", "session_id": "$2", "session_created": 100})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "web1", "to_kind": "agent", "to_target": "web2", "standing": true, "body": "x",
	})
	if resp.OK {
		t.Fatal("standing on a directed message must be rejected")
	}
	if !strings.Contains(resp.Error, "standing is broadcast-only") {
		t.Fatalf("error should explain the constraint, got: %q", resp.Error)
	}
}
