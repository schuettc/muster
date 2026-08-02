package humancli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/schuettc/muster/internal/clock"
	"github.com/schuettc/muster/internal/store"
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

// registerClaudeViaDaemon registers a live claude-model agent with a pane —
// the row shape `muster label`'s /rename gate looks for.
func registerClaudeViaDaemon(t *testing.T, alias, socketPath, sessionID, paneID string) {
	t.Helper()
	registerModelViaDaemon(t, alias, socketPath, sessionID, paneID, "claude")
}

func registerModelViaDaemon(t *testing.T, alias, socketPath, sessionID, paneID, model string) {
	t.Helper()
	if _, err := callData("register_agent", map[string]any{
		"alias": alias, "socket_path": socketPath, "session_id": sessionID,
		"pane_id": paneID, "model_type": model,
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

// TestDeregisterUsesAliasPrecedence covers the deregistration tombstone: the
// row must SURVIVE (spec: departed history stays drillable), marked Departed,
// not vanish.
func TestDeregisterUsesAliasPrecedence(t *testing.T) {
	sock := startCLITestDaemon(t)
	registerViaDaemon(t, sock, "gone", "/s", "$1")

	var buf bytes.Buffer
	if err := cmdDeregister([]string{"gone"}, &buf); err != nil {
		t.Fatal(err)
	}
	agents := listAgentsForTest(t, sock)
	if len(agents) != 1 || agents[0].Alias != "gone" || !agents[0].Departed {
		t.Fatalf("expected 'gone' to survive deregister, tombstoned, got %+v", agents)
	}
}

// TestGCTombstonesOnlyDeadAgents covers gc's new default behavior (spec: "gc's
// agent-reaping no longer deletes ANY rows by default"): the dead agent's row
// survives, marked Departed, while the alive one is untouched.
func TestGCTombstonesOnlyDeadAgents(t *testing.T) {
	sock := startCLITestDaemon(t)
	// register two agents directly via the daemon: one "alive", one "dead"
	registerViaDaemon(t, sock, "alive", "/s", "$ALIVE")
	registerViaDaemon(t, sock, "dead", "/s", "$DEAD")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		// the liveness probe (display-message … '#{session_created}', see
		// tmuxenv.IsSessionAlive) answers only for $ALIVE
		if len(args) >= 7 && args[6] == "#{session_created}" {
			if args[5] == "$ALIVE" {
				return "1784000000", nil
			}
			return "", fmt.Errorf("dead")
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdGC(nil, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "gc: tombstoned 1 agent(s)") {
		t.Fatalf("expected gc output to report tombstoning 1 agent, got %q", buf.String())
	}
	agents := listAgentsForTest(t, sock)
	if len(agents) != 2 {
		t.Fatalf("expected BOTH rows to survive gc's default reap, got %+v", agents)
	}
	byAlias := map[string]agentRow{}
	for _, a := range agents {
		byAlias[a.Alias] = a
	}
	if byAlias["alive"].Departed {
		t.Fatalf("the still-alive agent must not be tombstoned, got %+v", byAlias["alive"])
	}
	if !byAlias["dead"].Departed {
		t.Fatalf("the dead agent must be tombstoned, got %+v", byAlias["dead"])
	}

	// Running gc again must be idempotent: nothing new to tombstone.
	buf.Reset()
	if err := cmdGC(nil, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "gc: tombstoned 0 agent(s)") {
		t.Fatalf("expected a second gc run to tombstone 0 (already departed/still alive), got %q", buf.String())
	}
}

// TestGCPurgeAgentsHardDeletes covers `muster gc --purge-agents`: the old
// hard-delete behavior, now explicit — a departed row (from a prior plain
// gc/deregister) and a currently-dead-but-never-deregistered row are BOTH
// removed for good; a live agent is left alone.
func TestGCPurgeAgentsHardDeletes(t *testing.T) {
	sock := startCLITestDaemon(t)
	registerViaDaemon(t, sock, "alive", "/s", "$ALIVE")
	registerViaDaemon(t, sock, "already-departed", "/s", "$OLD")
	registerViaDaemon(t, sock, "freshly-dead", "/s", "$DEAD")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if len(args) >= 7 && args[6] == "#{session_created}" {
			if args[5] == "$ALIVE" {
				return "1784000000", nil
			}
			return "", fmt.Errorf("dead")
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	if _, err := callData("deregister_agent", map[string]any{"alias": "already-departed"}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := cmdGC([]string{"--purge-agents"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "gc: purged 2 agent(s)") {
		t.Fatalf("expected gc --purge-agents to report purging 2 agents, got %q", buf.String())
	}
	agents := listAgentsForTest(t, sock)
	if len(agents) != 1 || agents[0].Alias != "alive" {
		t.Fatalf("after --purge-agents=%+v (want only 'alive' left)", agents)
	}
}

// TestGCPrunesEventsPastRetention drives `gc --events-keep 1h` against the
// test daemon: one event stamped 2h in the past (older than the 1h cutoff,
// must be pruned) and one stamped now (must survive). The clock is faked so
// the old event's ts is directly comparable against a real 1h duration —
// threads_test.go's fakeTick (tiny incrementing ints) can't do that, since a
// 1h cutoff would dwarf any tick-based timestamp.
func TestGCPrunesEventsPastRetention(t *testing.T) {
	now := time.Now().Add(-2 * time.Hour).UnixMilli()
	clock.SetForTesting(func() int64 { return now })
	t.Cleanup(clock.ResetForTesting)

	s := startTestDaemon(t)
	if err := s.AppendEvent(store.Event{Kind: "read", Agent: "old"}); err != nil {
		t.Fatal(err)
	}
	now = time.Now().UnixMilli() // advance the fake clock to "now"
	if err := s.AppendEvent(store.Event{Kind: "read", Agent: "recent"}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Dispatch([]string{"gc", "--events-keep", "1h"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "pruned 1 event(s)") {
		t.Fatalf("expected %q to report pruned 1 event(s)", buf.String())
	}
	left, err := s.Events(store.EventQuery{Backlog: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Agent != "recent" {
		t.Fatalf("expected only the recent event to survive, got %+v", left)
	}
}

// TestGCRejectsNonPositiveEventsKeep ensures --events-keep <= 0 is rejected
// before any daemon call is made.
func TestGCRejectsNonPositiveEventsKeep(t *testing.T) {
	startTestDaemon(t)
	var buf bytes.Buffer
	err := Dispatch([]string{"gc", "--events-keep", "0s"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "--events-keep must be > 0") {
		t.Fatalf("expected --events-keep must be > 0 error, got %v", err)
	}
}

// tmuxCaptureStub wires tmuxenv.Run to answer the register-path queries used
// by cmdRegister's tmux-anchored branch: #{session_id} and #{session_name}
// from the given values, everything else (the project/branch probe) with a
// constant benign answer. Mirrors TestRegisterUsesAliasPrecedenceAndCaptures's
// inline stub, factored out so the --if-absent tests can vary just the
// session_id (the tuple's second half — socket_path comes from $TMUX itself).
func tmuxCaptureStub(t *testing.T, sessionID, sessionName string) {
	t.Helper()
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		switch args[len(args)-1] {
		case "#{session_id}":
			return sessionID, nil
		case "#{session_name}":
			return sessionName, nil
		default:
			return "frontend\x1f1", nil
		}
	}
	t.Cleanup(func() { tmuxenv.Run = prev })
}

// TestRegisterIfAbsentConflict covers the CAS the whole feature exists for: a
// launch wrapper seeding a tmux-session-name alias must not silently steal it
// out from under a LIVE claimed identity. Alias "seed" is already registered
// to tuple A ($A); a --if-absent register capturing a DIFFERENT tuple ($B)
// must fail with "if_absent conflict" and leave the row on tuple A untouched.
func TestRegisterIfAbsentConflict(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/tmux-0/proj-muster,1,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("MUSTER_ALIAS", "")
	socketPath := "/tmp/tmux-0/proj-muster"
	registerViaDaemon(t, "", "seed", socketPath, "$A")

	tmuxCaptureStub(t, "$B", "seed")

	var buf bytes.Buffer
	err := cmdRegister([]string{"seed", "--if-absent"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "if_absent conflict") {
		t.Fatalf("expected an if_absent conflict error, got %v", err)
	}
	agents := listAgentsForTest(t, "")
	if len(agents) != 1 || agents[0].SessionID != "$A" {
		t.Fatalf("expected the row to remain on tuple A untouched, got %+v", agents)
	}
}

// TestRegisterIfAbsentSameTupleIdempotent covers the non-conflicting case:
// re-registering with --if-absent from the SAME (socket_path, session_id)
// tuple already on the row is a no-op upsert, not a conflict — a launch
// wrapper re-running against its own already-seeded identity must succeed.
func TestRegisterIfAbsentSameTupleIdempotent(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/tmux-0/proj-muster,1,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("MUSTER_ALIAS", "")
	socketPath := "/tmp/tmux-0/proj-muster"
	registerViaDaemon(t, "", "seed2", socketPath, "$SAME")

	tmuxCaptureStub(t, "$SAME", "seed2")

	var buf bytes.Buffer
	if err := cmdRegister([]string{"seed2", "--if-absent"}, &buf); err != nil {
		t.Fatalf("same-tuple --if-absent register should succeed idempotently, got %v", err)
	}
	agents := listAgentsForTest(t, "")
	if len(agents) != 1 || agents[0].SessionID != "$SAME" {
		t.Fatalf("expected the row unchanged on the same tuple, got %+v", agents)
	}
}

// TestRegisterIfAbsentOnFreshAlias covers the absent case: --if-absent
// against an alias with no existing row at all must succeed and create it —
// the CAS only ever blocks a DIFFERENT-tuple collision, never a fresh claim.
func TestRegisterIfAbsentOnFreshAlias(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/tmux-0/proj-muster,1,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("MUSTER_ALIAS", "")
	tmuxCaptureStub(t, "$FRESH", "freshalias")

	var buf bytes.Buffer
	if err := cmdRegister([]string{"freshalias", "--if-absent"}, &buf); err != nil {
		t.Fatalf("--if-absent on a fresh alias should succeed, got %v", err)
	}
	agents := listAgentsForTest(t, "")
	if len(agents) != 1 || agents[0].Alias != "freshalias" {
		t.Fatalf("expected a fresh row to be created, got %+v", agents)
	}
}

// TestRegisterIfAbsentRejectedForPaneless covers the paneless-path decision:
// allocPanelessAlias already runs its own internal if_absent CAS per
// candidate suffix, so layering the CLI flag on top has no clean meaning —
// cmdRegister must reject it with a clear, actionable error rather than
// silently ignoring it or picking an arbitrary interpretation.
func TestRegisterIfAbsentRejectedForPaneless(t *testing.T) {
	startTestDaemon(t)
	panelessEnv(t, "hs-ifabsent", "wt-ifabsent")

	var buf bytes.Buffer
	err := cmdRegister([]string{"--if-absent"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "--if-absent is for the tmux-anchored seed path") {
		t.Fatalf("expected a paneless rejection error, got %v", err)
	}
}

// TestRegisterPrintsRevivalAndUnread covers the resume loop's CLI surface:
// re-registering a departed alias that accrued mail must say so, so the
// operator (or a resumed agent using the CLI) learns the backlog in the
// same command.
func TestRegisterPrintsRevivalAndUnread(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("MUSTER_ALIAS", "")
	seed := func(op string, args map[string]any) {
		t.Helper()
		if _, err := callData(op, args); err != nil {
			t.Fatal(err)
		}
	}
	seed("register_agent", map[string]any{"alias": "backend", "socket_path": "/s", "session_id": "$1"})
	seed("register_agent", map[string]any{"alias": "sender", "socket_path": "/s", "session_id": "$2"})
	seed("send_message", map[string]any{"from": "sender", "to_kind": "agent", "to_target": "backend", "subject": "hi", "body": "b"})
	seed("deregister_agent", map[string]any{"alias": "backend"})

	var buf bytes.Buffer
	if err := cmdRegister([]string{"backend"}, &buf); err != nil {
		t.Fatalf("register: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "revived") || !strings.Contains(out, "1 unread") {
		t.Fatalf("register output missing revival/unread notice:\n%s", out)
	}
}
