package daemon

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/mustertest"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

// startTestDaemon boots a real in-process daemon on a temp socket.
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
	d, err := Serve(paths.SocketPath(), s, nil)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return paths.SocketPath()
}

func TestDaemonRegisterAndList(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	sock := filepath.Join(dir, "sock")
	d, err := Serve(sock, s, nil)
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

// TestTaskClaimAcceptsStringThreadID mimics the debug CLI, which sends all
// arg values as strings (not JSON numbers). Before Fix 1, i64() only
// understood float64 and silently coerced the string thread_id to 0,
// so the claim landed on thread 0 instead of the real thread.
func TestTaskClaimAcceptsStringThreadID(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	sock := filepath.Join(dir, "sock")
	d, err := Serve(sock, s, nil)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	reg, err := client.Call(sock, proto.Request{Op: "register_agent", Args: map[string]any{
		"alias": "rev1", "role": "reviewer", "model_type": "claude",
	}})
	if err != nil || !reg.OK {
		t.Fatalf("register: err=%v resp=%+v", err, reg)
	}

	create, err := client.Call(sock, proto.Request{Op: "task_create", Args: map[string]any{
		"from": "backend", "to_kind": "role", "to_target": "reviewer",
		"subject": "x", "body": "y",
	}})
	if err != nil || !create.OK {
		t.Fatalf("task_create: err=%v resp=%+v", err, create)
	}
	data, ok := create.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map data, got %T %v", create.Data, create.Data)
	}
	threadIDFloat, ok := data["thread_id"].(float64)
	if !ok {
		t.Fatalf("expected float64 thread_id, got %T %v", data["thread_id"], data["thread_id"])
	}
	threadIDStr := fmt.Sprintf("%d", int64(threadIDFloat))

	claim, err := client.Call(sock, proto.Request{Op: "task_claim", Args: map[string]any{
		"thread_id": threadIDStr, "by": "rev1",
	}})
	if err != nil {
		t.Fatalf("task_claim: err=%v", err)
	}
	if !claim.OK {
		t.Fatalf("expected task_claim to succeed with string thread_id, got resp=%+v", claim)
	}
}

// decodeGetAgent re-marshals a get_agent response's Data (already a
// map[string]any from the wire) into a typed found/agent pair, matching the
// approach internal/humancli uses for the same response shape.
func decodeGetAgent(t *testing.T, resp proto.Response) (store.Agent, bool) {
	t.Helper()
	raw, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatalf("marshal resp.Data: %v", err)
	}
	var res struct {
		Found bool        `json:"found"`
		Agent store.Agent `json:"agent"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal get_agent response: %v", err)
	}
	return res.Agent, res.Found
}

func TestRegisterCapturesLabelAndDeregister(t *testing.T) {
	sock := startTestDaemon(t) // existing helper
	if _, err := client.Call(sock, proto.Request{Op: "register_agent", Args: map[string]any{
		"alias": "muster-2", "role": "peer", "model_type": "codex",
		"socket_path": "/s", "session_id": "$1",
		"project": "muster", "label": "frontend", "label_manual": true,
	}}); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Call(sock, proto.Request{Op: "get_agent", Args: map[string]any{"alias": "muster-2"}})
	if err != nil || !resp.OK {
		t.Fatalf("get_agent: %v %+v", err, resp)
	}
	ag, found := decodeGetAgent(t, resp)
	if !found || ag.Project != "muster" || ag.Label != "frontend" || !ag.LabelManual {
		t.Fatalf("expected project/label/label_manual to round-trip, got found=%v agent=%+v", found, ag)
	}

	if _, err := client.Call(sock, proto.Request{Op: "deregister_agent", Args: map[string]any{"alias": "muster-2"}}); err != nil {
		t.Fatal(err)
	}
	resp, err = client.Call(sock, proto.Request{Op: "get_agent", Args: map[string]any{"alias": "muster-2"}})
	if err != nil || !resp.OK {
		t.Fatalf("get_agent after deregister: %v %+v", err, resp)
	}
	if _, found := decodeGetAgent(t, resp); found {
		t.Fatal("expected agent to be gone after deregister_agent")
	}
}
