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
	// 3 writes + the bus-sync session-id probe (which the stub answers "",
	// so no daemon call follows — see syncLabelToBus).
	if len(calls) != 4 {
		t.Fatalf("expected 4 tmux calls (set label, set manual, refresh, session-id probe), got %v", calls)
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
	if len(calls) != 4 {
		t.Fatalf("expected 4 tmux calls (unset label, unset manual, refresh, session-id probe), got %v", calls)
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
	if len(calls) != 4 || calls[0][1] != "-u" {
		t.Fatalf("expected an unset sequence for empty name, got %v", calls)
	}
}

// TestLabelSyncsStoredLabelToBus covers syncLabelToBus end to end: `muster
// label` must land the label in the STORE for every alias on the ambient
// session (the daemon's resolver reads only the stored copy — see
// daemon.resolveAgentTarget), and `muster label --clear` must clear it.
// Without this push, a CLI sender (live tmux labels) and an MCP sender
// (stored labels) would resolve the same label differently until the
// session's next re-register.
func TestLabelSyncsStoredLabelToBus(t *testing.T) {
	sock := startCLITestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	registerViaDaemon(t, sock, "worker", "/tmp/sock", "$1")
	registerViaDaemon(t, sock, "worker-2", "/tmp/sock", "$1")
	registerViaDaemon(t, sock, "bystander", "/tmp/sock", "$9")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if args[len(args)-1] == "#{session_id}" {
			return "$1", nil
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdLabel([]string{"datalake"}, &buf); err != nil {
		t.Fatalf("label: %v", err)
	}
	if strings.Contains(buf.String(), "warning") {
		t.Fatalf("sync must succeed against the test daemon, got %q", buf.String())
	}
	want := map[string]string{"worker": "datalake", "worker-2": "datalake", "bystander": ""}
	for _, a := range listAgentsForTest(t, sock) {
		if a.Label != want[a.Alias] || (a.Label != "" && !a.LabelManual) {
			t.Errorf("%s: stored label=(%q, manual=%v), want %q manual", a.Alias, a.Label, a.LabelManual, want[a.Alias])
		}
	}

	if err := cmdLabel([]string{"--clear"}, &buf); err != nil {
		t.Fatalf("label --clear: %v", err)
	}
	for _, a := range listAgentsForTest(t, sock) {
		if a.Alias != "bystander" && (a.Label != "" || a.LabelManual) {
			t.Errorf("%s: stored label must clear, got (%q, manual=%v)", a.Alias, a.Label, a.LabelManual)
		}
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
