package daemon

import "testing"

// TestBroadcastReplyNotifiesOriginatorOnly pins the reply-storm fix: the
// INITIAL broadcast fans out to every member, but a member's REPLY wakes only
// the thread's originator — never the whole group again. N members each
// replying once would otherwise fan out N×(N-1) wakes.
func TestBroadcastReplyNotifiesOriginatorOnly(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "lead", "project": "web", "socket_path": "/s", "session_id": "$1", "session_created": 100})
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$2", "session_created": 100})
	call(t, sock, "register_agent", map[string]any{"alias": "web2", "project": "web", "socket_path": "/s", "session_id": "$3", "session_created": 100})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "lead", "to_kind": "broadcast", "to_target": "web",
		"subject": "check-in", "body": "status?", "confirm": true,
	})
	var out struct {
		ThreadID int64 `json:"thread_id"`
	}
	decode(t, resp, &out)

	// The send woke the two members, not the sender.
	sent := n.snap(&n.notified)
	if len(sent) != 2 {
		t.Fatalf("initial broadcast should wake both members, got %v", sent)
	}

	call(t, sock, "reply", map[string]any{"thread_id": out.ThreadID, "from": "web1", "body": "green"})

	after := n.snap(&n.notified)
	added := after[len(sent):]
	if len(added) != 1 || added[0] != "$1" {
		t.Fatalf("a broadcast reply must wake only the originator's session $1, got %v", added)
	}
}

// TestBroadcastRequiresConfirm pins the hard gate: an unconfirmed broadcast
// creates nothing and answers with its blast radius; the same send with
// confirm=true fans out.
func TestBroadcastRequiresConfirm(t *testing.T) {
	n := &fakeNotifier{}
	sock, s := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "lead", "project": "web", "socket_path": "/s", "session_id": "$1", "session_created": 100})
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$2", "session_created": 100})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "lead", "to_kind": "broadcast", "to_target": "web", "subject": "s", "body": "b",
	})
	var gate struct {
		ConfirmRequired bool     `json:"confirm_required"`
		RecipientCount  int      `json:"recipient_count"`
		Recipients      []string `json:"recipients"`
		ThreadID        int64    `json:"thread_id"`
	}
	decode(t, resp, &gate)
	if !gate.ConfirmRequired || gate.ThreadID != 0 {
		t.Fatalf("unconfirmed broadcast must not send: %+v", gate)
	}
	if gate.RecipientCount != 1 || len(gate.Recipients) != 1 || gate.Recipients[0] != "web1" {
		t.Fatalf("blast radius should be exactly [web1] (sender excluded), got count=%d %v", gate.RecipientCount, gate.Recipients)
	}
	if ths, err := s.Threads(10); err != nil || len(ths) != 0 {
		t.Fatalf("no thread may exist before confirm: %v (%d)", err, len(ths))
	}
	if got := n.snap(&n.notified); len(got) != 0 {
		t.Fatalf("an unconfirmed broadcast must wake nobody, got %v", got)
	}

	resp = call(t, sock, "send_message", map[string]any{
		"from": "lead", "to_kind": "broadcast", "to_target": "web",
		"subject": "s", "body": "b", "confirm": true,
	})
	var out struct {
		ThreadID int64 `json:"thread_id"`
	}
	decode(t, resp, &out)
	if out.ThreadID == 0 {
		t.Fatal("confirmed broadcast must create a thread")
	}
	if got := n.snap(&n.notified); len(got) != 1 || got[0] != "$2" {
		t.Fatalf("confirmed broadcast should wake web1's session $2, got %v", got)
	}
}
