package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestEndToEndOverMCP boots the real server (all tools) on an in-memory
// transport and drives it with an MCP client — the cross-model scenario:
// create a review task, claim it, complete it, and read it back.
func TestEndToEndOverMCP(t *testing.T) {
	startTestDaemon(t)
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
	call("register_agent", map[string]any{"alias": "reviewer1", "role": "reviewer", "model_type": "codex"})
	created := call("task_create", map[string]any{
		"from": "backend", "to_kind": "role", "to_target": "reviewer",
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
	call("task_claim", map[string]any{"thread_id": tid, "by": "reviewer1"})
	call("task_transition", map[string]any{"thread_id": tid, "by": "reviewer1", "status": "completed", "note": "LGTM"})

	got := call("get_thread", map[string]any{"thread_id": tid})
	gsc, _ := got.StructuredContent.(map[string]any)
	thread, _ := gsc["thread"].(map[string]any)
	if thread["status"] != "completed" {
		t.Fatalf("expected completed, got %v", thread["status"])
	}
}
