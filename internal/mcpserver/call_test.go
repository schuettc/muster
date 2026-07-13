package mcpserver

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/store"
)

func startTestDaemon(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := daemon.Serve(paths.SocketPath(), s)
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

func TestCallDaemonSurfacesError(t *testing.T) {
	startTestDaemon(t)
	// task_claim on a nonexistent thread → daemon returns !OK → error.
	if _, err := callDaemon("task_claim", map[string]any{"thread_id": "999", "by": "x"}); err == nil {
		t.Fatalf("expected error for claiming nonexistent task")
	}
}
