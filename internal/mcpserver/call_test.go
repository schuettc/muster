package mcpserver

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/mustertest"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/store"
)

func startTestDaemon(t *testing.T) string {
	t.Helper()
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := daemon.Serve(paths.SocketPath(), s, nil, "")
	if err != nil {
		t.Fatalf("daemon.Serve: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return paths.SocketPath()
}

func TestCallDaemonRegisterAndList(t *testing.T) {
	startTestDaemon(t)

	if _, err := callDaemon("register_agent", map[string]any{
		"alias": "backend", "role": "producer", "model_type": "claude",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	raw, err := callDaemon("list_agents", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var agents []AgentView
	if err := json.Unmarshal(raw, &agents); err != nil {
		t.Fatalf("unmarshal agents: %v", err)
	}
	if len(agents) != 1 || agents[0].Alias != "backend" || agents[0].Role != "producer" {
		t.Fatalf("unexpected agents: %+v", agents)
	}
}

// TestPaneRegistrationFollowsBecomeChain covers the register_agent
// pane-ownership check crossing a `become` retirement: the pane's tuple is
// stamped on a now-departed seed row whose superseded_by names the alias its
// identity moved onto. The successor's OWN roster row no longer shares that
// tuple (it registered elsewhere since, or moved via a later become of its
// own) — a plain tuple scan over the roster would never find it — so
// paneRegistration must follow the superseded_by link by ALIAS to the live
// successor row and return that, rather than reporting "not registered" for
// a pane whose identity plainly still exists, just moved.
func TestPaneRegistrationFollowsBecomeChain(t *testing.T) {
	prev := callDaemon
	t.Cleanup(func() { callDaemon = prev })
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		if op != "list_agents" {
			t.Fatalf("unexpected op %s", op)
		}
		return json.RawMessage(`[
			{"alias":"seed","departed":true,"superseded_by":"me","socket_path":"/tmp/sock","session_id":"$1","pane_id":"%1"},
			{"alias":"me","departed":false,"superseded_by":"","socket_path":"/tmp/other-sock","session_id":"$2","pane_id":"%2"}
		]`), nil
	}
	row, ok := paneRegistration("/tmp/sock", "$1", "%1", 0)
	if !ok || row.Alias != "me" {
		t.Fatalf("paneRegistration = %+v ok=%v, want alias 'me'", row, ok)
	}
}

// TestPaneRegistrationStopsAtOrdinaryTombstone: a departed row with an EMPTY
// superseded_by is an ordinary tombstone (deregistered, not become-retired) —
// there is no live successor to follow, so the pane reads as unregistered.
func TestPaneRegistrationStopsAtOrdinaryTombstone(t *testing.T) {
	prev := callDaemon
	t.Cleanup(func() { callDaemon = prev })
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		if op != "list_agents" {
			t.Fatalf("unexpected op %s", op)
		}
		return json.RawMessage(`[{"alias":"ghost","departed":true,"superseded_by":"","socket_path":"/tmp/sock","session_id":"$1","pane_id":"%1"}]`), nil
	}
	if row, ok := paneRegistration("/tmp/sock", "$1", "%1", 0); ok {
		t.Fatalf("expected ok=false for a tombstone with no successor, got %+v", row)
	}
}

func TestCallDaemonSurfacesError(t *testing.T) {
	startTestDaemon(t)
	// task_claim on a nonexistent thread → daemon returns !OK → error.
	if _, err := callDaemon("task_claim", map[string]any{"thread_id": "999", "by": "x"}); err == nil {
		t.Fatalf("expected error for claiming nonexistent task")
	}
}
