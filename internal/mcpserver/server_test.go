package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestEndToEndOverMCP boots the real server (all tools) on an in-memory
// transport and drives it with an MCP client — the cross-model scenario:
// create a review task, claim it, complete it, and read it back.
//
// MUSTER_DEVICE_NAME is pinned so register_agent's mint-time seeding
// (device.SeedMinted) is deterministic: the daemon stores "e2e-backend" and
// "e2e-reviewer1", not the bare aliases this test asks for, so every later
// reference to those agents (from/by) must use the seeded form too — the
// same thing a real MCP client does by reading the alias back out of the
// register_agent reply rather than assuming its own input still matches.
//
// The reviewer joins the roster through the daemon, not through a second
// register_agent tool call: a second registration from the SAME pane is
// refused by design (the already-registered guard), so the tool call that used
// to be here created no row and "e2e-reviewer1" was an alias nobody held. That
// was invisible while task_claim accepted any string for `by`; it is not now.
func TestCallerLifecycleEndToEndOverMCP(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "lifecycle")
	t.Setenv("AGENT_SESSION_ID", "lifecycle-session")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	ctx := context.Background()

	srv := mcp.NewServer(&mcp.Implementation{Name: "muster", Version: version}, nil)
	registerAll(srv)
	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = cs.Close() }()
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	seen := map[string]bool{}
	schemas := map[string]map[string]any{}
	for _, tool := range listed.Tools {
		seen[tool.Name] = true
		schemas[tool.Name], _ = tool.InputSchema.(map[string]any)
	}
	if !seen["current_agent"] || !seen["deregister_agent"] || !seen["get_status"] {
		t.Fatalf("caller lifecycle tools not advertised: %v", seen)
	}
	if properties, _ := schemas["deregister_agent"]["properties"].(map[string]any); properties["alias"] != nil || properties["target"] != nil {
		t.Fatalf("deregister_agent must have no target property: %v", schemas["deregister_agent"])
	}
	if !seen["list_tasks"] {
		t.Fatalf("list_tasks not advertised: %v", seen)
	}
	for _, name := range []string{"kv_set", "kv_delete"} {
		if !seen[name] {
			t.Fatalf("%s not advertised: %v", name, seen)
		}
		if properties, _ := schemas[name]["properties"].(map[string]any); properties["by"] != nil {
			t.Fatalf("%s must not expose by: %v", name, schemas[name])
		}
	}
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s transport error: %v", name, err)
		}
		if res.IsError {
			if len(res.Content) > 0 {
				if text, ok := res.Content[0].(*mcp.TextContent); ok {
					t.Fatalf("%s tool error: %s", name, text.Text)
				}
			}
			t.Fatalf("%s tool error: %+v", name, res.Content)
		}
		out, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("%s StructuredContent not an object: %T", name, res.StructuredContent)
		}
		return out
	}

	call("register_agent", map[string]any{"alias": "worker", "role": "worker", "model_type": "claude"})
	current := call("current_agent", map[string]any{})
	if current["registered"] != true {
		t.Fatalf("current_agent = %v", current)
	}
	deregistered := call("deregister_agent", map[string]any{})
	if deregistered["changed"] != float64(1) {
		t.Fatalf("deregister_agent = %v", deregistered)
	}
	current = call("current_agent", map[string]any{})
	if current["registered"] != false {
		t.Fatalf("current_agent after deregistration = %v", current)
	}
	again := call("deregister_agent", map[string]any{})
	if again["changed"] != float64(0) {
		t.Fatalf("second deregister_agent = %v", again)
	}
}

func TestEndToEndOverMCP(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "e2e")
	ctx := context.Background()

	srv := mcp.NewServer(&mcp.Implementation{Name: "muster", Version: version}, nil)
	registerAll(srv)

	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s transport error: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s tool error: %+v", name, res.Content)
		}
		return res
	}

	call("register_agent", map[string]any{"alias": "backend", "role": "producer", "model_type": "claude"})
	if _, err := callDaemon("register_agent", map[string]any{
		"alias": "e2e-reviewer1", "role": "reviewer", "model_type": "codex",
	}); err != nil {
		t.Fatalf("register reviewer: %v", err)
	}
	created := call("task_create", map[string]any{
		"from": "e2e-backend", "to_kind": "role", "to_target": "reviewer",
		"subject": "Review feat/wagers", "ref": "repo=bhw", "body": "please review",
	})
	sc, ok := created.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("task_create StructuredContent not an object: %T", created.StructuredContent)
	}
	tid, ok := sc["thread_id"].(float64)
	if !ok || tid == 0 {
		t.Fatalf("no thread_id in task_create output: %v", sc)
	}
	call("task_claim", map[string]any{"thread_id": tid, "by": "e2e-reviewer1"})
	call("task_transition", map[string]any{"thread_id": tid, "by": "e2e-reviewer1", "status": "completed", "note": "LGTM"})

	got := call("get_thread", map[string]any{"thread_id": tid})
	gsc, _ := got.StructuredContent.(map[string]any)
	thread, _ := gsc["thread"].(map[string]any)
	if thread["status"] != "completed" {
		t.Fatalf("expected completed, got %v", thread["status"])
	}
}
