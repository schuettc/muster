package humancli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

func TestLabelSetsOptionsAndRefreshes(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	var calls [][]string
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdLabel([]string{"backend"}, &buf); err != nil {
		t.Fatalf("label: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("expected 3 tmux calls (set label, set manual, refresh), got %v", calls)
	}
	if calls[0][0] != "set-option" || calls[0][1] != "@claude_task" || calls[0][2] != "backend" {
		t.Fatalf("unexpected label set call: %v", calls[0])
	}
	if calls[1][0] != "set-option" || calls[1][1] != "@claude_task_manual" || calls[1][2] != "1" {
		t.Fatalf("unexpected manual set call: %v", calls[1])
	}
	if calls[2][0] != "refresh-client" || calls[2][1] != "-S" {
		t.Fatalf("unexpected refresh call: %v", calls[2])
	}
	if !strings.Contains(buf.String(), `"backend"`) || !strings.Contains(buf.String(), "@claude_task") {
		t.Fatalf("unexpected confirmation output: %q", buf.String())
	}
}

func TestLabelClearUnsetsBothOptions(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	var calls [][]string
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdLabel([]string{"--clear"}, &buf); err != nil {
		t.Fatalf("label --clear: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("expected 3 tmux calls (unset label, unset manual, refresh), got %v", calls)
	}
	if calls[0][0] != "set-option" || calls[0][1] != "-u" || calls[0][2] != "@claude_task" {
		t.Fatalf("unexpected label unset call: %v", calls[0])
	}
	if calls[1][0] != "set-option" || calls[1][1] != "-u" || calls[1][2] != "@claude_task_manual" {
		t.Fatalf("unexpected manual unset call: %v", calls[1])
	}
	if !strings.Contains(buf.String(), "cleared") {
		t.Fatalf("unexpected confirmation output: %q", buf.String())
	}
}

func TestLabelEmptyNameActsLikeClear(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	var calls [][]string
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdLabel(nil, &buf); err != nil {
		t.Fatalf("label with no args: %v", err)
	}
	if len(calls) != 3 || calls[0][1] != "-u" {
		t.Fatalf("expected an unset sequence for empty name, got %v", calls)
	}
}

func TestLabelRequiresTmux(t *testing.T) {
	t.Setenv("TMUX", "")
	var buf bytes.Buffer
	err := cmdLabel([]string{"backend"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "tmux") {
		t.Fatalf("expected a tmux-mentioning error, got %v", err)
	}
}
