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

// TestLabelRenamesLiveClaudePane covers the third fan-out of `muster label`:
// a live claude-model registration on the ambient session tuple gets
// "/rename <name>" typed into its pane (and submitted), so the Claude Code
// session name follows the label. The gate is the roster + a live pane —
// see TestLabelSkipsRenameWithoutLiveClaude for every negative.
func TestLabelRenamesLiveClaudePane(t *testing.T) {
	startCLITestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	registerClaudeViaDaemon(t, "worker", "/tmp/sock", "$1", "%5")

	var sent [][]string
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		last := args[len(args)-1]
		if last == "#{session_id}" {
			return "$1", nil
		}
		if last == "#{pane_id}" {
			return "%5", nil // pane-alive probe answers: alive
		}
		if len(args) > 2 && args[2] == "send-keys" {
			sent = append(sent, append([]string(nil), args...))
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdLabel([]string{"standard 2000"}, &buf); err != nil {
		t.Fatalf("label: %v", err)
	}
	if len(sent) != 2 {
		t.Fatalf("expected /rename type + Enter submit, got %v", sent)
	}
	if got := sent[0][len(sent[0])-1]; got != "/rename standard 2000" {
		t.Fatalf("typed %q, want %q", got, "/rename standard 2000")
	}
	if sent[1][len(sent[1])-1] != "Enter" {
		t.Fatalf("expected Enter submit, got %v", sent[1])
	}
	if !strings.Contains(buf.String(), "renamed claude session") {
		t.Fatalf("expected rename confirmation in output, got %q", buf.String())
	}
}

// TestLabelSkipsRenameWithoutLiveClaude: no injection for (a) a codex row,
// (b) a departed claude row, (c) a claude row whose pane is dead, (d) a
// claude row on a DIFFERENT session tuple. The label/bus writes still happen.
func TestLabelSkipsRenameWithoutLiveClaude(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		sessionID string
		depart    bool
		paneAlive bool
	}{
		{"codex row", "codex", "$1", false, true},
		{"departed claude", "claude", "$1", true, true},
		{"dead pane", "claude", "$1", false, false},
		{"other session", "claude", "$9", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			startCLITestDaemon(t)
			t.Setenv("TMUX", "/tmp/sock,1,0")
			registerModelViaDaemon(t, "worker", "/tmp/sock", tc.sessionID, "%5", tc.model)
			if tc.depart {
				if _, err := callData("deregister_agent", map[string]any{"alias": "worker"}); err != nil {
					t.Fatal(err)
				}
			}
			var sent [][]string
			prev := tmuxenv.Run
			tmuxenv.Run = func(args ...string) (string, error) {
				last := args[len(args)-1]
				if last == "#{session_id}" {
					return "$1", nil
				}
				if last == "#{pane_id}" {
					if tc.paneAlive {
						return "%5", nil
					}
					return "", nil // dead pane
				}
				if len(args) > 2 && args[2] == "send-keys" {
					sent = append(sent, append([]string(nil), args...))
				}
				return "", nil
			}
			t.Cleanup(func() { tmuxenv.Run = prev })

			var buf bytes.Buffer
			if err := cmdLabel([]string{"datalake"}, &buf); err != nil {
				t.Fatalf("label: %v", err)
			}
			if len(sent) != 0 {
				t.Fatalf("expected NO injection for %s, got %v", tc.name, sent)
			}
		})
	}
}

// TestLabelClearNeverInjects: clearing a label must not type anything into
// any pane — there is no "/rename to nothing" gesture worth sending.
func TestLabelClearNeverInjects(t *testing.T) {
	startCLITestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	registerClaudeViaDaemon(t, "worker", "/tmp/sock", "$1", "%5")
	var sent [][]string
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		last := args[len(args)-1]
		if last == "#{session_id}" {
			return "$1", nil
		}
		if last == "#{pane_id}" {
			return "%5", nil
		}
		if len(args) > 2 && args[2] == "send-keys" {
			sent = append(sent, append([]string(nil), args...))
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })
	var buf bytes.Buffer
	if err := cmdLabel([]string{"--clear"}, &buf); err != nil {
		t.Fatalf("label --clear: %v", err)
	}
	if len(sent) != 0 {
		t.Fatalf("clear must not inject, got %v", sent)
	}
}
