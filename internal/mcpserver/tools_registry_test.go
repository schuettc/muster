package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
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

func TestRegisterAgentIdempotentForRegisteredPane(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if args[len(args)-1] == "#{session_created}" {
			return "500", nil // same incarnation as the roster row below
		}
		return "$1", nil // session-id probe (and other queries)
	}
	t.Cleanup(func() { tmuxenv.Run = prevRun })

	var registered bool
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "list_agents":
			return json.RawMessage(`[{"alias":"timewalk-2","model_type":"claude","socket_path":"/tmp/sock","pane_id":"%14","session_id":"$1","session_created":500,"label":"standard 2000","departed":false}]`), nil
		case "register_agent":
			registered = true
			return json.RawMessage(`null`), nil
		}
		t.Fatalf("unexpected op %s", op)
		return nil, nil
	}

	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "timewalk-2002", ModelType: "claude"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if registered {
		t.Fatal("must NOT mint a second alias for an already-registered pane")
	}
	if !out.OK || !strings.Contains(out.Detail, "already registered as 'timewalk-2'") || !strings.Contains(out.Detail, "standard 2000") {
		t.Fatalf("expected identity-bearing detail, got %+v", out)
	}
}

// TestRegisterAgentGhostSessionCreatedRegisters covers the ghost-guard: a
// roster row can match the calling pane's (socket_path, session_id, pane_id)
// tuple yet be a leftover from a DEAD server incarnation — tmux recycles
// session IDs from $0 after a server restart. When the row's recorded
// session_created disagrees with the caller's live one, paneRegistration must
// NOT treat it as this pane's own registration: the handler must register
// fresh, not short-circuit as "already registered" under the ghost's alias.
func TestRegisterAgentGhostSessionCreatedRegisters(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if args[len(args)-1] == "#{session_created}" {
			return "222", nil // caller's LIVE session incarnation
		}
		return "$1", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prevRun })

	var registered bool
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "list_agents":
			// same tuple as the caller, but a STALE incarnation (111 != 222):
			// a ghost left by a session ID tmux recycled after a restart.
			return json.RawMessage(`[{"alias":"ghost-agent","model_type":"claude","socket_path":"/tmp/sock","pane_id":"%14","session_id":"$1","session_created":111,"label":"old label","departed":false}]`), nil
		case "register_agent":
			registered = true
			return json.RawMessage(`null`), nil
		}
		t.Fatalf("unexpected op %s", op)
		return nil, nil
	}

	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "fresh-agent", ModelType: "claude"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !registered {
		t.Fatal("a tuple match with a mismatched session_created is a ghost — must register fresh, not short-circuit")
	}
	if strings.Contains(out.Detail, "already registered") {
		t.Fatalf("must not report the ghost row's alias as this session's identity, got %+v", out)
	}
}

func TestRegisterAgentSameAliasStillUpserts(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(_ ...string) (string, error) { return "$1", nil }
	t.Cleanup(func() { tmuxenv.Run = prevRun })

	var registered bool
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "list_agents":
			return json.RawMessage(`[{"alias":"timewalk-2","model_type":"claude","socket_path":"/tmp/sock","pane_id":"%14","session_id":"$1","departed":false}]`), nil
		case "register_agent":
			registered = true
			return json.RawMessage(`null`), nil
		}
		return nil, nil
	}
	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "timewalk-2", ModelType: "claude"})
	if err != nil || !out.OK {
		t.Fatalf("same-alias re-register must succeed: %+v %v", out, err)
	}
	if !registered {
		t.Fatal("same-alias call must still upsert (refresh)")
	}
}

func TestRegisterAgentFreshPaneRegisters(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(_ ...string) (string, error) { return "$1", nil }
	t.Cleanup(func() { tmuxenv.Run = prevRun })

	var registered bool
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "list_agents":
			return json.RawMessage(`[]`), nil
		case "register_agent":
			registered = true
			return json.RawMessage(`null`), nil
		}
		return nil, nil
	}
	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "fresh", ModelType: "claude"})
	if err != nil || !out.OK || !registered {
		t.Fatalf("fresh pane must register: registered=%v out=%+v err=%v", registered, out, err)
	}
}
