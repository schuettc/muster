package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
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

// registerAliveViaDaemon registers an agent with an explicit non-zero
// session_created — spec §5.1 (2026-08-05) made session_created=0 (what
// registerViaDaemon stores) never read alive, so any test row that needs to
// pass tmuxenv.IsSessionAlive must record a real incarnation stamp and have
// its stub answer the matching value for #{session_created}.
func registerAliveViaDaemon(t *testing.T, alias, socketPath, sessionID string, created int64) {
	t.Helper()
	if _, err := callData("register_agent", map[string]any{
		"alias": alias, "socket_path": socketPath, "session_id": sessionID,
		"session_created": created,
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
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
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
	// verify via list_agents that alias == the seeded session name, project=="muster", label=="frontend"
	agents := listAgentsForTest(t, sock)
	if len(agents) != 1 || agents[0].Alias != "testdev-muster-2" || agents[0].Project != "muster" || agents[0].Label != "frontend" || !agents[0].LabelManual {
		t.Fatalf("registered=%+v", agents)
	}

	// explicit positional alias wins over session name, and is seeded exactly
	// like the derived one — every mint site seeds, no exceptions.
	buf.Reset()
	if err := cmdRegister([]string{"backend", "--model", "codex"}, &buf); err != nil {
		t.Fatal(err)
	}
	agents = listAgentsForTest(t, sock)
	found := false
	for _, a := range agents {
		if a.Alias == "testdev-backend" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'testdev-backend' among registered agents: %+v", agents)
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

// TestDeregisterExpandsExplicitArgLocalFirst is the self-review case: the
// roster holds BOTH a local seeded row and a foreign bare row sharing the
// same short name. `muster deregister work` must depart the LOCAL one — on
// this machine a short name is mine — never the foreign row a stranger
// happens to have registered under the bare string.
func TestDeregisterExpandsExplicitArgLocalFirst(t *testing.T) {
	sock := startCLITestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	registerViaDaemon(t, sock, "personal-work", "/s", "$1")
	registerViaDaemon(t, sock, "work", "/s2", "$2")

	var buf bytes.Buffer
	if err := cmdDeregister([]string{"work"}, &buf); err != nil {
		t.Fatal(err)
	}
	agents := listAgentsForTest(t, sock)
	var local, foreign agentRow
	for _, a := range agents {
		switch a.Alias {
		case "personal-work":
			local = a
		case "work":
			foreign = a
		}
	}
	if !local.Departed {
		t.Fatalf("local row 'personal-work' must be departed, got %+v", local)
	}
	if foreign.Departed {
		t.Fatalf("foreign row 'work' must survive untouched, got %+v", foreign)
	}
}

// TestDeregisterMusterAliasEnvBranchSeeds covers the no-arg $MUSTER_ALIAS
// branch. This is RECONSTRUCTING the exact alias cmdRegister would have
// minted for the same env var (register's own MUSTER_ALIAS branch seeds —
// see cmdRegister), not looking up an operator-typed short name — so the
// derivation must seed, not expand.
func TestDeregisterMusterAliasEnvBranchSeeds(t *testing.T) {
	sock := startCLITestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	t.Setenv("MUSTER_ALIAS", "worker")
	registerViaDaemon(t, sock, "personal-worker", "/s", "$1")

	var buf bytes.Buffer
	if err := cmdDeregister(nil, &buf); err != nil {
		t.Fatal(err)
	}
	agents := listAgentsForTest(t, sock)
	if len(agents) != 1 || agents[0].Alias != "personal-worker" || !agents[0].Departed {
		t.Fatalf("expected 'personal-worker' (the seeded MUSTER_ALIAS derivation) departed, got %+v", agents)
	}
}

// TestDeregisterSessionNameBranchSeeds covers the no-arg tmux-session-name
// branch, mirroring cmdRegister's own derivation of the same value
// (TestRegisterUsesAliasPrecedenceAndCaptures) — bare `muster deregister`
// inside the tmux session that registered must find the row it minted, not
// an unseeded near-miss.
func TestDeregisterSessionNameBranchSeeds(t *testing.T) {
	sock := startCLITestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	t.Setenv("MUSTER_ALIAS", "")
	t.Setenv("TMUX", "/tmp/tmux-0/proj-muster,1,0")
	t.Setenv("TMUX_PANE", "%0")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1", "#{session_name}": "muster-2"})
	t.Cleanup(func() { tmuxenv.Run = prev })
	registerViaDaemon(t, sock, "personal-muster-2", "/tmp/tmux-0/proj-muster", "$1")

	var buf bytes.Buffer
	if err := cmdDeregister(nil, &buf); err != nil {
		t.Fatal(err)
	}
	agents := listAgentsForTest(t, sock)
	if len(agents) != 1 || agents[0].Alias != "personal-muster-2" || !agents[0].Departed {
		t.Fatalf("expected 'personal-muster-2' (the seeded session-name derivation) departed, got %+v", agents)
	}
}

// TestDeregisterPanelessFallbackBranchSeeds covers the last-resort paneless
// branch (no tmux, no MUSTER_ALIAS, no harness-owned rows): the cwd
// basename fallback, mirroring cmdRegister's `seedAlias(h.Alias())` (see
// cmdRegister's paneless default case).
func TestDeregisterPanelessFallbackBranchSeeds(t *testing.T) {
	sock := startCLITestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	t.Setenv("MUSTER_ALIAS", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	// This test process is itself running inside a real tmux pane — without
	// clearing these, tmuxenv.CaptureEnv would pick up THAT session instead
	// of exercising the paneless fallback this test targets.
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	dir := t.TempDir()
	work := dir + "/some-project"
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	registerViaDaemon(t, sock, "personal-some-project", "", "")

	var buf bytes.Buffer
	if err := cmdDeregister(nil, &buf); err != nil {
		t.Fatal(err)
	}
	agents := listAgentsForTest(t, sock)
	if len(agents) != 1 || agents[0].Alias != "personal-some-project" || !agents[0].Departed {
		t.Fatalf("expected 'personal-some-project' (the seeded cwd-basename fallback) departed, got %+v", agents)
	}
}

// TestGCTombstonesOnlyDeadAgents covers gc's new default behavior (spec: "gc's
// agent-reaping no longer deletes ANY rows by default"): the dead agent's row
// survives, marked Departed, while the alive one is untouched.
func TestGCTombstonesOnlyDeadAgents(t *testing.T) {
	sock := startCLITestDaemon(t)
	// register two agents directly via the daemon: one "alive" (a real,
	// non-zero session_created — spec §5.1 — matching the stub's answer
	// below), one "dead"
	registerAliveViaDaemon(t, "alive", "/s", "$ALIVE", 1784000000)
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

// TestGCSweepsLegacyZeroCreatedRows pins the spec §5.1 legacy sweep: a row
// that cannot prove its incarnation (session_created = 0, the pre-v0.8.0
// population) is tombstoned by plain `muster gc` EVEN IF a session with its
// recycled ID is currently live — attribution requires proof. The tombstone
// keeps history and revives on the row's next register, so a genuinely live
// pre-upgrade session self-heals.
func TestGCSweepsLegacyZeroCreatedRows(t *testing.T) {
	sock := startCLITestDaemon(t)
	registerViaDaemon(t, sock, "legacy", "/s", "$1") // registerViaDaemon stores session_created=0
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if len(args) >= 7 && args[6] == "#{session_created}" {
			return "1784000000", nil // the recycled $1 IS live — sweep must still fire
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdGC(nil, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "tombstoned legacy (legacy row") {
		t.Fatalf("expected legacy row tombstoned, got %q", buf.String())
	}
}

// TestGCPurgeAgentsHardDeletes covers `muster gc --purge-agents`: the old
// hard-delete behavior, now explicit — a departed row (from a prior plain
// gc/deregister) and a currently-dead-but-never-deregistered row are BOTH
// removed for good; a live agent is left alone.
func TestGCPurgeAgentsHardDeletes(t *testing.T) {
	sock := startCLITestDaemon(t)
	// "alive" needs a real, non-zero session_created (spec §5.1) matching
	// the stub's answer below, or it would never read alive.
	registerAliveViaDaemon(t, "alive", "/s", "$ALIVE", 1784000000)
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

// TestGCSparesOtherDevicesAgents is a data-loss guard, and the reason it
// exists is worth stating: on a hosted bus list_agents returns EVERY device's
// rows, while every liveness check in gc is a probe of THIS machine's tmux.
// A remote agent's socket path either matches nothing here — reading as dead
// — or matches an unrelated local session with the same per-machine path.
// Without device scoping, gc on the laptop tombstones (and with
// --purge-agents HARD DELETES) agents running perfectly well on the desktop.
func TestGCSparesOtherDevicesAgents(t *testing.T) {
	sock := startCLITestDaemon(t)
	t.Setenv("MUSTER_DEVICE_ID", "this-device")

	// A dead row belonging to THIS device: gc's rightful target.
	if _, err := callData("register_agent", map[string]any{
		"alias": "mine-dead", "socket_path": "/s", "session_id": "$DEAD",
		"session_created": 1784000000, "device_id": "this-device",
	}); err != nil {
		t.Fatal(err)
	}
	// A row on ANOTHER device. Its session is not in this machine's tmux, so
	// every probe here says "dead" — and it must survive anyway.
	if _, err := callData("register_agent", map[string]any{
		"alias": "theirs", "socket_path": "/s", "session_id": "$THEIRS",
		"session_created": 1784000001, "device_id": "other-device",
	}); err != nil {
		t.Fatal(err)
	}

	prev := tmuxenv.Run
	tmuxenv.Run = func(...string) (string, error) { return "", fmt.Errorf("dead") }
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdGC([]string{"--purge-agents"}, &buf); err != nil {
		t.Fatal(err)
	}
	agents := listAgentsForTest(t, sock)
	var survived []string
	for _, a := range agents {
		survived = append(survived, a.Alias)
	}
	if len(survived) != 1 || survived[0] != "theirs" {
		t.Fatalf("after gc --purge-agents, surviving agents = %v; want only [theirs] "+
			"(this device's dead row purged, the other device's row untouched)", survived)
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
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	t.Setenv("TMUX", "/tmp/tmux-0/proj-muster,1,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("MUSTER_ALIAS", "")
	socketPath := "/tmp/tmux-0/proj-muster"
	// The pre-existing row is seeded exactly as cmdRegister's typed-name path
	// will seed the incoming "seed" arg, so the collision is on the SAME row.
	registerViaDaemon(t, "", "testdev-seed", socketPath, "$A")

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

// TestRegisterIfAbsentRefusesForeignHarnessClaim pins the semantics the
// whole feature exists for: a fresh session's --if-absent seed must NOT take
// over an alias already LIVE-claimed by a different owner. Alias "seed-owned"
// is registered on tuple A carrying a real harness link (harness_session_id
// "uuid-owner" — a genuinely claimed identity, not a bare tmux row). A
// --if-absent register from tuple B, itself linked to a DIFFERENT harness
// UUID ("uuid-attacker"), must fail with "if_absent conflict" and leave the
// row on tuple A untouched — including its harness link, which a successful
// (bad) overwrite would have clobbered.
func TestRegisterIfAbsentRefusesForeignHarnessClaim(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	t.Setenv("TMUX", "/tmp/tmux-0/proj-muster,1,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("MUSTER_ALIAS", "")
	socketPath := "/tmp/tmux-0/proj-muster"
	// Seeded to match what cmdRegister's typed-name path will produce from
	// "seed-owned" below, so the CAS lands on the same row.
	if _, err := callData("register_agent", map[string]any{
		"alias": "testdev-seed-owned", "socket_path": socketPath, "session_id": "$A",
		"harness_session_id": "uuid-owner",
	}); err != nil {
		t.Fatal(err)
	}

	tmuxCaptureStub(t, "$B", "seed-owned")

	var buf bytes.Buffer
	err := cmdRegister([]string{"seed-owned", "--if-absent", "--harness-session", "uuid-attacker"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "if_absent conflict") {
		t.Fatalf("expected an if_absent conflict error refusing the foreign claim, got %v", err)
	}
	agents := listAgentsForTest(t, "")
	if len(agents) != 1 || agents[0].SessionID != "$A" || agents[0].HarnessSessionID != "uuid-owner" {
		t.Fatalf("expected the owned row to survive untouched (tuple A, harness uuid-owner), got %+v", agents)
	}
}

// TestRegisterIfAbsentSameTupleIdempotent covers the non-conflicting case:
// re-registering with --if-absent from the SAME (socket_path, session_id)
// tuple already on the row is a no-op upsert, not a conflict — a launch
// wrapper re-running against its own already-seeded identity must succeed.
func TestRegisterIfAbsentSameTupleIdempotent(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	t.Setenv("TMUX", "/tmp/tmux-0/proj-muster,1,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("MUSTER_ALIAS", "")
	socketPath := "/tmp/tmux-0/proj-muster"
	// Seeded to match what cmdRegister's typed-name path will produce from
	// "seed2" below, so this is genuinely the same row, not a fresh one.
	registerViaDaemon(t, "", "testdev-seed2", socketPath, "$SAME")

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
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	t.Setenv("TMUX", "/tmp/tmux-0/proj-muster,1,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("MUSTER_ALIAS", "")
	tmuxCaptureStub(t, "$FRESH", "freshalias")

	var buf bytes.Buffer
	if err := cmdRegister([]string{"freshalias", "--if-absent"}, &buf); err != nil {
		t.Fatalf("--if-absent on a fresh alias should succeed, got %v", err)
	}
	agents := listAgentsForTest(t, "")
	if len(agents) != 1 || agents[0].Alias != "testdev-freshalias" {
		t.Fatalf("expected a fresh seeded row to be created, got %+v", agents)
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
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	t.Setenv("TMUX", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("MUSTER_ALIAS", "")
	seed := func(op string, args map[string]any) {
		t.Helper()
		if _, err := callData(op, args); err != nil {
			t.Fatal(err)
		}
	}
	// Pre-existing row is seeded to match what cmdRegister's typed-name path
	// will produce from "backend" below, so the revive lands on this row.
	seed("register_agent", map[string]any{"alias": "testdev-backend", "socket_path": "/s", "session_id": "$1"})
	seed("register_agent", map[string]any{"alias": "sender", "socket_path": "/s", "session_id": "$2"})
	seed("send_message", map[string]any{"from": "sender", "to_kind": "agent", "to_target": "testdev-backend", "subject": "hi", "body": "b"})
	seed("deregister_agent", map[string]any{"alias": "testdev-backend"})

	var buf bytes.Buffer
	if err := cmdRegister([]string{"backend"}, &buf); err != nil {
		t.Fatalf("register: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "revived") || !strings.Contains(out, "1 unread") {
		t.Fatalf("register output missing revival/unread notice:\n%s", out)
	}
}
