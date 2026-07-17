package station

import (
	"encoding/json"
	"path/filepath"
	"strings"
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

// fullAgentTuple decodes get_agent's FULL store.Agent payload — unlike
// agentTuple (registerStation's own narrow socket/session-only view), this
// exposes RegisteredAt/LastReadEntryID, the fields spec iteration-8's
// read-state-persistence tests need to assert against.
type fullAgentTuple struct {
	Found bool        `json:"found"`
	Agent store.Agent `json:"agent"`
}

func getFullAgent(t *testing.T, caller daemonCaller, alias string) fullAgentTuple {
	t.Helper()
	raw, err := caller.Call("get_agent", map[string]any{"alias": alias})
	if err != nil {
		t.Fatal(err)
	}
	var res fullAgentTuple
	if err := json.Unmarshal(raw, &res); err != nil {
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
// triggering fail-over — and (spec iteration-8) the takeover's own
// register_agent call is a plain upsert (registerStation deliberately omits
// if_absent for a dead-tuple takeover — see its own doc comment), so it must
// preserve whatever read watermark the dead row already carried rather than
// resetting it to 0 and resurrecting already-acknowledged mail as unread
// under the new session.
func TestRegisterStationTakesOverDeadCollision(t *testing.T) {
	startStationTestDaemon(t)
	caller := daemonCaller{}
	registerDirect(t, caller, "station", "/dead", "$DEAD")
	stubTmuxAlive(t, "", "") // nothing is alive, including /dead,$DEAD

	// The dead row already has mail acknowledged against it (get_inbox --
	// the same op focusConversation/openForYouThread fire -- calls MarkRead
	// as a side effect), so it carries a non-zero watermark BEFORE takeover.
	registerDirect(t, caller, "alpha-1", "/a", "$A")
	if _, err := caller.Call("send_message", map[string]any{"from": "alpha-1", "to_kind": "agent", "to_target": "station", "body": "hi", "intent": "fyi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := caller.Call("get_inbox", map[string]any{"alias": "station"}); err != nil {
		t.Fatal(err)
	}
	before := getFullAgent(t, caller, "station")
	if before.Agent.LastReadEntryID == 0 {
		t.Fatalf("setup: expected the dead row to already carry a non-zero read watermark, got %+v", before.Agent)
	}

	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", SessionName: "sess", PaneID: "%1"}
	got, err := registerStation(caller, "station", c)
	if err != nil {
		t.Fatal(err)
	}
	if got != "station" {
		t.Fatalf("alias = %q, want the dead alias taken over (%q)", got, "station")
	}
	mine := getFullAgent(t, caller, "station")
	if !mine.Found || mine.Agent.SocketPath != "/s" || mine.Agent.SessionID != "$1" {
		t.Fatalf("station's tuple was not taken over: %+v", mine)
	}
	if mine.Agent.LastReadEntryID != before.Agent.LastReadEntryID {
		t.Fatalf("dead-tuple takeover must preserve the read watermark, got %d, want %d", mine.Agent.LastReadEntryID, before.Agent.LastReadEntryID)
	}
}

// This section is spec iteration-8's own read-state-persistence test suite:
// station's exit path no longer deregisters at all (see Run's own doc
// comment) — the two deregisterIfStillOurs/deregisterOnce tests that used to
// live here covered CONDITIONAL deregistration, a code path that no longer
// exists, so they're replaced by tests asserting the actual spec iteration-8
// behavior: quitting leaves the row (and its read watermark) exactly in
// place, and the next launch's registerStation upserts onto that SAME row.

// TestRegisterMarkReadReRegisterPreservesWatermark is the direct,
// store/daemon-op-level version of the brief's own prescribed check:
// register → (mail arrives and gets) acknowledged (get_inbox, which calls
// store.Store.MarkRead as a side effect) → re-register the SAME alias/tuple
// (what a real relaunch in the same tmux pane does) → the read watermark
// must be EXACTLY what it was, never reset to 0 — RegisterAgent's ON
// CONFLICT clause simply never lists last_read_entry_id among the columns
// it overwrites (see agents.go).
func TestRegisterMarkReadReRegisterPreservesWatermark(t *testing.T) {
	startStationTestDaemon(t)
	caller := daemonCaller{}
	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", SessionName: "sess", PaneID: "%1"}

	if _, err := registerStation(caller, "station", c); err != nil {
		t.Fatal(err)
	}
	registerDirect(t, caller, "alpha-1", "/a", "$A")
	if _, err := caller.Call("send_message", map[string]any{"from": "alpha-1", "to_kind": "agent", "to_target": "station", "body": "hi", "intent": "fyi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := caller.Call("get_inbox", map[string]any{"alias": "station"}); err != nil {
		t.Fatal(err)
	}
	before := getFullAgent(t, caller, "station")
	if before.Agent.LastReadEntryID == 0 {
		t.Fatalf("setup: expected a non-zero read watermark after get_inbox, got %+v", before.Agent)
	}

	// Re-register the identical alias/tuple — what registerStation's
	// same-tuple upsert path does on a real relaunch in the same tmux pane.
	got, err := registerStation(caller, "station", c)
	if err != nil {
		t.Fatal(err)
	}
	if got != "station" {
		t.Fatalf("re-register alias = %q, want the SAME alias %q", got, "station")
	}
	after := getFullAgent(t, caller, "station")
	if after.Agent.LastReadEntryID != before.Agent.LastReadEntryID {
		t.Fatalf("re-register must preserve the read watermark, got %d, want %d", after.Agent.LastReadEntryID, before.Agent.LastReadEntryID)
	}
}

// TestReadStateSurvivesQuitAndRelaunch is spec iteration-8's model/
// integration-level read-state-persistence test, against the REAL daemon
// (the operator-diagnosed regression: "station quit → conditional
// deregister DELETES its agent row including last_read_entry_id → every
// restart resurrects previously-acknowledged mail as unread — 📬 2 came
// back"). It drives the ACTUAL acknowledge path — a real station Model's 'm'
// mail-jump key, which fires get_inbox for real, exactly like an operator's
// keypress — then simulates "quit" (Run's exit path no longer calls any
// deregister op at all — there is nothing left to invoke) and "relaunch" (a
// second registerStation call with the IDENTICAL tuple, exactly what a
// restart in the same tmux pane does), asserting the row is the SAME row
// throughout (registered_at unchanged — never deleted and recreated) and its
// read watermark survives intact.
//
// Note: this does NOT assert station's own in-session "📬 N for you" badge
// text specifically, because that count (operatorInboxCount/forYouRows) is
// deliberately scoped to THIS RUN's own ackedThreads map (see forYouRows'
// doc) — a fresh process legitimately starts that map empty and re-lists any
// still-outstanding thread, same as a human's mail client showing an email
// as unread again in a new session until it's replied to. The mechanism this
// test protects is the STORE's own persisted watermark (last_read_entry_id),
// which is what the pre-fix bug actually reset to 0 on every quit, and what
// every OTHER surface reading it (UnreadCount, SessionUnread — used by
// `muster agents`/`muster inbox` and any peer checking on station) depends
// on staying intact across a restart.
func TestReadStateSurvivesQuitAndRelaunch(t *testing.T) {
	startStationTestDaemon(t)
	caller := daemonCaller{}
	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", SessionName: "sess", PaneID: "%1"}

	if _, err := registerStation(caller, "station", c); err != nil {
		t.Fatal(err)
	}
	registerDirect(t, caller, "alpha-1", "/a", "$A")
	if _, err := caller.Call("send_message", map[string]any{"from": "alpha-1", "to_kind": "agent", "to_target": "station", "body": "please review", "intent": "action-requested"}); err != nil {
		t.Fatal(err)
	}

	// Acknowledge the mail through a REAL station Model driven against the
	// real daemon: 'm' opens the mailbox page, Enter on the unread row fires
	// get_inbox for real (spec §5-LOCK's open-to-acknowledge path) — the
	// exact op an operator's keypress triggers.
	m := NewModel(caller, Options{Alias: "station"})
	next, _ := m.Update(fetchAgentsCmd(caller)())
	m = mustModel(t, next)
	next, cmd := m.Update(fetchThreadsCmd(caller)())
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.operatorInboxCount() == 0 {
		t.Fatalf("setup: expected unread mail addressed to station before acknowledging")
	}
	next, cmd = m.Update(keyMsg("m"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenMailbox {
		t.Fatalf("setup: expected 'm' to open the mailbox, got screen=%v", m.screen)
	}
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenRead {
		t.Fatalf("setup: expected Enter on the mailbox's unread row to open it, got screen=%v", m.screen)
	}

	before := getFullAgent(t, caller, "station")
	if before.Agent.LastReadEntryID == 0 {
		t.Fatalf("setup: expected a non-zero read watermark after acknowledging, got %+v", before.Agent)
	}

	// "Quit": Run's exit path calls no deregister op at all any more (see
	// its own doc comment) — there is nothing to invoke here; the row must
	// simply still be exactly as it was.
	stillThere := getFullAgent(t, caller, "station")
	if !stillThere.Found {
		t.Fatalf("a quit must not delete the station row")
	}

	// "Relaunch" in the SAME tmux pane — the identical (socket_path,
	// session_id) tuple — re-registers onto the SAME row.
	got, err := registerStation(caller, "station", c)
	if err != nil {
		t.Fatal(err)
	}
	if got != "station" {
		t.Fatalf("relaunch alias = %q, want the SAME alias %q (not a fail-over)", got, "station")
	}
	after := getFullAgent(t, caller, "station")
	if !after.Found {
		t.Fatalf("relaunch must not have lost the row")
	}
	if after.Agent.RegisteredAt != before.Agent.RegisteredAt {
		t.Fatalf("relaunch must upsert onto the SAME row (registered_at unchanged), got %d, want %d", after.Agent.RegisteredAt, before.Agent.RegisteredAt)
	}
	if after.Agent.LastReadEntryID != before.Agent.LastReadEntryID {
		t.Fatalf("relaunch must preserve the read watermark, got %d, want %d (the pre-fix regression: '📬 2 came back')", after.Agent.LastReadEntryID, before.Agent.LastReadEntryID)
	}
}

// TestDepartedAgentSurvivesUnderTheBar is task 2's own fixture (spec §5-LOCK
// decision A: "departed history lives INSIDE its origin project... below a
// plain divider bar, dimmed, marked ✗" + "station: under-the-bar uses
// surviving rows"): a cleanly deregistered agent's row must now SURVIVE (the
// deregistration tombstone) and keep showing up in the AGENTS list's
// below-the-bar section — dimmed, ✗, its real alias, thread count + age —
// exactly like a tmux-dead-but-still-registered agent already did. Before the
// tombstone fix, deregister hard-deleted the row and this exact history is
// what the ghost-site/bettor-help-workspace-4 incident (spec §5-LOCK's own
// motivating case) lost.
func TestDepartedAgentSurvivesUnderTheBar(t *testing.T) {
	startStationTestDaemon(t)
	caller := daemonCaller{}
	registerDirect(t, caller, "alpha-1", "/a", "$A")
	registerDirect(t, caller, "someone", "/x", "$X")
	if _, err := caller.Call("send_message", map[string]any{
		"from": "alpha-1", "to_kind": "agent", "to_target": "someone", "body": "hi",
	}); err != nil {
		t.Fatal(err)
	}

	// The clean exit: deregister_agent, exactly what SessionEnd/`muster
	// deregister` drive in production.
	if _, err := caller.Call("deregister_agent", map[string]any{"alias": "alpha-1"}); err != nil {
		t.Fatal(err)
	}

	// fetchAgents is the REAL poll.go path station's tick issues every
	// second — proving the row still comes back from list_agents, and that
	// its now-defunct tmux tuple ("/a", "$A") correctly reads as dead.
	rows, err := fetchAgents(caller)
	if err != nil {
		t.Fatal(err)
	}
	var sawAlpha1, live bool
	for _, r := range rows {
		if r.Alias == "alpha-1" {
			sawAlpha1 = true
			live = r.Live
		}
	}
	if !sawAlpha1 {
		t.Fatalf("departed agent must still be listed by list_agents/fetchAgents, got %+v", rows)
	}
	if live {
		t.Fatalf("departed agent's defunct tmux tuple must read as dead (Live=false), got Live=true")
	}

	m := NewModel(caller, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: rows})
	m = mustModel(t, next)
	next, cmd := m.Update(fetchThreadsCmd(caller)())
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenProject {
		t.Fatalf("setup: expected the single-project auto-skip to screenProject, got %v", m.screen)
	}

	view := m.View()
	if !strings.Contains(view, "✗") {
		t.Fatalf("expected the ✗ marker on the departed agent:\n%s", view)
	}
	if !strings.Contains(view, "alpha-1") {
		t.Fatalf("expected the departed agent's real alias listed:\n%s", view)
	}
	if strings.Contains(strings.ToLower(view), "departed") {
		t.Fatalf("view must never contain the word \"departed\" (spec §5-LOCK decision A):\n%s", view)
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
