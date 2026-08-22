package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/device"
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

// TestGetInboxKeepsTheFullFromAgent covers ThreadView.FromAgent, the second
// model-facing shape populated from the same unstripped stored alias
// (internal/daemon/daemon.go's CreateThread call sites set FromAgent: from
// directly off the sender's alias — no device.Strip in the chain). get_inbox
// hands this straight into model context, same as list_agents, so the same
// asymmetry applies: a model that read a stripped from_agent here could echo
// it into a reply, and on another machine that bare name re-resolves against
// THAT device's own prefix and reaches a different, real agent.
//
// This drives the real getInboxHandler with a stubbed callDaemon (the daemon
// JSON get_inbox actually returns), not a hand-built ThreadView, so it also
// proves nothing strips the prefix between the daemon and the tool response.
//
// If you are here because this test failed while making alias rendering
// consistent: that inconsistency is the feature — see
// TestModelSurfacesKeepTheFullAlias for the full rationale.
func TestGetInboxKeepsTheFullFromAgent(t *testing.T) {
	t.Setenv("MUSTER_DEVICE_NAME", "personal")

	const fullAlias = "personal-dotfiles/main"
	prevDaemon := callDaemon
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		if op != "get_inbox" {
			t.Fatalf("unexpected op %s", op)
		}
		return json.RawMessage(`{"threads":[{"id":1,"kind":"message","from_agent":"` + fullAlias + `","to_kind":"agent","to_target":"peer","subject":"hi","last_from":"` + fullAlias + `"}],"marked_read":false}`), nil
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, out, err := getInboxHandler(context.Background(), nil, GetInboxIn{Alias: "peer"})
	if err != nil {
		t.Fatalf("getInboxHandler: %v", err)
	}
	if len(out.Threads) != 1 {
		t.Fatalf("threads = %+v, want exactly one", out.Threads)
	}
	got := out.Threads[0].FromAgent
	if got != fullAlias {
		t.Fatalf("ThreadView.FromAgent = %q, want the full stored alias %q", got, fullAlias)
	}
	if short := device.Strip("personal", fullAlias); got == short {
		t.Fatalf("ThreadView.FromAgent = %q, was device-stripped to %q; model surfaces must carry the full alias", got, short)
	}
}

// TestGetThreadKeepsTheFullFromAgent covers both ThreadView.FromAgent and
// EntryView.FromAgent as returned by get_thread — the same unstripped stored
// alias, populated by a different daemon op and MCP handler than
// TestGetInboxKeepsTheFullFromAgent, so the two guards do not share a single
// point of failure. See TestModelSurfacesKeepTheFullAlias for why the
// human/model asymmetry is deliberate rather than an oversight to "fix".
func TestGetThreadKeepsTheFullFromAgent(t *testing.T) {
	t.Setenv("MUSTER_DEVICE_NAME", "personal")

	const fullAlias = "personal-dotfiles/main"
	prevDaemon := callDaemon
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		if op != "get_thread" {
			t.Fatalf("unexpected op %s", op)
		}
		return json.RawMessage(`{"thread":{"id":1,"kind":"message","from_agent":"` + fullAlias + `","to_kind":"agent","to_target":"peer"},"entries":[{"id":1,"thread_id":1,"from_agent":"` + fullAlias + `","body":"hi"}]}`), nil
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, out, err := getThreadHandler(context.Background(), nil, GetThreadIn{ThreadID: 1})
	if err != nil {
		t.Fatalf("getThreadHandler: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("entries = %+v, want exactly one", out.Entries)
	}
	gotThread, gotEntry := out.Thread.FromAgent, out.Entries[0].FromAgent
	if gotThread != fullAlias {
		t.Fatalf("ThreadView.FromAgent = %q, want the full stored alias %q", gotThread, fullAlias)
	}
	if gotEntry != fullAlias {
		t.Fatalf("EntryView.FromAgent = %q, want the full stored alias %q", gotEntry, fullAlias)
	}
	short := device.Strip("personal", fullAlias)
	if gotThread == short || gotEntry == short {
		t.Fatalf("FromAgent was device-stripped to %q; model surfaces must carry the full alias (thread=%q entry=%q)", short, gotThread, gotEntry)
	}
}
