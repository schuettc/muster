package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	if row, ok := paneRegistration(c.SocketPath, c.SessionID, c.PaneID, c.SessionCreated); ok && row.Alias != in.Alias {
		if in.Become {
			raw, err := callDaemon("become", map[string]any{"from": row.Alias, "to": in.Alias})
			if err != nil {
				return nil, OKOut{}, err
			}
			var trade struct {
				From   string `json:"from"`
				To     string `json:"to"`
				Unread int    `json:"unread"`
			}
			_ = json.Unmarshal(raw, &trade)
			detail := fmt.Sprintf("you are now '%s' (was '%s'); %d unread thread(s): call get_inbox with alias '%s'", trade.To, trade.From, trade.Unread, trade.To)
			return nil, OKOut{OK: true, Detail: detail}, nil
		}
		detail := fmt.Sprintf("already registered as '%s'", row.Alias)
		if row.Label != "" {
			detail = fmt.Sprintf("already registered as '%s' (label '%s')", row.Alias, row.Label)
		}
		detail += " — use that alias; not adding a second, or pass become:true to claim '" + in.Alias + "' as this session's name"
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
	raw, err := callDaemon("register_agent", map[string]any{
		"alias":              in.Alias,
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
		Unread  int    `json:"unread"`
	}
	_ = json.Unmarshal(raw, &ack)
	detail := "registered " + in.Alias
	if ack.Outcome == "revived" {
		detail = fmt.Sprintf("reconnected as '%s' — revived a previous registration", in.Alias)
	}
	if ack.Unread > 0 {
		detail += fmt.Sprintf("; %d unread thread(s): call get_inbox with alias '%s'", ack.Unread, in.Alias)
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
