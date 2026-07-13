package humancli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/store"
)

// startTestDaemon boots a real in-process daemon on a temp socket.
func startTestDaemon(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := daemon.Serve(paths.SocketPath(), s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
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
	if !strings.Contains(out, "backend") || !strings.Contains(out, "consumer") || !strings.Contains(out, "producer") {
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
