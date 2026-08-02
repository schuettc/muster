package humancli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// TestBecomeClaimsSingleAliasSession: the happy path — one live alias on
// this session; become claims the new name and reports the trade.
func TestBecomeClaimsSingleAliasSession(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$1", "#{session_name}": "muster-2", "#{session_created}": "111",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })
	if _, err := callData("register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$1", "session_created": 111,
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Dispatch([]string{"become", "alias-routing"}, &buf); err != nil {
		t.Fatalf("become: %v", err)
	}
	if !strings.Contains(buf.String(), "you are now 'alias-routing' (was 'muster-2')") {
		t.Fatalf("output = %q", buf.String())
	}
	ag, ok := hookGetAgent("alias-routing")
	if !ok || ag.Departed || ag.SessionID != "$1" {
		t.Fatalf("claimed row = %+v (ok=%v)", ag, ok)
	}
}

// TestBecomeRequiresFromWhenSplit: two live aliases on one session — never
// guess which identity is being claimed over.
func TestBecomeRequiresFromWhenSplit(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1"})
	t.Cleanup(func() { tmuxenv.Run = prev })
	for _, a := range []string{"muster-2", "cost-audit"} {
		if _, err := callData("register_agent", map[string]any{
			"alias": a, "socket_path": "/tmp/sock", "session_id": "$1",
		}); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	err := Dispatch([]string{"become", "alias-routing"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "--from") {
		t.Fatalf("want --from requirement error, got %v (out %q)", err, buf.String())
	}
	if err := Dispatch([]string{"become", "alias-routing", "--from", "cost-audit"}, &buf); err != nil {
		t.Fatalf("explicit --from: %v", err)
	}
}

// TestBecomeRejectsEmptyAlias is finding F4: `muster become ""` (or a
// whitespace-only name) must error before dialing the daemon at all,
// mirroring cmdRegister's empty-alias rejection — a blank claimed name would
// round-trip into a row nothing could ever address again.
func TestBecomeRejectsEmptyAlias(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1"})
	t.Cleanup(func() { tmuxenv.Run = prev })
	if _, err := callData("register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$1",
	}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"", "   "} {
		var buf bytes.Buffer
		err := Dispatch([]string{"become", name}, &buf)
		if err == nil {
			t.Fatalf("become %q: want an error, got none (out %q)", name, buf.String())
		}
		if buf.Len() != 0 {
			t.Fatalf("become %q: must not print anything before erroring, got %q", name, buf.String())
		}
	}

	// No row must have been created for either rejected name.
	if _, found := hookGetAgent(""); found {
		t.Fatal("empty alias must never reach the daemon")
	}
	if _, found := hookGetAgent("   "); found {
		t.Fatal("whitespace-only alias must never reach the daemon")
	}
}
