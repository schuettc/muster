package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
	"github.com/schuettc/tools-common/harness"
)

func TestCurrentAgentReturnsCallerAndOwnedAliases(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	stubCallerCaptures(t, tmuxenv.Capture{}, harness.Capture{SessionID: "hs-1"})
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "session_aliases" {
			return json.RawMessage(`{"aliases":["work","work-old"]}`), nil
		}
		return json.RawMessage(`[
			{"alias":"work","role":"worker","project":"muster"},
			{"alias":"work-old","departed":true}
		]`), nil
	}

	_, got, err := currentAgentHandler(context.Background(), nil, CurrentAgentIn{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Registered || got.Agent == nil || got.Agent.Alias != "work" {
		t.Fatalf("current agent = %+v", got)
	}
	if len(got.OwnedAliases) != 1 || got.OwnedAliases[0] != "work" {
		t.Fatalf("owned aliases = %v", got.OwnedAliases)
	}
}

func TestCurrentAgentUnregisteredReturnsEmptyAliasList(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	stubCallerCaptures(t, tmuxenv.Capture{}, harness.Capture{SessionID: "hs-1"})
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		if op != "session_aliases" {
			t.Fatalf("unexpected op %q", op)
		}
		return json.RawMessage(`{"aliases":[]}`), nil
	}

	_, got, err := currentAgentHandler(context.Background(), nil, CurrentAgentIn{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Registered || got.Agent != nil || got.OwnedAliases == nil || len(got.OwnedAliases) != 0 {
		t.Fatalf("current agent = %+v, want unregistered with non-nil empty aliases", got)
	}
}

func TestDeregisterAgentWithoutSessionProofFails(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	stubCallerCaptures(t, tmuxenv.Capture{}, harness.Capture{})

	_, _, err := deregisterAgentHandler(context.Background(), nil, DeregisterAgentIn{})
	if err == nil {
		t.Fatal("deregister without session proof must fail")
	}
}

func TestDeregisterAgentTombstonesEveryOwnedLiveAlias(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	stubCallerCaptures(t, tmuxenv.Capture{}, harness.Capture{SessionID: "hs-1"})
	var departed []string
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		switch op {
		case "session_aliases":
			return json.RawMessage(`{"aliases":["sibling","primary"]}`), nil
		case "list_agents":
			return json.RawMessage(`[{"alias":"sibling"},{"alias":"primary"}]`), nil
		case "deregister_agent":
			departed = append(departed, args["alias"].(string))
			return json.RawMessage(`null`), nil
		default:
			t.Fatalf("unexpected op %q", op)
			return nil, nil
		}
	}

	_, got, err := deregisterAgentHandler(context.Background(), nil, DeregisterAgentIn{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Changed != 2 || len(departed) != 2 || departed[0] != "primary" || departed[1] != "sibling" {
		t.Fatalf("result = %+v, departed = %v", got, departed)
	}
}
