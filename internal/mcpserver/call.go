// Package mcpserver exposes muster's daemon operations as MCP tools over stdio.
package mcpserver

import (
	"encoding/json"
	"fmt"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
)

// rosterRow is the full-fidelity decode of a list_agents row — the fields
// AgentView (the tool-facing shape) deliberately omits but identity guards
// need. Tags match the daemon's snake_case store JSON.
type rosterRow struct {
	Alias      string `json:"alias"`
	ModelType  string `json:"model_type"`
	SocketPath string `json:"socket_path"`
	PaneID     string `json:"pane_id"`
	SessionID  string `json:"session_id"`
	// SessionCreated is the incarnation half of tmux identity (#{session_created},
	// unix seconds — see tmuxenv.Capture.SessionCreated). tmux recycles session
	// IDs from $0 across server restarts, so a (socket_path, session_id, pane_id)
	// tuple match alone cannot tell a live registration from a stale un-reaped
	// row left behind by a dead server incarnation that happened to reuse the
	// same IDs. 0 = unknown (a pre-upgrade row, or one captured outside tmux).
	SessionCreated int64  `json:"session_created"`
	Label          string `json:"label"`
	Departed       bool   `json:"departed"`
}

// paneRegistration returns the calling pane's own live registration: the
// non-departed roster row matching this exact (socket_path, session_id,
// pane_id) tuple AND, when both sides know it, the same session incarnation
// (session_created — see rosterRow.SessionCreated). A row whose recorded
// creation time differs from the caller's live one is a ghost left by a
// recycled session ID, not a match. ok=false outside tmux, on any
// daemon/decode failure (guards degrade open — today's behavior), when no
// row matches the tuple, or when a tuple-matching row is a ghost.
func paneRegistration(socketPath, sessionID, paneID string, sessionCreated int64) (rosterRow, bool) {
	if socketPath == "" || sessionID == "" || paneID == "" {
		return rosterRow{}, false
	}
	raw, err := callDaemon("list_agents", nil)
	if err != nil {
		return rosterRow{}, false
	}
	var rows []rosterRow
	if json.Unmarshal(raw, &rows) != nil {
		return rosterRow{}, false
	}
	for _, r := range rows {
		if !r.Departed && r.SocketPath == socketPath && r.SessionID == sessionID && r.PaneID == paneID {
			if r.SessionCreated != 0 && sessionCreated != 0 && r.SessionCreated != sessionCreated {
				continue // ghost: same tuple, different session incarnation
			}
			return r, true
		}
	}
	return rosterRow{}, false
}

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

// ThreadView is the tool-facing shape of a message/task thread. LastFrom,
// LastAt, EntryCount, and Unread are query-time annotations the daemon's
// get_inbox (and get_thread, for its own thread) populate from store.Thread —
// zero-valued when a surface doesn't compute them, so they're additive
// fields, not a breaking change to existing callers.
type ThreadView struct {
	ID         int64  `json:"id" jsonschema:"the thread id"`
	Kind       string `json:"kind" jsonschema:"message or task"`
	FromAgent  string `json:"from_agent" jsonschema:"who created the thread"`
	ToKind     string `json:"to_kind" jsonschema:"agent, role, or broadcast"`
	ToTarget   string `json:"to_target" jsonschema:"the addressed alias or role"`
	Subject    string `json:"subject" jsonschema:"the thread subject"`
	Ref        string `json:"ref" jsonschema:"a pointer to the work (repo/branch/endpoint/file)"`
	Status     string `json:"status" jsonschema:"task status, empty for messages"`
	CreatedAt  int64  `json:"created_at" jsonschema:"creation time (unix ms)"`
	UpdatedAt  int64  `json:"updated_at" jsonschema:"last-update time (unix ms)"`
	LastFrom   string `json:"last_from" jsonschema:"who wrote the thread's most recent entry"`
	LastAt     int64  `json:"last_at" jsonschema:"when the most recent entry was written (unix ms)"`
	EntryCount int    `json:"entry_count" jsonschema:"total entries in the thread"`
	Unread     int    `json:"unread" jsonschema:"entries after your last read that you didn't write yourself; 0 means you've seen everything"`
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
