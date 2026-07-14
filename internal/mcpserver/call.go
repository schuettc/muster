// Package mcpserver exposes muster's daemon operations as MCP tools over stdio.
package mcpserver

import (
	"encoding/json"
	"fmt"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
)

// callDaemon sends one op to the daemon (lazily starting it) and returns the
// response Data as JSON, or an error if the transport failed or the daemon
// reported !OK. It is a package-level var so tests can stub it.
var callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
	resp, err := client.Call(paths.SocketPath(), proto.Request{Op: op, Args: args})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s: %s", op, resp.Error)
	}
	b, err := json.Marshal(resp.Data)
	if err != nil {
		return nil, fmt.Errorf("marshal %s result: %w", op, err)
	}
	return b, nil
}

// AgentView is the tool-facing shape of a registered agent. Field tags match
// the daemon's JSON case-insensitively.
type AgentView struct {
	Alias        string `json:"alias" jsonschema:"the agent's addressable alias"`
	Role         string `json:"role" jsonschema:"the agent's role (producer, consumer, reviewer, ...)"`
	ModelType    string `json:"model_type" jsonschema:"the agent's model (claude or codex)"`
	SessionName  string `json:"session_name" jsonschema:"the tmux session the agent runs in"`
	RegisteredAt int64  `json:"registered_at" jsonschema:"when the agent first registered (unix ms)"`
	LastSeen     int64  `json:"last_seen" jsonschema:"when the agent was last active (unix ms)"`
}

// ThreadView is the tool-facing shape of a message/task thread.
type ThreadView struct {
	ID        int64  `json:"id" jsonschema:"the thread id"`
	Kind      string `json:"kind" jsonschema:"message or task"`
	FromAgent string `json:"from_agent" jsonschema:"who created the thread"`
	ToKind    string `json:"to_kind" jsonschema:"agent, role, or broadcast"`
	ToTarget  string `json:"to_target" jsonschema:"the addressed alias or role"`
	Subject   string `json:"subject" jsonschema:"the thread subject"`
	Ref       string `json:"ref" jsonschema:"a pointer to the work (repo/branch/endpoint/file)"`
	Status    string `json:"status" jsonschema:"task status, empty for messages"`
	CreatedAt int64  `json:"created_at" jsonschema:"creation time (unix ms)"`
	UpdatedAt int64  `json:"updated_at" jsonschema:"last-update time (unix ms)"`
}

// EntryView is one append-only entry within a thread.
type EntryView struct {
	ID           int64  `json:"id" jsonschema:"the entry id"`
	ThreadID     int64  `json:"thread_id" jsonschema:"the parent thread id"`
	FromAgent    string `json:"from_agent" jsonschema:"who wrote this entry"`
	Body         string `json:"body" jsonschema:"the entry text"`
	StatusChange string `json:"status_change" jsonschema:"the status this entry set, if any"`
	CreatedAt    int64  `json:"created_at" jsonschema:"when the entry was written (unix ms)"`
}
