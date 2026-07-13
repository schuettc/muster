package daemon

import (
	"path/filepath"
	"testing"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

func TestDaemonRegisterAndList(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	sock := filepath.Join(dir, "sock")
	d, err := Serve(sock, s)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	// register_agent
	reg, err := client.Call(sock, proto.Request{Op: "register_agent", Args: map[string]any{
		"alias": "backend", "role": "producer", "model_type": "claude",
	}})
	if err != nil || !reg.OK {
		t.Fatalf("register: err=%v resp=%+v", err, reg)
	}

	// list_agents
	list, err := client.Call(sock, proto.Request{Op: "list_agents"})
	if err != nil || !list.OK {
		t.Fatalf("list: err=%v resp=%+v", err, list)
	}
	agents, ok := list.Data.([]any)
	if !ok || len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %T %v", list.Data, list.Data)
	}
}
