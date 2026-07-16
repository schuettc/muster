package humancli

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/mustertest"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/store"
)

// startTestDaemon boots a real in-process daemon on a temp socket, returning
// the underlying store so tests can seed rows (e.g. events at a controlled
// timestamp) directly, bypassing the wire protocol.
func startTestDaemon(t *testing.T) *store.Store {
	t.Helper()
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := daemon.Serve(paths.SocketPath(), s, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return s
}

func TestAgentsCommandListsRegistered(t *testing.T) {
	startTestDaemon(t)
	// Register two agents directly via the daemon op (through Dispatch's helper).
	if _, err := callData("register_agent", map[string]any{"alias": "backend", "role": "producer", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Dispatch([]string{"agents"}, &buf); err != nil {
		t.Fatalf("agents: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "backend") || !strings.Contains(out, "consumer") || !strings.Contains(out, "claude") || !strings.Contains(out, "codex") {
		t.Fatalf("agents output missing rows:\n%s", out)
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	if err := Dispatch([]string{"bogus"}, nil); err == nil {
		t.Fatalf("expected error for unknown subcommand")
	}
}

func TestSendThenInboxShowsMessage(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	var sendBuf bytes.Buffer
	if err := Dispatch([]string{"send", "consumer", "the API changed", "--from", "backend", "--subject", "heads up"}, &sendBuf); err != nil {
		t.Fatalf("send: %v", err)
	}
	var inboxBuf bytes.Buffer
	if err := Dispatch([]string{"inbox", "consumer"}, &inboxBuf); err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if !strings.Contains(inboxBuf.String(), "heads up") {
		t.Fatalf("inbox missing sent message:\n%s", inboxBuf.String())
	}
}

func TestTasksCommandShowsOnlyTasks(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "rev", "role": "reviewer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	// One message and one task addressed to rev's role.
	if _, err := callData("send_message", map[string]any{"from": "backend", "to_kind": "role", "to_target": "reviewer", "subject": "just a note", "body": "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("task_create", map[string]any{"from": "backend", "to_kind": "role", "to_target": "reviewer", "subject": "please review", "body": "y"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Dispatch([]string{"tasks", "rev"}, &buf); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "please review") {
		t.Fatalf("tasks output missing the task:\n%s", out)
	}
	if strings.Contains(out, "just a note") {
		t.Fatalf("tasks output should exclude the plain message:\n%s", out)
	}
}

func TestNudgeCommandRejectsUnknownAlias(t *testing.T) {
	startTestDaemon(t)
	var buf bytes.Buffer
	if err := Dispatch([]string{"nudge", "ghost"}, &buf); err == nil {
		t.Fatalf("expected error nudging an unregistered alias")
	}
}

func TestNudgeCommandResolvesAndNudges(t *testing.T) {
	startTestDaemon(t)
	// register an agent with a pane via the daemon op directly
	if _, err := callData("register_agent", map[string]any{"alias": "rev", "role": "reviewer", "model_type": "codex", "socket_path": "/s", "pane_id": "%2", "session_id": "$1"}); err != nil {
		t.Fatal(err)
	}
	var recorded [][]string
	origNudge := nudgeRun
	nudgeRun = func(args ...string) error { recorded = append(recorded, args); return nil }
	t.Cleanup(func() { nudgeRun = origNudge })

	var buf bytes.Buffer
	if err := Dispatch([]string{"nudge", "rev"}, &buf); err != nil {
		t.Fatalf("nudge: %v", err)
	}
	if !strings.Contains(buf.String(), "rev") || len(recorded) == 0 {
		t.Fatalf("expected resolved-target output + a send-keys call; out=%q calls=%v", buf.String(), recorded)
	}
}

func TestSplitFlagsAndPositional(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		wantFlagArgs   []string
		wantPositional []string
	}{
		{
			name:           "flags after positionals",
			args:           []string{"consumer", "the body", "--from", "backend", "--subject", "heads up"},
			wantFlagArgs:   []string{"--from", "backend", "--subject", "heads up"},
			wantPositional: []string{"consumer", "the body"},
		},
		{
			name:           "boolean flag does not consume the next token",
			args:           []string{"rev", "please review", "--role"},
			wantFlagArgs:   []string{"--role"},
			wantPositional: []string{"rev", "please review"},
		},
		{
			name:           "broadcast bool flag plus body",
			args:           []string{"--broadcast", "hello world"},
			wantFlagArgs:   []string{"--broadcast"},
			wantPositional: []string{"hello world"},
		},
		{
			name:           "equals form keeps flag and value together",
			args:           []string{"--from=backend", "x", "y"},
			wantFlagArgs:   []string{"--from=backend"},
			wantPositional: []string{"x", "y"},
		},
		{
			name:           "missing value at end does not panic",
			args:           []string{"a", "b", "--from"},
			wantFlagArgs:   []string{"--from"},
			wantPositional: []string{"a", "b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flagArgs, positional := splitFlagsAndPositional(tc.args)
			if !reflect.DeepEqual(flagArgs, tc.wantFlagArgs) {
				t.Errorf("flagArgs = %#v, want %#v", flagArgs, tc.wantFlagArgs)
			}
			if !reflect.DeepEqual(positional, tc.wantPositional) {
				t.Errorf("positional = %#v, want %#v", positional, tc.wantPositional)
			}
		})
	}
}
