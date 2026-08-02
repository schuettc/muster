package daemon

import (
	"strings"
	"testing"
)

// TestBecomeOpClaimsAndReports: end-to-end over the wire — identity moves,
// seed retires, journal records the claim, unread rides the response so
// surfaces can say "you are now X — N unread".
func TestBecomeOpClaimsAndReports(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/s", "session_id": "$1", "harness_session_id": "uuid-1",
	})
	call(t, sock, "register_agent", map[string]any{"alias": "peer", "socket_path": "/s", "session_id": "$2"})
	call(t, sock, "send_message", map[string]any{
		"from": "peer", "to_kind": "agent", "to_target": "muster-2", "subject": "s", "body": "b",
	})

	resp := call(t, sock, "become", map[string]any{"from": "muster-2", "to": "alias-routing"})
	if !resp.OK {
		t.Fatalf("become: %+v", resp)
	}
	data, _ := resp.Data.(map[string]any)
	if data["to"] != "alias-routing" || data["from"] != "muster-2" {
		t.Fatalf("response = %+v", data)
	}
	if n, _ := data["unread"].(float64); n < 1 {
		t.Fatalf("unread = %v, want >= 1 (pre-claim mail concerns the claimed identity's session)", data["unread"])
	}

	ev := call(t, sock, "list_events", map[string]any{"agent": "alias-routing"})
	if !containsEventDetail(t, ev, "become", "muster-2 → alias-routing") {
		t.Fatalf("no become journal event: %+v", ev.Data)
	}

	resp = call(t, sock, "become", map[string]any{"from": "alias-routing", "to": "peer"})
	if resp.OK || !strings.Contains(resp.Error, "already has history") {
		t.Fatalf("existing-to guard: %+v", resp)
	}
}
