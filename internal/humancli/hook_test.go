package humancli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

func TestHookStopLoopGuard(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{"stop_hook_active":true}`), &buf); err != nil {
		t.Fatalf("hook Stop: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output on loop guard, got %q", buf.String())
	}
}

func TestHookStopNoTmux(t *testing.T) {
	t.Setenv("TMUX", "")
	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{}`), &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output outside tmux, got %q", buf.String())
	}
}

func TestHookStopNoUnread(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	prev := tmuxenv.Run
	t.Cleanup(func() { tmuxenv.Run = prev })
	for _, c := range []string{"", "abc", "0", "-1"} {
		count := c
		tmuxenv.Run = func(_ ...string) (string, error) { return count, nil }
		var buf bytes.Buffer
		if err := cmdHook([]string{"Stop"}, strings.NewReader(`{}`), &buf); err != nil {
			t.Fatal(err)
		}
		if buf.Len() != 0 {
			t.Fatalf("count=%q: expected no output, got %q", c, buf.String())
		}
	}
}

func TestHookStopUnreadEmitsBlockDecision(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		switch args[len(args)-1] {
		case "@muster_inbox":
			return "3", nil
		case "#{session_name}":
			return "backend", nil
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	// Invalid stdin JSON must be tolerated (treated as stop_hook_active=false),
	// still proceeding to the count-based decision below.
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`not json`), &buf); err != nil {
		t.Fatal(err)
	}
	var res struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("output not valid JSON: %v (%q)", err, buf.String())
	}
	if res.Decision != "block" {
		t.Fatalf("decision = %q, want block", res.Decision)
	}
	if !strings.Contains(res.Reason, "alias 'backend'") || !strings.Contains(res.Reason, "3 unread") {
		t.Fatalf("reason missing expected fields: %q", res.Reason)
	}
}

func TestHookSessionStartAndEnd(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/tmux-0/proj-muster,1,0")
	t.Setenv("TMUX_PANE", "%0")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		switch args[len(args)-1] {
		case "#{session_id}":
			return "$1", nil
		case "#{session_name}":
			return "muster-hook", nil
		default:
			return "", nil
		}
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart", "codex"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("SessionStart: %v", err)
	}
	agents := listAgentsForTest(t, "")
	found := false
	for _, a := range agents {
		if a.Alias == "muster-hook" && a.ModelType == "codex" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected muster-hook registered via SessionStart hook: %+v", agents)
	}

	buf.Reset()
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("SessionEnd: %v", err)
	}
	agents = listAgentsForTest(t, "")
	for _, a := range agents {
		if a.Alias == "muster-hook" {
			t.Fatalf("expected muster-hook deregistered via SessionEnd hook: %+v", agents)
		}
	}
}

func TestHookSessionStartBestEffortWhenDaemonUnreachable(t *testing.T) {
	// No test daemon started, and no tmux identity to fall back on: cmdRegister
	// will fail (can't determine alias / can't reach daemon), but the hook must
	// swallow that and still return nil — a hook must never block a session.
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("MUSTER_ALIAS", "")
	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("hook must never return an error, got %v", err)
	}
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("hook must never return an error, got %v", err)
	}
}
