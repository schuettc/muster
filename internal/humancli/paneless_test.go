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

// panelessEnv pins the process into a paneless shape: no tmux, a known
// harness session UUID, and a working directory whose basename is the
// expected fallback alias. CLAUDE_CODE_SESSION_ID must be pinned in every
// paneless test — on a dev machine `go test` itself runs inside a Claude
// session whose UUID would otherwise leak in.
func panelessEnv(t *testing.T, uuid, dirName string) string {
	t.Helper()
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("MUSTER_ALIAS", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", uuid)
	dir := filepath.Join(t.TempDir(), dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return dir
}

func TestRegisterPanelessFallsBackToCwdAlias(t *testing.T) {
	startTestDaemon(t)
	panelessEnv(t, "hs-reg-1", "wt-alpha")

	var buf bytes.Buffer
	if err := cmdRegister(nil, &buf); err != nil {
		t.Fatalf("paneless register must succeed on the cwd fallback, got %v", err)
	}
	if !strings.Contains(buf.String(), "registered wt-alpha (paneless") {
		t.Fatalf("output must name the alias and the paneless shape, got %q", buf.String())
	}
	agents := listAgentsForTest(t, "")
	if len(agents) != 1 {
		t.Fatalf("expected one registration, got %+v", agents)
	}
	a := agents[0]
	if a.Alias != "wt-alpha" || a.SocketPath != "" || a.PaneID != "" || a.SessionID != "hs-reg-1" {
		t.Fatalf("paneless row shape wrong: %+v", a)
	}
}

func TestRegisterPanelessWithoutAnyIdentityStillErrors(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("MUSTER_ALIAS", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Chdir("/") // basename "/" derives no alias
	var buf bytes.Buffer
	err := cmdRegister(nil, &buf)
	if err == nil || !strings.Contains(err.Error(), "cannot determine alias") {
		t.Fatalf("expected the no-identity error, got %v", err)
	}
}

func TestHookSessionStartPanelessRegistersFromPayload(t *testing.T) {
	startTestDaemon(t)
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
	if a.Alias != "payload-dir" || a.SessionID != "hs-hook-1" || a.SocketPath != "" || a.ModelType != "codex" {
		t.Fatalf("paneless hook registration shape wrong: %+v", a)
	}
}

func TestHookSessionStartPanelessSuffixesPastLiveTmuxOwner(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "owner-dir", "socket_path": "/tmp/sockOwn", "session_id": "$1", "pane_id": "%1",
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
	ag, found := hookGetAgent("owner-dir")
	if !found || ag.SessionID != "$1" || ag.SocketPath != "/tmp/sockOwn" {
		t.Fatalf("a live tmux owner must keep the alias, got %+v found=%v", ag, found)
	}
	suf, found := hookGetAgent("owner-dir-2")
	if !found || suf.SessionID != "hs-thief" || suf.SocketPath != "" {
		t.Fatalf("the paneless session must allocate the next suffix, got %+v found=%v", suf, found)
	}
}

func TestHookSessionStartPanelessAllocatesUniqueAliases(t *testing.T) {
	startTestDaemon(t)
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

	want := map[string]string{"shared-dir": "hs-one", "shared-dir-2": "hs-two", "shared-dir-3": "hs-three"}
	for alias, uuid := range want {
		ag, found := hookGetAgent(alias)
		if !found || ag.SessionID != uuid || ag.SocketPath != "" || ag.Departed {
			t.Fatalf("%s: want live paneless row for %s, got %+v found=%v", alias, uuid, ag, found)
		}
	}
	if _, found := hookGetAgent("shared-dir-4"); found {
		t.Fatal("a resumed session must reuse its alias, not allocate shared-dir-4")
	}
}

func TestHookSessionStartPanelessRevivesOwnTombstoneOnResume(t *testing.T) {
	startTestDaemon(t)
	panelessEnv(t, "", "revive-dir")
	payload := `{"session_id":"hs-rev","cwd":"/x/revive-dir"}`
	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}
	if ag, _ := hookGetAgent("revive-dir"); !ag.Departed {
		t.Fatalf("setup: expected tombstone after SessionEnd, got %+v", ag)
	}
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}
	ag, found := hookGetAgent("revive-dir")
	if !found || ag.Departed || ag.SessionID != "hs-rev" {
		t.Fatalf("resume must revive the session's own tombstone, got %+v found=%v", ag, found)
	}
	if _, found := hookGetAgent("revive-dir-2"); found {
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
