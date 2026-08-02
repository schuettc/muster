package humancli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// TestStopHookDrainsSeedStragglerAfterBecome: mail sent to the retired seed
// AFTER the claim still reaches the session's drain — session_aliases
// includes departed aliases on purpose, and become must not break that.
func TestStopHookDrainsSeedStragglerAfterBecome(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"@muster_inbox": "1", "#{session_id}": "$1", "#{session_name}": "muster-2",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })
	seed := func(op string, args map[string]any) {
		t.Helper()
		if _, err := callData(op, args); err != nil {
			t.Fatal(err)
		}
	}
	seed("register_agent", map[string]any{"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$1", "pane_id": "%1"})
	seed("become", map[string]any{"from": "muster-2", "to": "alias-routing"})
	seed("register_agent", map[string]any{"alias": "peer", "socket_path": "/tmp/sock", "session_id": "$9"})
	seed("send_message", map[string]any{"from": "peer", "to_kind": "agent", "to_target": "alias-routing", "subject": "s", "body": "b"})

	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{}`), &buf); err != nil {
		t.Fatal(err)
	}
	outStr := buf.String()
	if !strings.Contains(outStr, "alias-routing") {
		t.Fatalf("drain reason must name the claimed alias:\n%s", outStr)
	}
}

// TestResumeReclaimsClaimedName: the v0.8.0 resume chain composed with
// become — kill the tmux session, resume env-stripped in a new one, and the
// CLAIMED alias (not the seed) reconnects onto the new tuple.
func TestResumeReclaimsClaimedName(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$NEW", "#{session_name}": "muster-9", "#{session_created}": "222",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })
	seed := func(op string, args map[string]any) {
		t.Helper()
		if _, err := callData(op, args); err != nil {
			t.Fatal(err)
		}
	}
	seed("register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$OLD",
		"session_created": 111, "harness_session_id": "uuid-42",
	})
	seed("become", map[string]any{"from": "muster-2", "to": "alias-routing"})

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(`{"source":"resume","session_id":"uuid-42"}`), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "reconnected as 'alias-routing'") {
		t.Fatalf("resume summary:\n%s", buf.String())
	}
	ag, ok := hookGetAgent("alias-routing")
	if !ok || ag.Departed || ag.SessionID != "$NEW" {
		t.Fatalf("claimed row after resume = %+v (ok=%v)", ag, ok)
	}
	if seedRow, _ := hookGetAgent("muster-2"); !seedRow.Departed {
		t.Fatalf("seed must stay retired after resume, got %+v", seedRow)
	}
}

// TestResumeAfterGracefulExitReclaimsOnlyClaimedName is fix round 1's main
// regression: the COMMON flow, not the killed-without-SessionEnd variant
// TestResumeReclaimsClaimedName models. become claims the name, then the
// session exits gracefully — SessionEnd tombstones BOTH rows sharing the
// harness UUID (the claimed alias AND the already-retired seed) — so on
// resume there is no live sibling left for a tuple-sharing heuristic to key
// on. Only store.Become's persisted superseded_by tells resume the seed must
// never come back; a departed row's tuple/liveness alone cannot.
func TestResumeAfterGracefulExitReclaimsOnlyClaimedName(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$NEW", "#{session_name}": "muster-9", "#{session_created}": "222",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })
	seed := func(op string, args map[string]any) {
		t.Helper()
		if _, err := callData(op, args); err != nil {
			t.Fatal(err)
		}
	}
	seed("register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$OLD",
		"session_created": 111, "harness_session_id": "uuid-42",
	})
	seed("become", map[string]any{"from": "muster-2", "to": "alias-routing"})
	// The graceful-exit half: SessionEnd tombstones every alias the dying
	// session owns, including the claimed one — unlike the killed-session
	// variant, alias-routing is departed too by the time resume runs.
	seed("deregister_agent", map[string]any{"alias": "alias-routing"})

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(`{"source":"resume","session_id":"uuid-42"}`), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "reconnected as 'alias-routing'") {
		t.Fatalf("resume summary must reclaim the claimed alias:\n%s", out)
	}
	if strings.Contains(out, "'muster-2'") {
		t.Fatalf("resume summary must not resurrect the retired seed:\n%s", out)
	}
	ag, ok := hookGetAgent("alias-routing")
	if !ok || ag.Departed || ag.SessionID != "$NEW" {
		t.Fatalf("claimed row after resume = %+v (ok=%v)", ag, ok)
	}
	if seedRow, _ := hookGetAgent("muster-2"); !seedRow.Departed || seedRow.SessionID == "$NEW" {
		t.Fatalf("seed must stay retired, never touching the new tuple: %+v", seedRow)
	}
}

// TestResumeChainedBecomeReclaimsOnlyFinalName: two claims in the same
// session's lifetime (A becomes B, B becomes C), all three rows now sharing
// one harness UUID with A and B departed. Resume must reclaim only C — the
// end of the chain — never A or B, proving superseded_by (not a
// single-hop heuristic) is what resume actually trusts.
func TestResumeChainedBecomeReclaimsOnlyFinalName(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$NEW", "#{session_name}": "muster-9", "#{session_created}": "222",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })
	seed := func(op string, args map[string]any) {
		t.Helper()
		if _, err := callData(op, args); err != nil {
			t.Fatal(err)
		}
	}
	seed("register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$OLD",
		"session_created": 111, "harness_session_id": "uuid-42",
	})
	seed("become", map[string]any{"from": "muster-2", "to": "alias-routing"})
	seed("become", map[string]any{"from": "alias-routing", "to": "final-name"})
	seed("deregister_agent", map[string]any{"alias": "final-name"})

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(`{"source":"resume","session_id":"uuid-42"}`), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "reconnected as 'final-name'") {
		t.Fatalf("resume summary must reclaim only the chain's end:\n%s", out)
	}
	if strings.Contains(out, "'muster-2'") || strings.Contains(out, "'alias-routing'") {
		t.Fatalf("resume summary must not resurrect either superseded link:\n%s", out)
	}
	final, ok := hookGetAgent("final-name")
	if !ok || final.Departed || final.SessionID != "$NEW" {
		t.Fatalf("final row after resume = %+v (ok=%v)", final, ok)
	}
	if seedRow, _ := hookGetAgent("muster-2"); !seedRow.Departed || seedRow.SessionID == "$NEW" {
		t.Fatalf("seed must stay retired: %+v", seedRow)
	}
	if midRow, _ := hookGetAgent("alias-routing"); !midRow.Departed || midRow.SessionID == "$NEW" {
		t.Fatalf("middle link must stay retired: %+v", midRow)
	}
}
