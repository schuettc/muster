package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
	"github.com/schuettc/tools-common/harness"
)

func stubCallerCaptures(t *testing.T, c tmuxenv.Capture, h harness.Capture) {
	t.Helper()
	prevTmux, prevHarness := captureCallerTmux, captureCallerHarness
	t.Cleanup(func() { captureCallerTmux, captureCallerHarness = prevTmux, prevHarness })
	captureCallerTmux = func() tmuxenv.Capture { return c }
	captureCallerHarness = func() harness.Capture { return h }
}

func TestResolveCallerIdentityFromTmuxSession(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	stubCallerCaptures(t,
		tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", PaneID: "%2", SessionCreated: 100},
		harness.Capture{})
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
	stubCallerCaptures(t, tmuxenv.Capture{}, harness.Capture{SessionID: "hs-1"})
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
	stubCallerCaptures(t, tmuxenv.Capture{}, harness.Capture{SessionID: "hs-1"})
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
		harness.Capture{})
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
	stubCallerCaptures(t, tmuxenv.Capture{}, harness.Capture{})
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		t.Fatalf("unexpected daemon call %q", op)
		return nil, nil
	}

	got, err := resolveCallerIdentity()
	if err != nil || got.Registered {
		t.Fatalf("identity = %+v, err = %v", got, err)
	}
}

// A paneless caller proves the session tools-common/harness resolves from the
// real environment (captureCallerHarness is left unstubbed here). The two
// cases with both ids are the ones that matter for roster ownership: a
// pi-claude-bridge child (marker set) acts for the pi session that ran it; a
// Claude session started from inside pi (no marker) is its own session.
func TestResolveCallerIdentityPanelessSessionRule(t *testing.T) {
	for _, tc := range []struct {
		name, claude, agent, child, want string
	}{
		{"bridge child: both ids and the marker", "claude-child", "pi-parent", "1", "pi-parent"},
		{"claude launched from pi: both ids, no marker", "claude-own", "pi-parent", "", "claude-own"},
		{"claude only", "claude-only", "", "", "claude-only"},
		{"agent only", "", "pi-only", "", "pi-only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_SESSION_ID", tc.claude)
			t.Setenv("AGENT_SESSION_ID", tc.agent)
			t.Setenv("AGENT_SESSION_CHILD", tc.child)
			prevTmux, prevCall := captureCallerTmux, callDaemon
			t.Cleanup(func() { captureCallerTmux, callDaemon = prevTmux, prevCall })
			captureCallerTmux = func() tmuxenv.Capture { return tmuxenv.Capture{} }
			var proved any
			callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
				switch op {
				case "session_aliases":
					proved = args["session_id"]
					return json.RawMessage(`{"aliases":[]}`), nil
				case "list_agents":
					return json.RawMessage(`[]`), nil
				default:
					t.Fatalf("unexpected op %q", op)
					return nil, nil
				}
			}
			if _, err := resolveCallerIdentity(); err != nil {
				t.Fatal(err)
			}
			if proved != tc.want {
				t.Fatalf("paneless caller proved session %v, want %q", proved, tc.want)
			}
		})
	}
}
