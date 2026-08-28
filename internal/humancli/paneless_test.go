package humancli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// pinAncestryWalkAway neuters tmuxenv.CaptureFromAncestry for the duration of
// a test by making its two seams find nothing: no ancestor PIDs, no socket
// directory to glob. Without this, a paneless test (empty $TMUX/$TMUX_PANE)
// leaves hookCapture() falling through to the REAL ancestry walk — and on a
// dev machine, `go test` itself commonly runs inside a real tmux pane (e.g. a
// coding-agent session on the muster bus), so the walk can find that real
// pane and turn an intended paneless SessionStart into a pane-anchored one.
func pinAncestryWalkAway(t *testing.T) {
	t.Helper()
	prevAnc, prevDir := tmuxenv.AncestorPIDs, tmuxenv.SocketDir
	tmuxenv.AncestorPIDs = func() []int { return nil }
	tmuxenv.SocketDir = func() string { return t.TempDir() } // fresh empty dir: Glob matches nothing
	t.Cleanup(func() { tmuxenv.AncestorPIDs, tmuxenv.SocketDir = prevAnc, prevDir })
}

// panelessEnv pins the process into a paneless shape: no tmux, a known
// harness session UUID, and a working directory whose basename is the
// expected fallback alias. CLAUDE_CODE_SESSION_ID must be pinned in every
// paneless test — on a dev machine `go test` itself runs inside a Claude
// session whose UUID would otherwise leak in. Also pins the ancestry-walk
// seams away (see pinAncestryWalkAway) so hookCapture()'s fallback can't
// resolve this test process's real tmux pane.
func panelessEnv(t *testing.T, uuid, dirName string) string {
	t.Helper()
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("MUSTER_ALIAS", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", uuid)
	// The harness-neutral spelling must be pinned too: a dev machine running
	// `go test` inside a pi session leaks AGENT_SESSION_ID, and harnessenv
	// falls back to it whenever the Claude spelling is empty.
	t.Setenv("AGENT_SESSION_ID", "")
	pinAncestryWalkAway(t)
	dir := filepath.Join(t.TempDir(), dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return dir
}

func TestRegisterPanelessFallsBackToCwdAlias(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	panelessEnv(t, "hs-reg-1", "wt-alpha")

	var buf bytes.Buffer
	if err := cmdRegister(nil, &buf); err != nil {
		t.Fatalf("paneless register must succeed on the cwd fallback, got %v", err)
	}
	// The paneless base is seeded before allocation, like every other mint
	// site — but the confirmation is a human surface, so this machine's own
	// prefix ("testdev-") is stripped from what's PRINTED. The stored row
	// (checked below) keeps the full seeded alias.
	if !strings.Contains(buf.String(), "registered wt-alpha (paneless") {
		t.Fatalf("output must name the display-stripped alias and the paneless shape, got %q", buf.String())
	}
	agents := listAgentsForTest(t, "")
	if len(agents) != 1 {
		t.Fatalf("expected one registration, got %+v", agents)
	}
	a := agents[0]
	if a.Alias != "testdev-wt-alpha" || a.SocketPath != "" || a.PaneID != "" || a.SessionID != "hs-reg-1" {
		t.Fatalf("paneless row shape wrong: %+v", a)
	}
}

func TestRegisterPanelessWithoutAnyIdentityStillErrors(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("MUSTER_ALIAS", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("AGENT_SESSION_ID", "") // see panelessEnv: pi leaks this into dev runs
	t.Chdir("/")                     // basename "/" derives no alias
	var buf bytes.Buffer
	err := cmdRegister(nil, &buf)
	if err == nil || !strings.Contains(err.Error(), "cannot determine alias") {
		t.Fatalf("expected the no-identity error, got %v", err)
	}
}

func TestHookSessionStartPanelessRegistersFromPayload(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	panelessEnv(t, "", "env-dir") // env UUID empty: the payload must carry identity

	payload := `{"session_id":"hs-hook-1","cwd":"/tmp/somewhere/payload-dir"}`
	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart", "codex"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatalf("SessionStart: %v", err)
	}
	agents := listAgentsForTest(t, "")
	if len(agents) != 1 {
		t.Fatalf("expected one paneless registration, got %+v", agents)
	}
	a := agents[0]
	// The hook's own paneless allocation seeds the cwd-basename base, like
	// every other mint site.
	if a.Alias != "testdev-payload-dir" || a.SessionID != "hs-hook-1" || a.SocketPath != "" || a.ModelType != "codex" {
		t.Fatalf("paneless hook registration shape wrong: %+v", a)
	}
}

// TestHookSessionStartPanelessSeedRegisterCarriesTranscriptPath covers
// Finding 4: the very first paneless SEED registration (spec §3.3 requires
// every seed register to pass transcript_path) must store the hook payload's
// transcript_path, not just the revive/reclaim paths that already did. A
// daemon-hosted conversation that later runs /login has no live tmux tuple to
// fall back on, so a row that never got its transcript stamped at seed time
// can never be matched again by conversationRows, and every later get_inbox
// for it comes back an unowned peek.
func TestHookSessionStartPanelessSeedRegisterCarriesTranscriptPath(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	panelessEnv(t, "", "tp-dir")

	payload := `{"session_id":"hs-tp-1","cwd":"/tmp/somewhere/tp-dir","transcript_path":"/home/op/.claude/projects/x/tp-1.jsonl"}`
	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart", "claude"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatalf("SessionStart: %v", err)
	}
	agents := listAgentsForTest(t, "")
	if len(agents) != 1 {
		t.Fatalf("expected one paneless registration, got %+v", agents)
	}
	if got := agents[0].TranscriptPath; got != "/home/op/.claude/projects/x/tp-1.jsonl" {
		t.Fatalf("TranscriptPath = %q, want the payload's transcript_path stamped at seed time", got)
	}
}

func TestHookSessionStartPanelessSuffixesPastLiveTmuxOwner(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	// Seeded to match what the paneless hook will seed the cwd basename
	// "owner-dir" to below, so the collision is genuine.
	if _, err := callData("register_agent", map[string]any{
		"alias": "testdev-owner-dir", "socket_path": "/tmp/sockOwn", "session_id": "$1", "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}
	panelessEnv(t, "hs-thief", "owner-dir")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{pane_id}": "%1"}) // the tmux owner's pane is alive
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(`{"session_id":"hs-thief","cwd":"`+"/x/owner-dir"+`"}`), &buf); err != nil {
		t.Fatalf("SessionStart: %v", err)
	}
	ag, found, _ := hookGetAgent("testdev-owner-dir")
	if !found || ag.SessionID != "$1" || ag.SocketPath != "/tmp/sockOwn" {
		t.Fatalf("a live tmux owner must keep the alias, got %+v found=%v", ag, found)
	}
	suf, found, _ := hookGetAgent("testdev-owner-dir-2")
	if !found || suf.SessionID != "hs-thief" || suf.SocketPath != "" {
		t.Fatalf("the paneless session must allocate the next suffix, got %+v found=%v", suf, found)
	}
}

func TestHookSessionStartPanelessAllocatesUniqueAliases(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	panelessEnv(t, "", "shared-dir")
	start := func(uuid string) {
		t.Helper()
		var buf bytes.Buffer
		if err := cmdHook([]string{"SessionStart"}, strings.NewReader(`{"session_id":"`+uuid+`","cwd":"/x/shared-dir"}`), &buf); err != nil {
			t.Fatalf("SessionStart(%s): %v", uuid, err)
		}
	}
	start("hs-one")
	start("hs-two")   // same directory, different session: must NOT steal
	start("hs-three") // third session: next suffix again
	start("hs-two")   // resume: must refresh its own alias, not allocate a fourth

	// The seeded base is what the whole allocated family carries the prefix
	// on: testdev-shared-dir, testdev-shared-dir-2, ...
	want := map[string]string{"testdev-shared-dir": "hs-one", "testdev-shared-dir-2": "hs-two", "testdev-shared-dir-3": "hs-three"}
	for alias, uuid := range want {
		ag, found, _ := hookGetAgent(alias)
		if !found || ag.SessionID != uuid || ag.SocketPath != "" || ag.Departed {
			t.Fatalf("%s: want live paneless row for %s, got %+v found=%v", alias, uuid, ag, found)
		}
	}
	if _, found, _ := hookGetAgent("testdev-shared-dir-4"); found {
		t.Fatal("a resumed session must reuse its alias, not allocate testdev-shared-dir-4")
	}
}

func TestHookSessionStartPanelessRevivesOwnTombstoneOnResume(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	panelessEnv(t, "", "revive-dir")
	payload := `{"session_id":"hs-rev","cwd":"/x/revive-dir"}`
	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}
	if ag, _, _ := hookGetAgent("testdev-revive-dir"); !ag.Departed {
		t.Fatalf("setup: expected tombstone after SessionEnd, got %+v", ag)
	}
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}
	ag, found, _ := hookGetAgent("testdev-revive-dir")
	if !found || ag.Departed || ag.SessionID != "hs-rev" {
		t.Fatalf("resume must revive the session's own tombstone, got %+v found=%v", ag, found)
	}
	if _, found, _ := hookGetAgent("testdev-revive-dir-2"); found {
		t.Fatal("revival must not allocate a suffix")
	}
}

func TestHookSessionEndPanelessReapsOnlyOwnAliases(t *testing.T) {
	startTestDaemon(t)
	reg := func(alias, socket, session string) {
		t.Helper()
		if _, err := callData("register_agent", map[string]any{
			"alias": alias, "socket_path": socket, "session_id": session,
		}); err != nil {
			t.Fatal(err)
		}
	}
	reg("mine-a", "", "hs-end-1")
	reg("mine-b", "", "hs-end-1")
	reg("other-paneless", "", "hs-other")
	reg("tmux-agent", "/s", "hs-end-1") // a tmux row whose session_id coincides: not paneless, not ours

	panelessEnv(t, "", "end-dir")
	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(`{"session_id":"hs-end-1"}`), &buf); err != nil {
		t.Fatalf("SessionEnd: %v", err)
	}
	departed := map[string]bool{}
	for _, a := range listAgentsForTest(t, "") {
		departed[a.Alias] = a.Departed
	}
	if !departed["mine-a"] || !departed["mine-b"] {
		t.Fatalf("both of the dying session's aliases must be tombstoned, got %+v", departed)
	}
	if departed["other-paneless"] || departed["tmux-agent"] {
		t.Fatalf("another session's rows must survive, got %+v", departed)
	}
}

// TestHookSessionEndPanelessSparesLiveOtherTuple is finding F2's core
// scenario: the resume-coexistence race. conversationRows keys purely on the
// harness session UUID with no tuple discrimination, so a dying paneless
// SessionEnd that enumerates them could tombstone a DIFFERENT alias's row
// that a concurrent resume has already reclaimed onto a brand-new, live tmux
// tuple sharing the same UUID. The belt-and-suspenders check
// (tmuxenv.IsSessionAlive, mirroring hookSessionStartResume's own collision
// predicate) must spare that provably-alive row while still tombstoning the
// dying tuple's own (paneless, unqueryable) row. Contrast
// TestLaunchHandshakeLifecycle, which pins the mirror case: a row whose tmux
// tuple is genuinely gone must still be tombstoned as before.
func TestHookSessionEndPanelessSparesLiveOtherTuple(t *testing.T) {
	startTestDaemon(t)
	seed := func(args map[string]any) {
		t.Helper()
		if _, err := callData("register_agent", args); err != nil {
			t.Fatal(err)
		}
	}
	// The dying side: a paneless row (no tmux tuple to probe at all).
	seed(map[string]any{
		"alias": "race-old", "socket_path": "", "session_id": "uuid-race",
		"harness_session_id": "uuid-race",
	})
	// The other side: already reclaimed onto a brand-new, live tmux tuple,
	// sharing the SAME harness UUID (the reclaim race's live half).
	seed(map[string]any{
		"alias": "race-new", "socket_path": "/tmp/sockRace", "session_id": "$9",
		"session_created": 777, "harness_session_id": "uuid-race",
	})

	panelessEnv(t, "uuid-race", "race-dir")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		for _, a := range args {
			if a == "/tmp/sockRace" {
				return "777", nil // IsSessionAlive: matches the stored created time -> alive
			}
		}
		return "", fmt.Errorf("gone")
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(`{"session_id":"uuid-race"}`), &buf); err != nil {
		t.Fatal(err)
	}
	departed := map[string]bool{}
	for _, a := range listAgentsForTest(t, "") {
		departed[a.Alias] = a.Departed
	}
	if !departed["race-old"] {
		t.Fatalf("the dying (paneless) tuple's row must still be tombstoned, got %+v", departed)
	}
	if departed["race-new"] {
		t.Fatalf("a provably-live other-tuple row sharing the UUID must survive the race, got %+v", departed)
	}
}

func TestHookStopPanelessBlocksOnUnread(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "wt-mail", "socket_path": "", "session_id": "hs-stop-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "sender", "socket_path": "/p", "session_id": "$2",
	}); err != nil {
		t.Fatal(err)
	}

	panelessEnv(t, "", "stop-dir")

	// No mail yet: the hook must stay silent.
	var quiet bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{"session_id":"hs-stop-1"}`), &quiet); err != nil {
		t.Fatal(err)
	}
	if quiet.Len() != 0 {
		t.Fatalf("expected silence with no unread mail, got %q", quiet.String())
	}

	if _, err := callData("send_message", map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "wt-mail", "subject": "s", "body": "b",
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{"session_id":"hs-stop-1"}`), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"decision":"block"`) || !strings.Contains(buf.String(), "alias 'wt-mail'") {
		t.Fatalf("expected a block decision addressing wt-mail, got %q", buf.String())
	}

	// An unregistered paneless session (unknown UUID) must stay silent.
	var other bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{"session_id":"hs-unknown"}`), &other); err != nil {
		t.Fatal(err)
	}
	if other.Len() != 0 {
		t.Fatalf("an unregistered session must print nothing, got %q", other.String())
	}
}

func TestGCSparesLivePanelessRows(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "paneless-live", "socket_path": "", "session_id": "hs-gc-1",
	}); err != nil {
		t.Fatal(err)
	}
	prev := tmuxenv.Run
	tmuxenv.Run = func(_ ...string) (string, error) { return "", fmt.Errorf("dead") } // every tmux probe answers dead
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdGC(nil, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "gc: tombstoned 0 agent(s)") {
		t.Fatalf("gc must not judge a paneless row by tmux liveness, got %q", buf.String())
	}

	buf.Reset()
	if err := cmdGC([]string{"--purge-agents"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "gc: purged 0 agent(s)") {
		t.Fatalf("--purge-agents must spare a live paneless row, got %q", buf.String())
	}

	// Once departed, the row is purgeable exactly like any other tombstone.
	if err := cmdDeregister([]string{"paneless-live"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := cmdGC([]string{"--purge-agents"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "gc: purged 1 agent(s)") {
		t.Fatalf("a departed paneless row must purge, got %q", buf.String())
	}
}

// TestLaunchHandshakeLifecycle drives the pane-side launch handshake end to
// end: the pane registers the tmux session name with --harness-session (the
// UUID it will hand to `claude --session-id`), then the SESSION's hooks —
// which see no tmux — resolve that row by UUID: SessionStart leaves the live
// row alone (no cwd-derived duplicate), Stop drains mail via the row's tmux
// tuple with the label leading the reason, SessionEnd tombstones it.
func TestLaunchHandshakeLifecycle(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")

	// Pane side: tmux env present, handshake registration.
	t.Setenv("TMUX", "/tmp/sockHS,1,0")
	t.Setenv("TMUX_PANE", "%3")
	t.Setenv("MUSTER_ALIAS", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$7", "#{session_name}": "bh-workspace-4", "#{session_created}": "1784000123",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })
	var buf bytes.Buffer
	if err := cmdRegister([]string{"--harness-session", "hs-shake"}, &buf); err != nil {
		t.Fatalf("handshake register: %v", err)
	}
	// The derived session name is seeded, like every other mint site.
	ag, found, _ := hookGetAgent("testdev-bh-workspace-4")
	if !found {
		t.Fatal("handshake row missing")
	}

	// Session side: no tmux at all.
	panelessEnv(t, "", "some-worktree-dir")
	payload := `{"session_id":"hs-shake","cwd":"/x/some-worktree-dir"}`
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}
	// The would-be cwd alias is also seeded — check the form the hook would
	// actually mint if the handshake early-return ever broke, not the
	// unseeded bare name (which would never appear either way).
	if _, found, _ := hookGetAgent("testdev-some-worktree-dir"); found {
		t.Fatal("SessionStart must not allocate a cwd alias when the handshake row exists")
	}

	if _, err := callData("register_agent", map[string]any{
		"alias": "sender", "socket_path": "/p", "session_id": "$9",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "testdev-bh-workspace-4", "subject": "s", "body": "b",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("set_label", map[string]any{
		"socket_path": "/tmp/sockHS", "session_id": "$7", "session_created": 1784000123,
		"label": "debug alarms", "label_manual": true,
	}); err != nil {
		t.Fatal(err)
	}
	var stop bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{"session_id":"hs-shake"}`), &stop); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stop.String(), "alias 'testdev-bh-workspace-4'") || !strings.Contains(stop.String(), "You are 'debug alarms'") {
		t.Fatalf("Stop must address the handshake alias and lead with its label, got %q", stop.String())
	}

	// By the time the daemon-hosted session's own SessionEnd fires, the
	// launching pane has actually closed too (a realistic teardown order,
	// and the ordinary case finding F2's belt-and-suspenders IsSessionAlive
	// check must still let through) — every tmux probe now answers "gone".
	// Contrast TestHookSessionEndPanelessSparesLiveOtherTuple, which pins the
	// OTHER half: a tuple that IS still alive must survive.
	tmuxenv.Run = func(_ ...string) (string, error) { return "", fmt.Errorf("pane closed") }
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}
	ag, found, _ = hookGetAgent("testdev-bh-workspace-4")
	if !found || !ag.Departed {
		t.Fatalf("SessionEnd must tombstone the handshake row, got %+v found=%v", ag, found)
	}
}

// TestHookSessionStartPanelessRevivesSuccessorNotSupersededSeed is finding
// F1's core scenario: a harness session UUID owns TWO tombstoned rows —
// "aaa-old" (become-retired: superseded_by="zzz-new") and "zzz-new" (the
// successor, also since departed on its own). ORDER BY alias in ListAgents
// puts "aaa-old" first, so the pre-fix code (owned[0], no SupersededBy
// check) would revive the RETIRED SEED under its old alias — resurrecting an
// identity that `muster become` already carried forward. The fix
// (firstUnsuperseded) must skip it and revive "zzz-new" instead, leaving
// "aaa-old" departed forever.
func TestHookSessionStartPanelessRevivesSuccessorNotSupersededSeed(t *testing.T) {
	startTestDaemon(t)
	panelessEnv(t, "hs-f1", "f1-dir")

	if _, err := callData("register_agent", map[string]any{
		"alias": "aaa-old", "session_id": "hs-f1", "harness_session_id": "hs-f1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("become", map[string]any{"from": "aaa-old", "to": "zzz-new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("deregister_agent", map[string]any{"alias": "zzz-new"}); err != nil {
		t.Fatal(err)
	}

	// Setup sanity: both rows departed, and the supersession link points the
	// direction the fix must respect.
	old, ok, _ := hookGetAgent("aaa-old")
	if !ok || !old.Departed || old.SupersededBy != "zzz-new" {
		t.Fatalf("setup: aaa-old = %+v (ok=%v)", old, ok)
	}
	successor, ok, _ := hookGetAgent("zzz-new")
	if !ok || !successor.Departed || successor.SupersededBy != "" {
		t.Fatalf("setup: zzz-new = %+v (ok=%v)", successor, ok)
	}

	payload := `{"session_id":"hs-f1","cwd":"/x/f1-dir"}`
	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}

	revived, ok, _ := hookGetAgent("zzz-new")
	if !ok || revived.Departed || revived.SessionID != "hs-f1" {
		t.Fatalf("revive must pick the successor zzz-new, got %+v (ok=%v)", revived, ok)
	}
	stillOld, ok, _ := hookGetAgent("aaa-old")
	if !ok || !stillOld.Departed {
		t.Fatalf("the superseded seed must never be resurrected, got %+v (ok=%v)", stillOld, ok)
	}
}
