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
