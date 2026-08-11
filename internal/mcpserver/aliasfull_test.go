package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestModelSurfacesKeepTheFullAlias is the counterweight to every stripping
// test in this change. Human surfaces (CLI tables, the station TUI, the tmux
// badge) render this machine's aliases short via device.Strip; MCP surfaces —
// what a model reads — must not.
//
// The reason is an asymmetry in failure modes. A human who types a short name
// that does not resolve gets an error and retries. A model writes aliases
// into message bodies and task descriptions that are read on the OTHER
// machine, where a bare name re-resolves against THAT device and lands on a
// different, real agent — a silent misroute nothing reports.
//
// This exercises the real list_agents handler (not a hand-built AgentView
// fixture) so it also proves nothing between the daemon's JSON and the
// tool's response type quietly strips the prefix along the way.
//
// If you are here because this test failed while you were making alias
// rendering consistent: that inconsistency is the feature.
func TestModelSurfacesKeepTheFullAlias(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")

	prevDaemon := callDaemon
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		if op != "list_agents" {
			t.Fatalf("unexpected op %s", op)
		}
		return json.RawMessage(`[{"alias":"personal-dotfiles/main","model_type":"claude","role":"producer"}]`), nil
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, out, err := listAgentsHandler(context.Background(), nil, ListAgentsIn{})
	if err != nil {
		t.Fatalf("listAgentsHandler: %v", err)
	}
	if len(out.Agents) != 1 {
		t.Fatalf("agents = %+v, want exactly one", out.Agents)
	}
	got := out.Agents[0].Alias
	if got != "personal-dotfiles/main" {
		t.Fatalf("AgentView.Alias = %q, want the full stored alias", got)
	}
	if strings.HasPrefix(got, "dotfiles/") {
		t.Fatal("AgentView.Alias was device-stripped; model surfaces must carry the full alias")
	}
}
