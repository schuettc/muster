package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SendMessageIn is the input to send_message.
type SendMessageIn struct {
	From     string `json:"from" jsonschema:"the sending agent's alias"`
	ToKind   string `json:"to_kind" jsonschema:"agent, role, or broadcast"`
	ToTarget string `json:"to_target,omitempty" jsonschema:"the recipient alias or role; for broadcast: empty reaches every agent on the bus, or a project name reaches only that project's agents (unknown projects are rejected)"`
	Subject  string `json:"subject" jsonschema:"a short subject line"`
	Ref      string `json:"ref,omitempty" jsonschema:"optional pointer to the work (repo/branch/endpoint/file)"`
	Body     string `json:"body" jsonschema:"the message body"`
	Intent   string `json:"intent,omitempty" jsonschema:"fyi | reply-requested | action-requested; mark FYIs so recipients' drains stay cheap — an FYI doesn't demand a reply. Leave empty when the message's urgency is unspecified."`
}

// ThreadIDOut is the output of send_message and task_create.
type ThreadIDOut struct {
	ThreadID int64 `json:"thread_id" jsonschema:"the created thread's id"`
}

// ReplyIn is the input to reply.
type ReplyIn struct {
	ThreadID int64  `json:"thread_id" jsonschema:"the thread to reply to"`
	From     string `json:"from" jsonschema:"the replying agent's alias"`
	Body     string `json:"body" jsonschema:"the reply text"`
}

// EntryIDOut is the output of reply.
type EntryIDOut struct {
	EntryID int64 `json:"entry_id" jsonschema:"the created entry's id"`
}

// GetInboxIn is the input to get_inbox.
type GetInboxIn struct {
	Alias string `json:"alias" jsonschema:"the agent whose inbox to read"`
}

// GetInboxOut is the output of get_inbox.
type GetInboxOut struct {
	Threads []ThreadView `json:"threads" jsonschema:"threads that concern the agent: addressed to it, its role, broadcast, or originated by it"`
}

// GetThreadIn is the input to get_thread.
type GetThreadIn struct {
	ThreadID int64 `json:"thread_id" jsonschema:"the thread to fetch"`
}

// GetThreadOut is the output of get_thread.
type GetThreadOut struct {
	Thread  ThreadView  `json:"thread" jsonschema:"the thread"`
	Entries []EntryView `json:"entries" jsonschema:"the thread's entries in order"`
}

func sendMessageHandler(_ context.Context, _ *mcp.CallToolRequest, in SendMessageIn) (*mcp.CallToolResult, ThreadIDOut, error) {
	if err := requireRegisteredFrom(in.From); err != nil {
		return nil, ThreadIDOut{}, err
	}
	raw, err := callDaemon("send_message", map[string]any{
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

func replyHandler(_ context.Context, _ *mcp.CallToolRequest, in ReplyIn) (*mcp.CallToolResult, EntryIDOut, error) {
	if err := requireRegisteredFrom(in.From); err != nil {
		return nil, EntryIDOut{}, err
	}
	raw, err := callDaemon("reply", map[string]any{
		"thread_id": in.ThreadID, "from": in.From, "body": in.Body,
	})
	if err != nil {
		return nil, EntryIDOut{}, err
	}
	var out EntryIDOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, EntryIDOut{}, err
	}
	return nil, out, nil
}

func getInboxHandler(_ context.Context, _ *mcp.CallToolRequest, in GetInboxIn) (*mcp.CallToolResult, GetInboxOut, error) {
	raw, err := callDaemon("get_inbox", map[string]any{"alias": in.Alias})
	if err != nil {
		return nil, GetInboxOut{}, err
	}
	var threads []ThreadView
	if err := json.Unmarshal(raw, &threads); err != nil {
		return nil, GetInboxOut{}, err
	}
	return nil, GetInboxOut{Threads: threads}, nil
}

func getThreadHandler(_ context.Context, _ *mcp.CallToolRequest, in GetThreadIn) (*mcp.CallToolResult, GetThreadOut, error) {
	raw, err := callDaemon("get_thread", map[string]any{"thread_id": in.ThreadID})
	if err != nil {
		return nil, GetThreadOut{}, err
	}
	var out GetThreadOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, GetThreadOut{}, err
	}
	return nil, out, nil
}

// registerMessageTools registers send_message, reply, get_inbox, and
// get_thread on srv.
func registerMessageTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "send_message", Description: "Send a message to another agent (to_kind=agent), a role (to_kind=role), or many agents at once (to_kind=broadcast). A broadcast with empty to_target reaches every agent on the bus; set to_target to a project name to reach only that project's agents. Set intent to fyi/reply-requested/action-requested so the recipient's inbox and drain reflect what you actually need back."}, sendMessageHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "reply", Description: "Append a reply to an existing thread (message or task)."}, replyHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_inbox", Description: "Read the threads that concern an agent — addressed to it (directly, by role, or broadcast) or originated by it, so replies on threads it started show up here — newest first. Rows carry last_from and an unread count — unread > 0 means entries you have not seen; read those threads with get_thread before reporting their state."}, getInboxHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_thread", Description: "Fetch a single thread and all its entries in order."}, getThreadHandler)
}
