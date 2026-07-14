package nudge

import (
	"strings"
	"testing"
)

func recorder() (*[][]string, func(args ...string) error) {
	var calls [][]string
	return &calls, func(args ...string) error { calls = append(calls, args); return nil }
}

func TestNudgeClaudeTypesAndSubmits(t *testing.T) {
	calls, run := recorder()
	n := TmuxNudger{Run: run}
	submitted, err := n.Nudge("/s", "%1", "claude", true)
	if err != nil || !submitted {
		t.Fatalf("claude submit: submitted=%v err=%v", submitted, err)
	}
	joined := ""
	for _, c := range *calls {
		joined += strings.Join(c, " ") + "\n"
	}
	if !strings.Contains(joined, "send-keys") || !strings.Contains(joined, "-t %1") || !strings.Contains(joined, "Enter") {
		t.Fatalf("expected send-keys + Enter for claude:\n%s", joined)
	}
}

func TestNudgeCodexTypesOnly(t *testing.T) {
	calls, run := recorder()
	n := TmuxNudger{Run: run}
	submitted, err := n.Nudge("/s", "%2", "codex", true)
	if err != nil {
		t.Fatalf("codex: %v", err)
	}
	if submitted {
		t.Fatalf("codex must report submitted=false (its TUI ignores send-keys Enter)")
	}
	joined := ""
	for _, c := range *calls {
		joined += strings.Join(c, " ") + "\n"
	}
	if strings.Contains(joined, "Enter") {
		t.Fatalf("codex nudge must not send Enter:\n%s", joined)
	}
}
