package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TaskCreateIn is the input to task_create.
type TaskCreateIn struct {
	From     string `json:"from" jsonschema:"the requesting agent's alias"`
	ToKind   string `json:"to_kind" jsonschema:"agent, role, or broadcast"`
	ToTarget string `json:"to_target" jsonschema:"the assignee alias or role"`
	Subject  string `json:"subject" jsonschema:"a short task title"`
	Ref      string `json:"ref,omitempty" jsonschema:"optional pointer to the work (repo/branch/endpoint/file)"`
	Body     string `json:"body" jsonschema:"task details"`
	Intent   string `json:"intent,omitempty" jsonschema:"fyi | reply-requested | action-requested; mark FYIs so recipients' drains stay cheap — an FYI doesn't demand a reply. Left empty, a task is still treated as action-requested (a task is inherently a request for action)."`
}

// TaskClaimIn is the input to task_claim.
type TaskClaimIn struct {
	ThreadID int64  `json:"thread_id" jsonschema:"the task thread to claim"`
	By       string `json:"by" jsonschema:"the alias of the agent claiming the task"`
}

// TaskTransitionIn is the input to task_transition.
type TaskTransitionIn struct {
	ThreadID int64  `json:"thread_id" jsonschema:"the task thread to update"`
	By       string `json:"by" jsonschema:"the alias making the change"`
	Status   string `json:"status" jsonschema:"new status: open, claimed, needs_info, blocked, completed, declined, or cancelled"`
	Note     string `json:"note,omitempty" jsonschema:"optional note recorded with the status change"`
}

func taskCreateHandler(_ context.Context, _ *mcp.CallToolRequest, in TaskCreateIn) (*mcp.CallToolResult, ThreadIDOut, error) {
	raw, err := callDaemon("task_create", map[string]any{
		"from": in.From, "to_kind": in.ToKind, "to_target": in.ToTarget,
		"subject": in.Subject, "ref": in.Ref, "body": in.Body, "intent": in.Intent,
	})
	if err != nil {
		return nil, ThreadIDOut{}, err
	}
	var out ThreadIDOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, ThreadIDOut{}, err
	}
	return nil, out, nil
}

func taskClaimHandler(_ context.Context, _ *mcp.CallToolRequest, in TaskClaimIn) (*mcp.CallToolResult, OKOut, error) {
	if _, err := callDaemon("task_claim", map[string]any{"thread_id": in.ThreadID, "by": in.By}); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true, Detail: "claimed"}, nil
}

func taskTransitionHandler(_ context.Context, _ *mcp.CallToolRequest, in TaskTransitionIn) (*mcp.CallToolResult, OKOut, error) {
	if _, err := callDaemon("task_transition", map[string]any{
		"thread_id": in.ThreadID, "by": in.By, "status": in.Status, "note": in.Note,
	}); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true, Detail: in.Status}, nil
}

// registerTaskTools registers task_create, task_claim, and task_transition
// on srv.
func registerTaskTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "task_create", Description: "Create a task addressed to an agent or role. The assignee(s) can claim and work it. Optional intent (fyi/reply-requested/action-requested) defaults to action-requested — a task is inherently a request for action."}, taskCreateHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "task_claim", Description: "Claim an open task. Only the first claimer succeeds; a second claim fails."}, taskClaimHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "task_transition", Description: "Move a task to a new status (claimed, needs_info, blocked, completed, declined, cancelled) with an optional note."}, taskTransitionHandler)
}
