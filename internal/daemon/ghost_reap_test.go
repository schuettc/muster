package daemon

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/schuettc/muster/internal/store"
)

// getAgentForTest fetches one agent row via the get_agent op, failing the
// test if the alias is unknown.
func getAgentForTest(t *testing.T, sock, alias string) store.Agent {
	t.Helper()
	resp := call(t, sock, "get_agent", map[string]any{"alias": alias})
	data, _ := json.Marshal(resp.Data)
	var out struct {
		Found bool        `json:"found"`
		Agent store.Agent `json:"agent"`
	}
	if err := json.Unmarshal(data, &out); err != nil || !out.Found {
		t.Fatalf("get_agent %s: found=%v err=%v (%s)", alias, out.Found, err, data)
	}
	return out.Agent
}

// TestRegisterReapsRecycledSessionGhost covers the register-time ghost
// reaper end to end: an alias registered under a previous tmux server
// incarnation shares (socket_path, session_id) with today's session (tmux
// numbers sessions from $0 again after a server restart) but carries the old
// incarnation's session_created. Registering from the live session must
// tombstone the ghost and push a badge that no longer lists it — the field
// bug this pins was a session whose @muster_agent badge showed both its own
// alias and a dead predecessor's.
func TestRegisterReapsRecycledSessionGhost(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)

	// The old incarnation of $0 registers, then its server dies (nothing to
	// do on the bus — deregistration never happens; that's the whole bug).
	call(t, sock, "register_agent", map[string]any{
		"alias": "workspace", "socket_path": "/s", "session_id": "$0", "session_created": 100,
	})
	// The new incarnation reuses session ID $0 with a new creation time.
	call(t, sock, "register_agent", map[string]any{
		"alias": "workspace-2", "socket_path": "/s", "session_id": "$0", "session_created": 200,
	})

	got := lastAgentSetFor(n.snapAgentSets(), "$0")
	if got == nil || !slices.Equal(got.aliases, []string{"workspace-2"}) {
		t.Fatalf("badge after re-register must list only the live alias, got %+v", got)
	}
	if a := getAgentForTest(t, sock, "workspace"); !a.Departed {
		t.Fatalf("ghost from the dead incarnation must be tombstoned, got %+v", a)
	}
	if a := getAgentForTest(t, sock, "workspace-2"); a.Departed {
		t.Fatalf("the live registrant must not be tombstoned, got %+v", a)
	}
}

// TestRegisterKeepsTrueSiblings: two agents in ONE live session (same tuple,
// same creation time) are legitimate siblings — the reaper must leave the
// earlier one alone and the badge must list both.
func TestRegisterKeepsTrueSiblings(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{
		"alias": "first", "socket_path": "/s", "session_id": "$0", "session_created": 100,
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "second", "socket_path": "/s", "session_id": "$0", "session_created": 100,
	})
	got := lastAgentSetFor(n.snapAgentSets(), "$0")
	if got == nil || !slices.Equal(got.aliases, []string{"first", "second"}) {
		t.Fatalf("true siblings must both stay on the badge, got %+v", got)
	}
}

// TestRegisterWithoutCreatedSparesSiblings: a registrant that carries no
// session_created (outside tmux, or a pre-upgrade client) has no incarnation
// evidence and must reap nothing.
func TestRegisterWithoutCreatedSparesSiblings(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{
		"alias": "old", "socket_path": "/s", "session_id": "$0", "session_created": 100,
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "new", "socket_path": "/s", "session_id": "$0",
	})
	if a := getAgentForTest(t, sock, "old"); a.Departed {
		t.Fatalf("a created-less register must not reap, got %+v", a)
	}
}

// lastNotifierCallFor returns the most recent Notify/Clear for session, or nil.
func lastNotifierCallFor(log []notifierCall, session string) *notifierCall {
	for i := len(log) - 1; i >= 0; i-- {
		if log[i].session == session {
			return &log[i]
		}
	}
	return nil
}

// TestNotifyBadgeIgnoresGhostIncarnationOrdering pins the badge half of spec
// §5.1: incarnation scopes the unread COUNT, but the tmux badge is still
// keyed (socket, session) — only one incarnation of a session ID can exist in
// tmux at a time. notifyForThread groups recipients by that bare tuple, so
// before the fix the group inherited the SessionCreated of whichever alias
// sorted FIRST: a legacy 0-created ghost sharing a recycled session ID drove
// the recompute, SessionUnread(...,0) returned 0, and the live agent's badge
// got an authoritative Clear — its Stop hook gates on @muster_inbox and would
// never learn about the mail. The alias names here make the ghost sort first
// on purpose; the badge must still be lit for the live incarnation.
func TestNotifyBadgeIgnoresGhostIncarnationOrdering(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	// The ghost registers first, on created 0 — DepartStaleSiblings spares
	// 0-rows, so it survives the live registration that follows.
	call(t, sock, "register_agent", map[string]any{
		"alias": "aaa-ghost", "socket_path": "/s", "session_id": "$1", "session_created": 0,
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "zzz-live", "socket_path": "/s", "session_id": "$1", "session_created": 200,
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "peer", "socket_path": "/p", "session_id": "$2", "session_created": 300,
	})

	call(t, sock, "send_message", map[string]any{"from": "peer", "to_kind": "broadcast", "subject": "s", "body": "for everyone"})

	got := lastNotifierCallFor(n.snapLog(), "$1")
	if got == nil || got.kind != "Notify" || got.count != 1 {
		t.Fatalf("a ghost sharing the tuple must not drive the badge to zero: last call for $1 = %+v, want Notify count 1", got)
	}
}

// TestDeregisterGhostKeepsLiveBadge is the same root cause on the
// reconcileBadge path: deregistering the ghost hands reconcileBadge the
// GHOST's row (created 0) for a tuple a proven live incarnation still
// occupies, and the recompute cleared a badge that had real mail behind it.
func TestDeregisterGhostKeepsLiveBadge(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{
		"alias": "ghost", "socket_path": "/s", "session_id": "$1", "session_created": 0,
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "live", "socket_path": "/s", "session_id": "$1", "session_created": 200,
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "peer", "socket_path": "/p", "session_id": "$2", "session_created": 300,
	})
	call(t, sock, "send_message", map[string]any{"from": "peer", "to_kind": "agent", "to_target": "live", "subject": "s", "body": "real mail"})
	if got := lastNotifierCallFor(n.snapLog(), "$1"); got == nil || got.kind != "Notify" || got.count != 1 {
		t.Fatalf("setup: mail to the live alias must light $1, got %+v", got)
	}

	call(t, sock, "deregister_agent", map[string]any{"alias": "ghost"})

	got := lastNotifierCallFor(n.snapLog(), "$1")
	if got == nil || got.kind != "Notify" || got.count != 1 {
		t.Fatalf("reaping the ghost must leave the live incarnation's badge lit: last call for $1 = %+v, want Notify count 1", got)
	}
}
