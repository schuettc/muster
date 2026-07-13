package mcpserver

import (
	"context"
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
