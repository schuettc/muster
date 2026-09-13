package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/schuettc/muster/internal/harnessenv"
	"github.com/schuettc/muster/internal/tmuxenv"
)

func stubCallerCaptures(t *testing.T, c tmuxenv.Capture, h harnessenv.Capture) {
	t.Helper()
	prevTmux, prevHarness := captureCallerTmux, captureCallerHarness
	t.Cleanup(func() { captureCallerTmux, captureCallerHarness = prevTmux, prevHarness })
	captureCallerTmux = func() tmuxenv.Capture { return c }
	captureCallerHarness = func() harnessenv.Capture { return h }
}

func TestResolveCallerIdentityFromTmuxSession(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	stubCallerCaptures(t,
		tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", PaneID: "%2", SessionCreated: 100},
		harnessenv.Capture{})
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "session_aliases":
			return json.RawMessage(`{"aliases":["device-api"]}`), nil
		case "list_agents":
			return json.RawMessage(`[{"alias":"device-api","role":"worker","model_type":"claude","socket_path":"/s","pane_id":"%2","session_id":"$1","session_created":100,"project":"muster","label":"api","label_manual":true,"registered_at":10,"last_seen":20}]`), nil
		default:
			t.Fatalf("unexpected op %q", op)
			return nil, nil
		}
	}

	got, err := resolveCallerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Registered || got.Agent.Alias != "device-api" {
		t.Fatalf("identity = %+v", got)
	}
	if len(got.LiveAliases) != 1 || got.LiveAliases[0] != "device-api" {
		t.Fatalf("live aliases = %v", got.LiveAliases)
	}
}

func TestResolveCallerIdentityFromPanelessSession(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	stubCallerCaptures(t, tmuxenv.Capture{}, harnessenv.Capture{SessionID: "hs-1"})
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		switch op {
		case "session_aliases":
			if args["socket_path"] != "" || args["session_id"] != "hs-1" {
				t.Fatalf("session proof = %+v", args)
			}
			return json.RawMessage(`{"aliases":["device-work"]}`), nil
		case "list_agents":
			return json.RawMessage(`[{"alias":"device-work","session_id":"hs-1","harness_session_id":"hs-1"}]`), nil
		default:
			t.Fatalf("unexpected op %q", op)
			return nil, nil
		}
	}

	got, err := resolveCallerIdentity()
	if err != nil || !got.Registered || got.Agent.Alias != "device-work" {
		t.Fatalf("identity = %+v, err = %v", got, err)
	}
}

func TestResolveCallerIdentityReturnsAllLiveLineageAliases(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	stubCallerCaptures(t, tmuxenv.Capture{}, harnessenv.Capture{SessionID: "hs-1"})
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "session_aliases" {
			return json.RawMessage(`{"aliases":["seed","chosen","sibling"]}`), nil
		}
		return json.RawMessage(`[
			{"alias":"seed","departed":true,"superseded_by":"chosen"},
			{"alias":"sibling"},
			{"alias":"chosen"},
			{"alias":"foreign"}
		]`), nil
	}

	got, err := resolveCallerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"chosen", "sibling"}
	if len(got.LiveAliases) != len(want) || got.LiveAliases[0] != want[0] || got.LiveAliases[1] != want[1] {
		t.Fatalf("live aliases = %v, want %v", got.LiveAliases, want)
	}
}

func TestResolveCallerIdentityExcludesForeignDeviceTupleCollision(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	stubCallerCaptures(t,
		tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", PaneID: "%2", SessionCreated: 100},
		harnessenv.Capture{})
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "session_aliases" {
			// The daemon's device-scoped proof returns only the local lineage.
			return json.RawMessage(`{"aliases":["local"]}`), nil
		}
		return json.RawMessage(`[
			{"alias":"local","device_id":"local-device","socket_path":"/s","session_id":"$1","pane_id":"%2","session_created":100},
			{"alias":"foreign","device_id":"foreign-device","socket_path":"/s","session_id":"$1","pane_id":"%2","session_created":100}
		]`), nil
	}

	got, err := resolveCallerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Registered || got.Agent.Alias != "local" || len(got.LiveAliases) != 1 || got.LiveAliases[0] != "local" {
		t.Fatalf("identity = %+v", got)
	}
}

func TestResolveCallerIdentityWithoutProofIsUnregistered(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	stubCallerCaptures(t, tmuxenv.Capture{}, harnessenv.Capture{})
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		t.Fatalf("unexpected daemon call %q", op)
		return nil, nil
	}

	got, err := resolveCallerIdentity()
	if err != nil || got.Registered {
		t.Fatalf("identity = %+v, err = %v", got, err)
	}
}
