package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/schuettc/muster/internal/harnessenv"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// SendMessageIn is the input to send_message.
type SendMessageIn struct {
	From     string `json:"from" jsonschema:"the sending agent's alias"`
	ToKind   string `json:"to_kind" jsonschema:"agent, role, or broadcast"`
	ToTarget string `json:"to_target,omitempty" jsonschema:"the recipient: an alias, a 'project:label' pair, or a bare label of an agent in your own project (resolved in that order) — or a role with to_kind=role; for broadcast: empty reaches every agent on the bus, or a project name reaches only that project's agents (unknown projects are rejected)"`
	Subject  string `json:"subject" jsonschema:"a short subject line"`
	Ref      string `json:"ref,omitempty" jsonschema:"optional pointer to the work (repo/branch/endpoint/file)"`
	Body     string `json:"body" jsonschema:"the message body"`
	Intent   string `json:"intent,omitempty" jsonschema:"fyi | reply-requested | action-requested; mark FYIs so recipients' drains stay cheap — an FYI doesn't demand a reply. Leave empty when the message's urgency is unspecified."`
	Standing bool   `json:"standing,omitempty" jsonschema:"broadcast-only: a plain broadcast reaches only sessions live now; set standing=true to ALSO reach sessions that start later, once, until they read it. Use for durable standing orders (e.g. 'read CONTRACT.md before editing'), NOT for transient holds — those should stay live-only so they evaporate for future sessions."`
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
	FYI      bool   `json:"fyi,omitempty" jsonschema:"closing note: the entry lands on the thread but wakes nobody — recipients see it on their next natural inbox check. Use for acks, wrap-ups, and any reply that needs nothing back."`
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
	// MarkedRead mirrors the daemon's marked_read (spec 2026-08-21 §3.2):
	// true when the caller proved ownership of alias and its read watermark
	// moved; false when this was a harmless peek that changed nothing.
	MarkedRead bool `json:"marked_read" jsonschema:"true if this read moved alias's read watermark (you proved you are this session); false if it was a peek that changed nothing"`
	// Detail carries the peek notice when MarkedRead is false — see
	// getInboxHandler.
	Detail string `json:"detail,omitempty" jsonschema:"present only on a peek: explains that alias's unread state was not changed by this read"`
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
		"standing": in.Standing,
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
		"thread_id": in.ThreadID, "from": in.From, "body": in.Body, "fyi": in.FYI,
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
	// The caller's tmux/harness identity is the proof the daemon's
	// callerOwns check needs (spec 2026-08-21 §3.2) to move alias's read
	// watermark — sourced exactly like register_agent's own mint sites
	// (tmuxenv.CaptureEnv for the tuple, harnessenv.FromEnv for the harness
	// UUID). No caller_device_id: the register path never sends one either
	// (it is stamped server-side only for a fixed set of session-scoped ops
	// in remote mode — see daemon.deviceOps), so a local client mirrors that
	// by omission. No caller_pane_id: ownership here is session-granular, not
	// pane-granular (daemon commit 5a79d0e).
	c := tmuxenv.CaptureEnv()
	h := harnessenv.FromEnv()
	raw, err := callDaemon("get_inbox", map[string]any{
		"alias":                     in.Alias,
		"caller_socket_path":        c.SocketPath,
		"caller_session_id":         c.SessionID,
		"caller_session_created":    c.SessionCreated,
		"caller_harness_session_id": h.SessionID,
	})
	if err != nil {
		return nil, GetInboxOut{}, err
	}
	threads, markedRead, err := decodeInboxResponse(raw)
	if err != nil {
		return nil, GetInboxOut{}, err
	}
	result := GetInboxOut{Threads: threads, MarkedRead: markedRead}
	if !markedRead {
		result.Detail = fmt.Sprintf("peek only — '%s' is not this session's; its unread state is unchanged", in.Alias)
	}
	return nil, result, nil
}

// decodeInboxResponse decodes a get_inbox payload, tolerating BOTH shapes
// (Finding 2): a 0.13.0+ daemon returns {threads, marked_read}, but a client
// upgraded ahead of a still-running pre-0.13.0 daemon (or a device forwarding
// to a not-yet-redeployed hosted lambda) still gets the old bare-array shape.
// Decoding that as marked_read=true matches the old daemon's actual
// behavior — every read moved the watermark — so this is not a guess, it's
// what happened. Deliberately no version handshake: this is decode-only.
func decodeInboxResponse(raw []byte) ([]ThreadView, bool, error) {
	var body struct {
		Threads    []ThreadView `json:"threads"`
		MarkedRead bool         `json:"marked_read"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		return body.Threads, body.MarkedRead, nil
	}
	var legacy []ThreadView
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil, false, err
	}
	return legacy, true, nil
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
	mcp.AddTool(srv, &mcp.Tool{Name: "send_message", Description: "Send a message to another agent (to_kind=agent), a role (to_kind=role), or many agents at once (to_kind=broadcast). A broadcast with empty to_target reaches every agent on the bus; set to_target to a project name to reach only that project's agents. A plain broadcast is live-only (reaches sessions live now, invisible to sessions that start later); set standing=true to also reach future sessions once, until read — for durable standing orders, not transient holds. Set intent to fyi/reply-requested/action-requested so the recipient's inbox and drain reflect what you actually need back."}, sendMessageHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "reply", Description: "Append a reply to an existing thread (message or task). Reply only when the sender needs something from you; never reply just to acknowledge an ack or a closure — the last word is free. For a closing note that needs nothing back, set fyi=true so the entry lands without waking anyone."}, replyHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_inbox", Description: "Read the threads that concern an agent — addressed to it (directly, by role, or broadcast) or originated by it, so replies on threads it started show up here — newest first. Rows carry last_from and an unread count — unread > 0 means entries you have not seen; read those threads with get_thread before reporting their state."}, getInboxHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_thread", Description: "Fetch a single thread and all its entries in order."}, getThreadHandler)
}
