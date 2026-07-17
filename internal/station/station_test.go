package station

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/mustertest"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/store"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// startStationTestDaemon boots a real in-process daemon on a temp socket —
// station's integration smoke test for Run's registration path (spec §5
// identity: collision fail-over, conditional deregister) exercises the real
// register_agent/get_agent/deregister_agent ops rather than a fake Caller.
func startStationTestDaemon(t *testing.T) {
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
}

// stubTmuxAlive replaces tmuxenv.Run so has-session reports "alive" only for
// the given (socket, sessionID) tuple and "dead" (an error) for everything
// else — station's collision logic shells out to real tmux via
// tmuxenv.IsSessionAlive, so tests must control that answer directly rather
// than depend on whatever tmux session (if any) the test happens to run
// inside.
func stubTmuxAlive(t *testing.T, aliveSocket, aliveSessionID string) {
	t.Helper()
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		// has-session invocation shape: "-S", socket, "has-session", "-t", sessionID
		if len(args) >= 5 && args[2] == "has-session" {
			if args[1] == aliveSocket && args[4] == aliveSessionID {
				return "", nil
			}
			return "", errNoSuchSession
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })
}

var errNoSuchSession = &stationTestError{"can't find session"}

type stationTestError struct{ msg string }

func (e *stationTestError) Error() string { return e.msg }

func registerDirect(t *testing.T, caller daemonCaller, alias, socketPath, sessionID string) {
	t.Helper()
	if _, err := caller.Call("register_agent", map[string]any{
		"alias": alias, "role": "operator", "model_type": "station",
		"socket_path": socketPath, "session_id": sessionID,
	}); err != nil {
		t.Fatal(err)
	}
}

func getAgentTupleForTest(t *testing.T, caller daemonCaller, alias string) agentTuple {
	t.Helper()
	res, err := getAgentTuple(caller, alias)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestRegisterStationFreshAliasRegisters covers the no-collision path: a
// brand-new alias registers directly with role operator / model station.
func TestRegisterStationFreshAliasRegisters(t *testing.T) {
	startStationTestDaemon(t)
	stubTmuxAlive(t, "", "") // nothing is alive; irrelevant here (no existing record)
	caller := daemonCaller{}
	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", SessionName: "sess", PaneID: "%1", Project: "muster"}

	got, err := registerStation(caller, "station", c)
	if err != nil {
		t.Fatal(err)
	}
	if got != "station" {
		t.Fatalf("alias = %q, want %q", got, "station")
	}
	raw, err := caller.Call("get_agent", map[string]any{"alias": "station"})
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Found bool        `json:"found"`
		Agent store.Agent `json:"agent"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Found || res.Agent.Role != "operator" || res.Agent.ModelType != "station" {
		t.Fatalf("expected a registered operator/station agent, got %+v", res)
	}
}

// TestRegisterStationFailsOverOnLiveCollision covers spec §5: a LIVE
// same-alias record with a DIFFERENT session tuple forces fail-over to
// "station-2" rather than stealing the alias.
func TestRegisterStationFailsOverOnLiveCollision(t *testing.T) {
	startStationTestDaemon(t)
	caller := daemonCaller{}
	registerDirect(t, caller, "station", "/other", "$OTHER")
	stubTmuxAlive(t, "/other", "$OTHER") // the existing "station" record is alive

	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", SessionName: "sess", PaneID: "%1"}
	got, err := registerStation(caller, "station", c)
	if err != nil {
		t.Fatal(err)
	}
	if got != "station-2" {
		t.Fatalf("alias = %q, want fail-over to %q", got, "station-2")
	}

	// The original "station" record must be untouched.
	orig := getAgentTupleForTest(t, caller, "station")
	if !orig.Found || orig.Agent.SocketPath != "/other" || orig.Agent.SessionID != "$OTHER" {
		t.Fatalf("original station record was overwritten: %+v", orig)
	}
	mine := getAgentTupleForTest(t, caller, "station-2")
	if !mine.Found || mine.Agent.SocketPath != "/s" || mine.Agent.SessionID != "$1" {
		t.Fatalf("station-2 does not carry our tuple: %+v", mine)
	}
}

// TestRegisterStationTakesOverDeadCollision covers spec §5: a same-alias
// record whose tmux session is no longer alive is taken over rather than
// triggering fail-over.
func TestRegisterStationTakesOverDeadCollision(t *testing.T) {
	startStationTestDaemon(t)
	caller := daemonCaller{}
	registerDirect(t, caller, "station", "/dead", "$DEAD")
	stubTmuxAlive(t, "", "") // nothing is alive, including /dead,$DEAD

	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", SessionName: "sess", PaneID: "%1"}
	got, err := registerStation(caller, "station", c)
	if err != nil {
		t.Fatal(err)
	}
	if got != "station" {
		t.Fatalf("alias = %q, want the dead alias taken over (%q)", got, "station")
	}
	mine := getAgentTupleForTest(t, caller, "station")
	if !mine.Found || mine.Agent.SocketPath != "/s" || mine.Agent.SessionID != "$1" {
		t.Fatalf("station's tuple was not taken over: %+v", mine)
	}
}

// TestDeregisterIfStillOursIsConditional covers spec §5: deregistration
// re-fetches the record and only removes it if the session tuple still
// matches — a second station that took the alias over after this one's
// "crash" must never be deleted by the first one's (delayed) exit.
func TestDeregisterIfStillOursIsConditional(t *testing.T) {
	startStationTestDaemon(t)
	caller := daemonCaller{}
	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1"}

	registerDirect(t, caller, "station", c.SocketPath, c.SessionID)
	deregisterIfStillOurs(caller, "station", c)
	if res := getAgentTupleForTest(t, caller, "station"); res.Found {
		t.Fatalf("clean exit with a matching tuple must deregister, but found: %+v", res)
	}

	// Simulate a second station taking the alias over with a different
	// tuple, then the FIRST station's (delayed) exit path running.
	other := tmuxenv.Capture{SocketPath: "/other", SessionID: "$OTHER"}
	registerDirect(t, caller, "station", other.SocketPath, other.SessionID)
	deregisterIfStillOurs(caller, "station", c) // c is the FIRST station's stale tuple
	res := getAgentTupleForTest(t, caller, "station")
	if !res.Found || res.Agent.SocketPath != "/other" || res.Agent.SessionID != "$OTHER" {
		t.Fatalf("a re-registered alias with a different tuple must survive the stale station's exit, got %+v", res)
	}
}

// TestDeregisterOnceFiresExactlyOnce covers Run's lifecycle hardening: the
// sync.Once-wrapped deregister a clean quit, a signal, and a recovered panic
// might all race to call must only ever touch the record ONCE — a second
// call (however it's triggered) must never delete a DIFFERENT station's
// meanwhile-registered record just because it happens to fire again.
func TestDeregisterOnceFiresExactlyOnce(t *testing.T) {
	startStationTestDaemon(t)
	caller := daemonCaller{}
	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1"}
	registerDirect(t, caller, "station", c.SocketPath, c.SessionID)

	dereg := deregisterOnce(caller, "station", c)
	dereg()
	if res := getAgentTupleForTest(t, caller, "station"); res.Found {
		t.Fatalf("first call must remove the record, got %+v", res)
	}

	other := tmuxenv.Capture{SocketPath: "/other", SessionID: "$OTHER"}
	registerDirect(t, caller, "station", other.SocketPath, other.SessionID)
	dereg() // e.g. a signal arriving right after `q` already fired the same func
	res := getAgentTupleForTest(t, caller, "station")
	if !res.Found || res.Agent.SocketPath != "/other" || res.Agent.SessionID != "$OTHER" {
		t.Fatalf("a second call (post sync.Once) must not touch a new owner, got %+v", res)
	}
}

// TestRegisterStationIfAbsentRaceFailsOverToNextSuffix covers the T7-deferred
// CAS race directly: two registerStation calls racing onto the SAME fresh
// alias concurrently must never both believe they won — the loser must fail
// over to "station-2" via the if_absent conflict path rather than either
// silently overwriting the other or erroring out.
func TestRegisterStationIfAbsentRaceFailsOverToNextSuffix(t *testing.T) {
	startStationTestDaemon(t)
	stubTmuxAlive(t, "", "") // nothing is alive; irrelevant (both starts see "not found")
	caller := daemonCaller{}

	c1 := tmuxenv.Capture{SocketPath: "/s1", SessionID: "$1", SessionName: "sess1", PaneID: "%1"}
	c2 := tmuxenv.Capture{SocketPath: "/s2", SessionID: "$2", SessionName: "sess2", PaneID: "%2"}

	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); results[0], errs[0] = registerStation(caller, "station", c1) }()
	go func() { defer wg.Done(); results[1], errs[1] = registerStation(caller, "station", c2) }()
	wg.Wait()

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("registerStation errors: %v, %v", errs[0], errs[1])
	}
	if results[0] == results[1] {
		t.Fatalf("both racers landed on the same alias %q — the race was not resolved", results[0])
	}
	got := map[string]bool{results[0]: true, results[1]: true}
	if !got["station"] || !got["station-2"] {
		t.Fatalf("expected exactly {station, station-2}, got %v", results)
	}
}
