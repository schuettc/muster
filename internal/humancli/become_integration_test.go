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
		"@muster_inbox": "1", "#{session_id}": "$1", "#{session_created}": "100", "#{session_name}": "muster-2",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })
	seed := func(op string, args map[string]any) {
		t.Helper()
		if _, err := callData(op, args); err != nil {
			t.Fatal(err)
		}
	}
	seed("register_agent", map[string]any{"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$1", "session_created": 100, "pane_id": "%1"})
	seed("become", map[string]any{"from": "muster-2", "to": "alias-routing"})
	seed("register_agent", map[string]any{"alias": "peer", "socket_path": "/tmp/sock", "session_id": "$9", "session_created": 100})
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
	ag, ok, _ := hookGetAgent("alias-routing")
	if !ok || ag.Departed || ag.SessionID != "$NEW" {
		t.Fatalf("claimed row after resume = %+v (ok=%v)", ag, ok)
	}
	if seedRow, _, _ := hookGetAgent("muster-2"); !seedRow.Departed {
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
	ag, ok, _ := hookGetAgent("alias-routing")
	if !ok || ag.Departed || ag.SessionID != "$NEW" {
		t.Fatalf("claimed row after resume = %+v (ok=%v)", ag, ok)
	}
	if seedRow, _, _ := hookGetAgent("muster-2"); !seedRow.Departed || seedRow.SessionID == "$NEW" {
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
	final, ok, _ := hookGetAgent("final-name")
	if !ok || final.Departed || final.SessionID != "$NEW" {
		t.Fatalf("final row after resume = %+v (ok=%v)", final, ok)
	}
	if seedRow, _, _ := hookGetAgent("muster-2"); !seedRow.Departed || seedRow.SessionID == "$NEW" {
		t.Fatalf("seed must stay retired: %+v", seedRow)
	}
	if midRow, _, _ := hookGetAgent("alias-routing"); !midRow.Departed || midRow.SessionID == "$NEW" {
		t.Fatalf("middle link must stay retired: %+v", midRow)
	}
}

// TestResumeSummaryReportsSessionUnreadTruth is finding F2, live rig: mail
// addressed to the retired seed alias pends BEFORE resume. Before the fix,
// the printed resume summary used the per-alias register ack (UnreadCount of
// the CLAIMED alias only), which never sees mail addressed to the SEED's
// name and printed "0 unread thread(s)" while a real thread pended — the
// injected context lied to the agent about its own backlog. This asserts
// the exact count, not just non-zero: session_unread's lineage-aware total.
func TestResumeSummaryReportsSessionUnreadTruth(t *testing.T) {
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
	seed("register_agent", map[string]any{"alias": "peer", "socket_path": "/tmp/sock2", "session_id": "$peer", "session_created": 100})
	// A straggler addressed to the RETIRED seed name, sent BEFORE resume —
	// the exact live-rig topology: the mail's target is a departed alias
	// whose superseded_by carries the identity forward.
	seed("send_message", map[string]any{"from": "peer", "to_kind": "agent", "to_target": "muster-2", "subject": "s", "body": "for the old name"})

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(`{"source":"resume","session_id":"uuid-42"}`), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "reconnected as 'alias-routing' (refreshed) — 1 unread thread(s)") {
		t.Fatalf("resume summary must report the true (lineage) pending count, got:\n%s", out)
	}
}

// TestStopHookDrainsSeedStragglerAfterResume: extends
// TestStopHookDrainsSeedStragglerAfterBecome across resume. A straggler sent
// to the RETIRED name AFTER the conversation resumed onto a brand-new tmux
// tuple must still light the Stop-hook drain on the NEW tuple — the seed's
// own row never moves off the dead old tuple, so this only works if the
// drain path (session_aliases + session_unread) follows supersession
// lineage rather than a flat tuple match (finding F1).
func TestStopHookDrainsSeedStragglerAfterResume(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("MUSTER_ALIAS", "")
	prevRun := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$NEW", "#{session_name}": "muster-9", "#{session_created}": "222",
	})
	t.Cleanup(func() { tmuxenv.Run = prevRun })
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

	var resumeBuf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(`{"source":"resume","session_id":"uuid-42"}`), &resumeBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resumeBuf.String(), "reconnected as 'alias-routing'") {
		t.Fatalf("setup: resume did not reclaim as expected:\n%s", resumeBuf.String())
	}

	// A straggler addressed to the RETIRED seed name, sent AFTER resume: the
	// seed's own row is still departed on the OLD (now-dead) tuple.
	seed("register_agent", map[string]any{"alias": "peer", "socket_path": "/tmp/sock2", "session_id": "$peer", "session_created": 100})
	seed("send_message", map[string]any{"from": "peer", "to_kind": "agent", "to_target": "muster-2", "subject": "s", "body": "still for the old name"})

	// Stop, on the NEW tuple, driven by tmux env matching the resumed session.
	tmuxenv.Run = hookRun(map[string]string{
		"@muster_inbox": "1", "#{session_id}": "$NEW", "#{session_created}": "222", "#{session_name}": "muster-9",
	})
	var stopBuf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{}`), &stopBuf); err != nil {
		t.Fatal(err)
	}
	out := stopBuf.String()
	if !strings.Contains(out, "alias-routing") {
		t.Fatalf("Stop-hook drain on the new tuple must surface the unread mail (either by naming the alias or the count), got:\n%s", out)
	}
	if !strings.Contains(out, "1 unread") && !strings.Contains(out, `"reason"`) {
		t.Fatalf("Stop-hook drain must surface the straggler as pending mail:\n%s", out)
	}
}
