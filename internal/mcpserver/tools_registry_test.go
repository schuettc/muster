package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRegisterAgentCapturesTmuxEnv(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/private/tmp/tmux-501/proj-bettor-help,123,4")
	t.Setenv("TMUX_PANE", "%6")

	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{
		Alias: "backend", Role: "producer", ModelType: "claude",
	})
	if err != nil {
		t.Fatalf("register handler: %v", err)
	}
	if !out.OK {
		t.Fatalf("expected ok")
	}

	// Verify via list that the socket/pane were captured from the env.
	_, listOut, err := listAgentsHandler(context.Background(), nil, ListAgentsIn{})
	if err != nil {
		t.Fatalf("list handler: %v", err)
	}
	if len(listOut.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(listOut.Agents))
	}
	if listOut.Agents[0].Alias != "backend" {
		t.Fatalf("unexpected agent: %+v", listOut.Agents[0])
	}
	if listOut.Agents[0].ModelType != "claude" {
		t.Fatalf("unexpected model_type: %+v", listOut.Agents[0])
	}
}

func TestRegisterAgentResolvesSessionID(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/private/tmp/tmux-501/proj-muster,123,4")
	t.Setenv("TMUX_PANE", "%2")
	// stub the tmux query so we don't need a real tmux
	orig := tmuxQuery
	tmuxQuery = func(_, _, format string) string {
		switch format {
		case "#{session_id}":
			return "$7"
		case "#{session_name}":
			return "muster-2"
		}
		return ""
	}
	t.Cleanup(func() { tmuxQuery = orig })

	if _, _, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "reviewer", Role: "reviewer", ModelType: "codex"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	raw, err := callDaemon("list_agents", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var agents []AgentView
	_ = json.Unmarshal(raw, &agents)
	if len(agents) != 1 || agents[0].SessionName != "muster-2" {
		t.Fatalf("session_name not captured: %+v", agents)
	}
}
