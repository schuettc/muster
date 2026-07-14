package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

func TestRegisterAgentCapturesTmuxEnv(t *testing.T) {
	t.Setenv("TMUX", "/private/tmp/tmux-501/proj-muster,123,0")
	t.Setenv("TMUX_PANE", "%6")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		switch args[len(args)-1] {
		case "#{session_id}":
			return "$5", nil
		case "#{session_name}":
			return "muster-2", nil
		default:
			return "backend\x1f1", nil
		}
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var got map[string]any
	prevDaemon := callDaemon
	callDaemon = func(_ string, args map[string]any) (json.RawMessage, error) {
		got = args
		return []byte(`{}`), nil
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, _, err := registerAgentHandler(context.TODO(), nil, RegisterAgentIn{Alias: "backend", Role: "producer", ModelType: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if got["socket_path"] != "/private/tmp/tmux-501/proj-muster" || got["session_id"] != "$5" ||
		got["project"] != "muster" || got["label"] != "backend" || got["label_manual"] != true {
		t.Fatalf("captured args = %+v", got)
	}
}
