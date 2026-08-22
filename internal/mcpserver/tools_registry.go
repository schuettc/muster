package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/schuettc/muster/internal/device"
	"github.com/schuettc/muster/internal/harnessenv"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// RegisterAgentIn is the input to register_agent. socket_path/pane_id are NOT
// input — they are captured from the process environment ($TMUX, $TMUX_PANE),
// which the agent's MCP server inherits from its tmux pane.
type RegisterAgentIn struct {
	Alias       string `json:"alias" jsonschema:"a short addressable name for this agent, e.g. backend"`
	Role        string `json:"role" jsonschema:"this agent's role: producer, consumer, reviewer, ..."`
	ModelType   string `json:"model_type" jsonschema:"the model backing this agent: claude, codex, or cursor"`
	SessionName string `json:"session_name,omitempty" jsonschema:"optional tmux session name for display"`
	Become      bool   `json:"become,omitempty" jsonschema:"claim this alias as the session's name: the current alias retires and its identity and read-state carry over"`
}

// OKOut is a simple success acknowledgement for void operations.
type OKOut struct {
	OK     bool   `json:"ok" jsonschema:"whether the operation succeeded"`
	Detail string `json:"detail,omitempty" jsonschema:"optional human-readable detail"`
}

// ListAgentsIn has no fields; list_agents takes no arguments.
type ListAgentsIn struct{}

// ListAgentsOut wraps the agent list (Out must be a struct, not a bare slice).
type ListAgentsOut struct {
	Agents []AgentView `json:"agents" jsonschema:"the registered agents"`
}

func registerAgentHandler(_ context.Context, _ *mcp.CallToolRequest, in RegisterAgentIn) (*mcp.CallToolResult, OKOut, error) {
	c := tmuxenv.CaptureEnv()
	// Minted once, up front: paneRegistration's row.Alias is always the
	// stored, SEEDED form (both mint sites below seed before writing), while
	// in.Alias is whatever bare or full string the model supplied. Comparing
	// row.Alias against bare in.Alias can never match for a row this handler
	// created, so the guard — and the refusal text it can produce — must both
	// work off the seeded form.
	seededAlias := device.SeedMinted(in.Alias)
	if row, ok := paneRegistration(c.SocketPath, c.SessionID, c.PaneID, c.SessionCreated); ok && row.Alias != seededAlias {
		if in.Become {
			to := seededAlias
			raw, err := callDaemon("become", map[string]any{"from": row.Alias, "to": to})
			if err != nil {
				return nil, OKOut{}, err
			}
			var trade struct {
				From   string `json:"from"`
				To     string `json:"to"`
				Unread int    `json:"unread"`
			}
			_ = json.Unmarshal(raw, &trade)
			detail := fmt.Sprintf("you are now '%s' (was '%s'); %d unread thread(s): call get_inbox with alias '%s'", to, trade.From, trade.Unread, to)
			return nil, OKOut{OK: true, Detail: detail}, nil
		}
		detail := fmt.Sprintf("already registered as '%s'", row.Alias)
		if row.Label != "" {
			detail = fmt.Sprintf("already registered as '%s' (label '%s')", row.Alias, row.Label)
		}
		detail += " — use that alias; not adding a second, or pass become:true to claim '" + seededAlias + "' as this session's name"
		return nil, OKOut{OK: true, Detail: detail}, nil
	}

	h := harnessenv.FromEnv()
	sessionName := in.SessionName
	if sessionName == "" {
		sessionName = c.SessionName
	}
	socketPath, paneID := c.SocketPath, c.PaneID
	sessionID, project := c.SessionID, c.Project
	if c.SocketPath == "" || c.PaneID == "" {
		// Paneless session (harness daemon-hosted — the MCP server inherits
		// the session's env, which has no tmux): register under the paneless
		// tuple ("", harness session UUID) so the SessionEnd hook can reap
		// this alias and gc knows not to judge it by tmux liveness. A
		// half-captured socket (run-shell contexts) is dropped too — a tuple
		// mixing a real socket with a non-tmux session ID would read as dead
		// to every liveness check.
		socketPath, paneID = "", ""
		sessionID, project = h.SessionID, h.Project()
	}
	alias := seededAlias
	raw, err := callDaemon("register_agent", map[string]any{
		"alias":              alias,
		"role":               in.Role,
		"model_type":         in.ModelType,
		"session_name":       sessionName,
		"session_id":         sessionID,
		"session_created":    c.SessionCreated,
		"socket_path":        socketPath,
		"pane_id":            paneID,
		"project":            project,
		"label":              c.Label,
		"label_manual":       c.LabelManual,
		"harness_session_id": h.SessionID,
	})
	if err != nil {
		return nil, OKOut{}, err
	}
	var ack struct {
		Outcome string `json:"outcome"`
		Alias   string `json:"alias"`
		Unread  int    `json:"unread"`
	}
	_ = json.Unmarshal(raw, &ack)
	if ack.Outcome == "adopted" {
		// The daemon recognized this caller as a conversation it already
		// knows (by transcript or tuple — spec 2026-08-21 §3.2, daemon commit
		// a0b31bd) and moved THAT row onto this pane instead of inserting a
		// sibling. ack.Alias is the row's real, EFFECTIVE alias — it can
		// differ from what the caller asked for (alias) — so the caller must
		// be told which name actually answers, or it will keep addressing
		// itself by a name no row owns.
		//
		// Finding 3: when in.Become was already true, register_agent's own
		// become branch above (paneRegistration ok && alias mismatch) is what
		// normally handles a reclaim — this "adopted" outcome instead means
		// the daemon resolved the row a DIFFERENT way (by transcript, not
		// pane), so there is no live become to retry: telling the caller to
		// "pass become:true" here would have it repeat exactly what it just
		// did, forever. Say what happened and what would actually rename it.
		var detail string
		if in.Become {
			detail = fmt.Sprintf("you are already '%s' on this pane — '%s' is now the adopted alias for this conversation; run 'muster label <name>' from this session to rename it", ack.Alias, ack.Alias)
		} else {
			detail = fmt.Sprintf("you are already '%s' on this pane — use that alias, or pass become:true to claim '%s' as this session's name", ack.Alias, alias)
		}
		return nil, OKOut{OK: true, Detail: detail}, nil
	}
	detail := "registered " + alias
	if ack.Outcome == "revived" {
		detail = fmt.Sprintf("reconnected as '%s' — revived a previous registration", alias)
	}
	if ack.Unread > 0 {
		detail += fmt.Sprintf("; %d unread thread(s): call get_inbox with alias '%s'", ack.Unread, alias)
	}
	return nil, OKOut{OK: true, Detail: detail}, nil
}

func listAgentsHandler(_ context.Context, _ *mcp.CallToolRequest, _ ListAgentsIn) (*mcp.CallToolResult, ListAgentsOut, error) {
	raw, err := callDaemon("list_agents", nil)
	if err != nil {
		return nil, ListAgentsOut{}, err
	}
	var agents []AgentView
	if err := json.Unmarshal(raw, &agents); err != nil {
		return nil, ListAgentsOut{}, err
	}
	return nil, ListAgentsOut{Agents: agents}, nil
}

func registerRegistryTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "register_agent",
		Description: "Claim an agent identity on the muster bus. NOTE: sessions inside tmux are auto-registered at session start under their tmux session name — you almost never need this tool; the Stop hook and your inbox already address you. Calling it from an already-registered pane returns your existing identity instead of adding a second alias.",
	}, registerAgentHandler)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_agents",
		Description: "List all agents currently registered on the muster bus, with the fields an address is built from. An alias is not the only way to reach someone: a target resolves as exact alias first, then 'project:label', then a bare label within your own project. So a session you know by a label (its label field, when label_manual is true) is reachable by that label even though the label is not an alias — do not report such a target as unreachable. Departed agents remain listed: their alias still accepts mail, their label no longer resolves.",
	}, listAgentsHandler)
}
