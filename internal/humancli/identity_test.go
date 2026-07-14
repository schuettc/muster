package humancli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// startCLITestDaemon boots a real in-process daemon on a temp socket and
// returns the socket path (kept for parity with the brief's helper name;
// callData always targets paths.SocketPath(), which is set to agree with it).
func startCLITestDaemon(t *testing.T) string {
	t.Helper()
	startTestDaemon(t)
	return ""
}

// listAgentsForTest fetches list_agents through the same callData path the
// commands use, decoded into agentRow.
func listAgentsForTest(t *testing.T, _ string) []agentRow {
	t.Helper()
	raw, err := callData("list_agents", nil)
	if err != nil {
		t.Fatal(err)
	}
	var agents []agentRow
	if err := json.Unmarshal(raw, &agents); err != nil {
		t.Fatal(err)
	}
	return agents
}

// registerViaDaemon registers an agent directly through the daemon op,
// bypassing tmux capture, so gc tests can set up known-alive/known-dead rows.
func registerViaDaemon(t *testing.T, _ string, alias, socketPath, sessionID string) {
	t.Helper()
	if _, err := callData("register_agent", map[string]any{
		"alias": alias, "socket_path": socketPath, "session_id": sessionID,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterUsesAliasPrecedenceAndCaptures(t *testing.T) {
	sock := startCLITestDaemon(t)
	t.Setenv("TMUX", "/tmp/tmux-0/proj-muster,1,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		switch args[len(args)-1] {
		case "#{session_id}":
			return "$1", nil
		case "#{session_name}":
			return "muster-2", nil
		default:
			return "frontend\x1f1", nil
		}
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	// no positional alias, no $MUSTER_ALIAS → alias falls back to session name
	if err := cmdRegister([]string{"--model", "codex", "--role", "peer"}, &buf); err != nil {
		t.Fatal(err)
	}
	// verify via list_agents that alias == "muster-2", project=="muster", label=="frontend"
	agents := listAgentsForTest(t, sock)
	if len(agents) != 1 || agents[0].Alias != "muster-2" || agents[0].Project != "muster" || agents[0].Label != "frontend" || !agents[0].LabelManual {
		t.Fatalf("registered=%+v", agents)
	}

	// explicit positional alias wins over session name
	buf.Reset()
	if err := cmdRegister([]string{"backend", "--model", "codex"}, &buf); err != nil {
		t.Fatal(err)
	}
	agents = listAgentsForTest(t, sock)
	found := false
	for _, a := range agents {
		if a.Alias == "backend" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'backend' among registered agents: %+v", agents)
	}
}

func TestDeregisterUsesAliasPrecedence(t *testing.T) {
	sock := startCLITestDaemon(t)
	registerViaDaemon(t, sock, "gone", "/s", "$1")

	var buf bytes.Buffer
	if err := cmdDeregister([]string{"gone"}, &buf); err != nil {
		t.Fatal(err)
	}
	agents := listAgentsForTest(t, sock)
	if len(agents) != 0 {
		t.Fatalf("expected no agents after deregister, got %+v", agents)
	}
}

func TestGCReapsOnlyDeadAgents(t *testing.T) {
	sock := startCLITestDaemon(t)
	// register two agents directly via the daemon: one "alive", one "dead"
	registerViaDaemon(t, sock, "alive", "/s", "$ALIVE")
	registerViaDaemon(t, sock, "dead", "/s", "$DEAD")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		// has-session succeeds only for $ALIVE
		if len(args) >= 5 && args[2] == "has-session" && args[4] == "$ALIVE" {
			return "", nil
		}
		if len(args) >= 3 && args[2] == "has-session" {
			return "", fmt.Errorf("dead")
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdGC(&buf); err != nil {
		t.Fatal(err)
	}
	agents := listAgentsForTest(t, sock)
	if len(agents) != 1 || agents[0].Alias != "alive" {
		t.Fatalf("after gc=%+v (want only 'alive')", agents)
	}
}
