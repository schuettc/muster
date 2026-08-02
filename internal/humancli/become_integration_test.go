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
