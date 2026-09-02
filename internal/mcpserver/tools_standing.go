package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// StandingSetIn is the input to standing_set.
type StandingSetIn struct {
	From    string `json:"from" jsonschema:"the authoring agent's alias"`
	Project string `json:"project" jsonschema:"the project whose standing orders to set (an agent must be registered under it)"`
	Key     string `json:"key,omitempty" jsonschema:"the order's key within the project; defaults to 'invariants' (the project's single set of golden rules). Use distinct keys only for genuinely separate standing orders."`
	Body    string `json:"body" jsonschema:"the standing order text — the durable instruction every session in the project should read on start"`
}

// StandingRetractIn is the input to standing_retract.
type StandingRetractIn struct {
	From    string `json:"from" jsonschema:"the retracting agent's alias"`
	Project string `json:"project" jsonschema:"the project whose standing order to retract"`
	Key     string `json:"key,omitempty" jsonschema:"the order's key; defaults to 'invariants'"`
}

// StandingListIn is the input to standing_list.
type StandingListIn struct {
	Project string `json:"project" jsonschema:"the project whose live standing orders to list"`
}

// StandingOrderView is one live standing order in a standing_list result.
type StandingOrderView struct {
	Key       string `json:"key" jsonschema:"the order's key within its project"`
	Body      string `json:"body" jsonschema:"the standing order text"`
	From      string `json:"from" jsonschema:"who authored it"`
	ThreadID  int64  `json:"thread_id" jsonschema:"the underlying thread id"`
	CreatedAt int64  `json:"created_at" jsonschema:"when it was authored (unix ms)"`
}

// StandingChangedOut is the output of standing_retract.
type StandingChangedOut struct {
	Changed bool `json:"changed" jsonschema:"true if a live order was retracted; false if there was nothing to retract (idempotent)"`
}

// StandingListOut is the output of standing_list.
type StandingListOut struct {
	Orders []StandingOrderView `json:"orders" jsonschema:"the project's live standing orders, sorted by key"`
}

func standingSetHandler(_ context.Context, _ *mcp.CallToolRequest, in StandingSetIn) (*mcp.CallToolResult, ThreadIDOut, error) {
	if err := requireRegisteredFrom(in.From); err != nil {
		return nil, ThreadIDOut{}, err
	}
	raw, err := callDaemon("standing_set", map[string]any{
		"from": in.From, "project": in.Project, "key": in.Key, "body": in.Body,
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

func standingRetractHandler(_ context.Context, _ *mcp.CallToolRequest, in StandingRetractIn) (*mcp.CallToolResult, StandingChangedOut, error) {
	if err := requireRegisteredFrom(in.From); err != nil {
		return nil, StandingChangedOut{}, err
	}
	raw, err := callDaemon("standing_retract", map[string]any{
		"from": in.From, "project": in.Project, "key": in.Key,
	})
	if err != nil {
		return nil, StandingChangedOut{}, err
	}
	var out StandingChangedOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, StandingChangedOut{}, err
	}
	return nil, out, nil
}

func standingListHandler(_ context.Context, _ *mcp.CallToolRequest, in StandingListIn) (*mcp.CallToolResult, StandingListOut, error) {
	raw, err := callDaemon("standing_list", map[string]any{"project": in.Project})
	if err != nil {
		return nil, StandingListOut{}, err
	}
	var out StandingListOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, StandingListOut{}, err
	}
	return nil, out, nil
}

// registerStandingTools registers the standing-orders convention surface: the
// durable, keyed, per-project standing orders a session should read on start —
// distinct from an ad-hoc send_message broadcast. This is the onboarding
// skill's primary surface (set at project setup, verify via standing_list).
func registerStandingTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "standing_set", Description: "Create or REPLACE a project's standing order (its durable invariants) under a key (default 'invariants'). Idempotent by (project, key): a new set replaces the prior order rather than stacking, and RE-GREETS every session — those running now and those that start later — with the updated text, until each reads it. Use this for a project's golden rules that every session must read on start; use send_message with standing for ad-hoc one-off standing messages."}, standingSetHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "standing_retract", Description: "Retract a project's standing order under a key (default 'invariants') so it greets no future session and drops from standing_list. Idempotent — retracting an absent order is a no-op. A session that already read the order is unaffected."}, standingRetractHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "standing_list", Description: "List a project's live standing orders (key, body, author) — the audit/verify seam: read this to check what greets a new session in the project, and whether the invariants are present and current."}, standingListHandler)
}
