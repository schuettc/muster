package daemon

import (
	"testing"
)

// TestSetActivityRejectsUnknownState: only working and idle are states; any
// other value fails and writes nothing.
func TestSetActivityRejectsUnknownState(t *testing.T) {
	sock, s := startWithNotifierAndStore(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "me", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1"})
	resp := call(t, sock, "set_activity", map[string]any{"aliases": []string{"me"}, "state": "busy"})
	if resp.OK {
		t.Fatalf("state busy must be refused: %+v", resp)
	}
	if a, _, _ := s.GetAgent("me"); a.Activity != "" || a.ActivityAt != 0 {
		t.Fatalf("refused call wrote activity %q@%d", a.Activity, a.ActivityAt)
	}
}

// TestSetActivityUpdatesEachAlias: every known alias is stamped and counted;
// an unknown one is skipped, not an error.
func TestSetActivityUpdatesEachAlias(t *testing.T) {
	sock, s := startWithNotifierAndStore(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "a", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1"})
	call(t, sock, "register_agent", map[string]any{"alias": "b", "socket_path": "/s", "session_id": "$2", "session_created": 5, "pane_id": "%2"})
	resp := call(t, sock, "set_activity", map[string]any{"aliases": []string{"a", "b", "ghost"}, "state": "working"})
	if !resp.OK {
		t.Fatalf("set_activity: %+v", resp)
	}
	m, _ := resp.Data.(map[string]any)
	if n, _ := m["updated"].(float64); n != 2 {
		t.Fatalf("updated = %v, want 2", m["updated"])
	}
	for _, alias := range []string{"a", "b"} {
		if a, _, _ := s.GetAgent(alias); a.Activity != "working" || a.ActivityAt == 0 {
			t.Fatalf("%s activity %q@%d, want working", alias, a.Activity, a.ActivityAt)
		}
	}
	if _, found, _ := s.GetAgent("ghost"); found {
		t.Fatalf("set_activity created a phantom row")
	}
}

// TestGetInboxOwnedMarksWorking: an owned read means the session has picked
// up its mail, so it is working.
func TestGetInboxOwnedMarksWorking(t *testing.T) {
	sock, s := startWithNotifierAndStore(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "me", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1"})
	resp := call(t, sock, "get_inbox", map[string]any{
		"alias": "me", "caller_socket_path": "/s", "caller_session_id": "$1", "caller_session_created": 5,
	})
	if !resp.OK {
		t.Fatalf("get_inbox: %+v", resp)
	}
	if a, _, _ := s.GetAgent("me"); a.Activity != "working" || a.ActivityAt == 0 {
		t.Fatalf("owned get_inbox left activity %q@%d, want working", a.Activity, a.ActivityAt)
	}
}

// TestGetInboxPeekLeavesActivity: a peek (station, a coordinator reading
// another alias) says nothing about the peeked session.
func TestGetInboxPeekLeavesActivity(t *testing.T) {
	sock, s := startWithNotifierAndStore(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "me", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1"})
	call(t, sock, "register_agent", map[string]any{"alias": "peer", "socket_path": "/s", "session_id": "$2", "session_created": 5, "pane_id": "%2"})
	if err := s.SetActivity("me", "idle"); err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.GetAgent("me")
	resp := call(t, sock, "get_inbox", map[string]any{
		"alias": "me", "caller_socket_path": "/s", "caller_session_id": "$2", "caller_session_created": 5,
	})
	if !resp.OK {
		t.Fatalf("get_inbox: %+v", resp)
	}
	if a, _, _ := s.GetAgent("me"); a.Activity != before.Activity || a.ActivityAt != before.ActivityAt {
		t.Fatalf("peek changed activity %q@%d -> %q@%d", before.Activity, before.ActivityAt, a.Activity, a.ActivityAt)
	}
}
