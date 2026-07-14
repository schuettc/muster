package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// RegisterAgentIn is the input to register_agent. socket_path/pane_id are NOT
// input — they are captured from the process environment ($TMUX, $TMUX_PANE),
// which the agent's MCP server inherits from its tmux pane.
type RegisterAgentIn struct {
	Alias       string `json:"alias" jsonschema:"a short addressable name for this agent, e.g. backend"`
	Role        string `json:"role" jsonschema:"this agent's role: producer, consumer, reviewer, ..."`
	ModelType   string `json:"model_type" jsonschema:"the model backing this agent: claude or codex"`
	SessionName string `json:"session_name,omitempty" jsonschema:"optional tmux session name for display"`
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
	sessionName := in.SessionName
	if sessionName == "" {
		sessionName = c.SessionName
	}
	_, err := callDaemon("register_agent", map[string]any{
		"alias":        in.Alias,
		"role":         in.Role,
		"model_type":   in.ModelType,
		"session_name": sessionName,
		"session_id":   c.SessionID,
		"socket_path":  c.SocketPath,
		"pane_id":      c.PaneID,
		"project":      c.Project,
		"label":        c.Label,
		"label_manual": c.LabelManual,
	})
	if err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true, Detail: "registered " + in.Alias}, nil
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
		Description: "Register this agent on the muster bus so others can address it. Captures the agent's tmux pane automatically. Call once at the start of a session.",
	}, registerAgentHandler)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_agents",
		Description: "List all agents currently registered on the muster bus.",
	}, listAgentsHandler)
}
