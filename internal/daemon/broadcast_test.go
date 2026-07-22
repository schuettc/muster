package daemon

import (
	"strings"
	"testing"
)

// Scoped broadcast to a project nobody (non-departed) is registered under
// must be rejected at the daemon — the black-hole backstop, same principle
// as resolveAgentTarget for mistyped aliases.
func TestScopedBroadcastUnknownProjectRejected(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1"})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "web1", "to_kind": "broadcast", "to_target": "wbe", "subject": "typo", "body": "x",
	})
	if resp.OK {
		t.Fatalf("expected rejection for unknown project, got OK")
	}
	if !strings.Contains(resp.Error, `no registered agents in project "wbe"`) || !strings.Contains(resp.Error, "known projects: web") {
		t.Fatalf("error should name the project and list known ones, got: %q", resp.Error)
	}
}

func TestScopedBroadcastKnownProjectAccepted(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "web2", "project": "web", "socket_path": "/s", "session_id": "$2"})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "web1", "to_kind": "broadcast", "to_target": "web", "subject": "ok", "body": "x",
	})
	if !resp.OK {
		t.Fatalf("scoped broadcast to a live project should succeed: %s", resp.Error)
	}
}

func TestScopedBroadcastDepartedOnlyProjectRejected(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "solo", "project": "api", "socket_path": "/s", "session_id": "$2"})
	call(t, sock, "deregister_agent", map[string]any{"alias": "solo"})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "web1", "to_kind": "broadcast", "to_target": "api", "subject": "tombstones", "body": "x",
	})
	if resp.OK {
		t.Fatalf("broadcast to a departed-only project should be rejected")
	}
}

func TestGlobalBroadcastNeverValidated(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1"})
	resp := call(t, sock, "send_message", map[string]any{
		"from": "web1", "to_kind": "broadcast", "to_target": "", "subject": "all", "body": "x",
	})
	if !resp.OK {
		t.Fatalf("global broadcast must not be validated: %s", resp.Error)
	}
}

func TestScopedBroadcastTaskCreateValidatedToo(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1"})
	resp := call(t, sock, "task_create", map[string]any{
		"from": "web1", "to_kind": "broadcast", "to_target": "nope", "subject": "t", "body": "x",
	})
	if resp.OK {
		t.Fatalf("task_create must validate scoped broadcast targets too")
	}
}
