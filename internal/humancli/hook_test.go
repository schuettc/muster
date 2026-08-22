package humancli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schuettc/muster/internal/harnessenv"
	"github.com/schuettc/muster/internal/mustertest"
	"github.com/schuettc/muster/internal/tmuxenv"
)

func TestHookStopLoopGuard(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{"stop_hook_active":true}`), &buf); err != nil {
		t.Fatalf("hook Stop: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output on loop guard, got %q", buf.String())
	}
}

func TestHookStopCursorLoopGuard(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	for _, input := range []string{
		`{"loop_count":1}`,
		`{"status":"aborted"}`,
		`{"status":"error"}`,
	} {
		var buf bytes.Buffer
		if err := cmdHook([]string{"Stop", "cursor"}, strings.NewReader(input), &buf); err != nil {
			t.Fatalf("hook Stop input %s: %v", input, err)
		}
		if buf.Len() != 0 {
			t.Fatalf("input %s: expected no output on loop guard, got %q", input, buf.String())
		}
	}
}

// TestHookAliasSeedsExplicitOverrideToo pins the fix for a carve-out that
// used to survive in hookAlias alone: the derived branch (tmux session name)
// already seeded, but the $MUSTER_ALIAS branch returned the operator's value
// raw, on the reasoning that an explicit choice should never be rewritten.
// That reasoning is exactly what this feature retires — once the prefix is
// hidden locally, an unseeded $MUSTER_ALIAS would be indistinguishable on
// screen from a device-scoped one, so hookRegisterPane (the SessionStart
// hook's own registration path, the way sessions register in production)
// must seed it like every other mint site.
func TestHookAliasSeedsExplicitOverrideToo(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	t.Setenv("MUSTER_ALIAS", "worker")
	if got, want := hookAlias(tmuxenv.Capture{SessionName: "ignored-when-alias-set"}), "testdev-worker"; got != want {
		t.Fatalf("hookAlias with $MUSTER_ALIAS set = %q, want %q", got, want)
	}
}

func TestHookStopNoTmux(t *testing.T) {
	t.Setenv("TMUX", "")
	// Pin the harness identity away: on a dev machine `go test` itself runs
	// inside a Claude session whose CLAUDE_CODE_SESSION_ID would otherwise
	// give this "no identity" scenario a paneless identity and a daemon dial.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	// Pin the ancestry-walk fallback away too (finding F1 made hookStop try
	// it when $TMUX is empty): on a dev machine `go test` itself commonly
	// runs inside a real tmux pane, and without this the walk could resolve
	// that real pane and turn this "genuinely paneless" test into the tmux
	// path.
	pinAncestryWalkAway(t)
	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{}`), &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output outside tmux, got %q", buf.String())
	}
}

func TestHookStopNoUnread(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	prev := tmuxenv.Run
	t.Cleanup(func() { tmuxenv.Run = prev })
	for _, c := range []string{"", "abc", "0", "-1"} {
		count := c
		tmuxenv.Run = func(_ ...string) (string, error) { return count, nil }
		var buf bytes.Buffer
		if err := cmdHook([]string{"Stop"}, strings.NewReader(`{}`), &buf); err != nil {
			t.Fatal(err)
		}
		if buf.Len() != 0 {
			t.Fatalf("count=%q: expected no output, got %q", c, buf.String())
		}
	}
}

// hookRun returns a tmuxenv.Run stub keyed by the last arg (the tmux format
// or option name), matching the pattern the daemon-backed hook tests need:
// callers only need to supply the format→value pairs they care about.
func hookRun(values map[string]string) func(args ...string) (string, error) {
	return func(args ...string) (string, error) {
		if v, ok := values[args[len(args)-1]]; ok {
			return v, nil
		}
		return "", nil
	}
}

func runHook(t *testing.T) stopReason {
	t.Helper()
	var buf bytes.Buffer
	// Invalid stdin JSON must be tolerated (treated as stop_hook_active=false),
	// still proceeding to the count-based decision below.
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`not json`), &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatalf("expected a block decision, got no output")
	}
	var res stopReason
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("output not valid JSON: %v (%q)", err, buf.String())
	}
	if res.Decision != "block" {
		t.Fatalf("decision = %q, want block", res.Decision)
	}
	return res
}

// TestHookStopUnreadEmitsBlockDecision covers the ordinary single-alias path:
// the hook's real session_unread/session_aliases calls succeed against a live
// (test) daemon, and the reason names that one alias with today's singular
// wording (spec §3).
func TestHookStopUnreadEmitsBlockDecision(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "backend", "role": "peer", "model_type": "claude",
		"socket_path": "/tmp/sock", "session_id": "$1", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "role": "peer", "model_type": "claude",
		"socket_path": "/tmp/other", "session_id": "$2", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{
		"from": "other", "to_kind": "agent", "to_target": "backend", "subject": "s", "body": "b",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sock,1,0")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "3", "#{session_id}": "$1", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	res := runHook(t)
	if !strings.Contains(res.Reason, "alias 'backend'") || !strings.Contains(res.Reason, "1 unread muster thread(s)") {
		t.Fatalf("reason missing expected fields: %q", res.Reason)
	}
	if strings.Contains(res.Reason, "needing action") {
		t.Fatalf("no action-requested thread: reason must not mention action count: %q", res.Reason)
	}
	if !strings.Contains(res.Reason, "muster inbox 'backend'") || !strings.Contains(res.Reason, "muster reply") {
		t.Fatalf("reason must carry the CLI fallback (a dead MCP connection must not strand the agent): %q", res.Reason)
	}
}

// TestHookStopMultiAliasListsAllSorted: a session with two sibling aliases
// (the split-identity case, spec §3) must have its block reason list BOTH,
// sorted, with the for-each instruction — not just the alias the hook
// happened to observe via the tmux option.
func TestHookStopMultiAliasListsAllSorted(t *testing.T) {
	startTestDaemon(t)
	for _, alias := range []string{"zeta", "alpha"} { // registered out of sorted order
		if _, err := callData("register_agent", map[string]any{
			"alias": alias, "role": "peer", "model_type": "claude",
			"socket_path": "/tmp/sock2", "session_id": "$5", "session_created": 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "role": "peer", "model_type": "claude",
		"socket_path": "/tmp/other2", "session_id": "$6", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{
		"from": "other", "to_kind": "agent", "to_target": "alpha", "subject": "s", "body": "b",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sock2,1,0")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "9", "#{session_id}": "$5", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	res := runHook(t)
	if !strings.Contains(res.Reason, "Your muster aliases are 'alpha', 'zeta'") {
		t.Fatalf("reason must list both aliases sorted: %q", res.Reason)
	}
	if !strings.Contains(res.Reason, "For EACH alias call get_inbox") {
		t.Fatalf("reason must carry the for-each drain instruction: %q", res.Reason)
	}
	if !strings.Contains(res.Reason, "muster inbox '<alias>'") {
		t.Fatalf("reason must carry the CLI fallback with the <alias> placeholder: %q", res.Reason)
	}
}

// TestHookStopActionCountAppearsWhenActionable: an action-requested unread
// thread must append ", N needing action" to the count line (spec §2).
func TestHookStopActionCountAppearsWhenActionable(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "worker", "role": "peer", "model_type": "claude",
		"socket_path": "/tmp/sock3", "session_id": "$7", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "role": "peer", "model_type": "claude",
		"socket_path": "/tmp/other3", "session_id": "$8", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{
		"from": "other", "to_kind": "agent", "to_target": "worker",
		"subject": "s", "body": "b", "intent": "action-requested",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sock3,1,0")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "1", "#{session_id}": "$7", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	res := runHook(t)
	if !strings.Contains(res.Reason, "1 unread muster thread(s), 1 needing action") {
		t.Fatalf("reason must append the action count: %q", res.Reason)
	}
}

// TestHookStopActionCountAbsentWhenNotActionable: an unread thread with no
// action-requested intent must NOT mention "needing action" at all (gated on
// M>0, spec §2).
func TestHookStopActionCountAbsentWhenNotActionable(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "worker", "role": "peer", "model_type": "claude",
		"socket_path": "/tmp/sock4", "session_id": "$9", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "role": "peer", "model_type": "claude",
		"socket_path": "/tmp/other4", "session_id": "$10", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{
		"from": "other", "to_kind": "agent", "to_target": "worker",
		"subject": "s", "body": "b", "intent": "fyi",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sock4,1,0")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "1", "#{session_id}": "$9", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	res := runHook(t)
	if !strings.Contains(res.Reason, "1 unread muster thread(s)") {
		t.Fatalf("reason missing count: %q", res.Reason)
	}
	if strings.Contains(res.Reason, "needing action") {
		t.Fatalf("fyi-only unread must not mention action count: %q", res.Reason)
	}
}

// TestHookStopSessionUnreadFailureFallsBackToOptionCount: when the daemon
// can't resolve a session_id (tmux couldn't answer #{session_id}, here left
// unmapped), session_unread fails its required-field check, and the hook
// must fall back to the @muster_inbox option's count rather than going
// silent (spec §3).
func TestHookStopSessionUnreadFailureFallsBackToOptionCount(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sockX,1,0")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "4", "#{session_name}": "solo-hook"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	res := runHook(t)
	if !strings.Contains(res.Reason, "4 unread muster thread(s)") {
		t.Fatalf("reason must fall back to the option count (4): %q", res.Reason)
	}
	if strings.Contains(res.Reason, "needing action") {
		t.Fatalf("fallback count has no action breakdown, must not mention it: %q", res.Reason)
	}
}

// TestHookStopSessionAliasesFailureFallsBackToSessionName: the same
// unresolved-session_id scenario must also fall back session_aliases to
// today's single session-name wording (spec §3) — SEEDED, since the reason
// text is a model surface and the alias it names has to be the stored one.
func TestHookStopSessionAliasesFailureFallsBackToSessionName(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	t.Setenv("TMUX", "/tmp/sockY,1,0")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "2", "#{session_name}": "fallback-session"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	res := runHook(t)
	if !strings.Contains(res.Reason, "alias 'testdev-fallback-session'") {
		t.Fatalf("reason must fall back to the seeded session-name wording: %q", res.Reason)
	}
	if strings.Contains(res.Reason, "aliases are") {
		t.Fatalf("fallback must use singular wording, not the multi-alias form: %q", res.Reason)
	}
}

func TestHookSessionStartAndEnd(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	t.Setenv("TMUX", "/tmp/tmux-0/proj-muster,1,0")
	t.Setenv("TMUX_PANE", "%0")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		switch args[len(args)-1] {
		case "#{session_id}":
			return "$1", nil
		case "#{session_name}":
			return "muster-hook", nil
		default:
			return "", nil
		}
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart", "codex"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("SessionStart: %v", err)
	}
	agents := listAgentsForTest(t, "")
	found := false
	for _, a := range agents {
		if a.Alias == "testdev-muster-hook" && a.ModelType == "codex" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected testdev-muster-hook registered via SessionStart hook: %+v", agents)
	}

	buf.Reset()
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("SessionEnd: %v", err)
	}
	agents = listAgentsForTest(t, "")
	found = false
	for _, a := range agents {
		if a.Alias == "testdev-muster-hook" {
			found = true
			if !a.Departed {
				t.Fatalf("expected testdev-muster-hook tombstoned (Departed=true) via SessionEnd hook, got %+v", a)
			}
		}
	}
	if !found {
		t.Fatalf("expected testdev-muster-hook's row to SURVIVE SessionEnd (tombstoned, not deleted): %+v", agents)
	}
}

func TestHookStaleBadgeSuppressedByAuthoritativeZero(t *testing.T) {
	// Regression test for stale mailbox badges from isolated test daemons.
	// The @muster_inbox tmux option reports 2 (stale from a previous mail),
	// but the daemon's authoritative session_unread query returns total=0
	// (no actual threads). The hook must suppress the block decision because
	// the authoritative count is 0, not emit a false positive.
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "worker", "role": "peer", "model_type": "claude",
		"socket_path": "/tmp/sock_stale", "session_id": "$99", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	// Deliberately send NO messages, so session_unread returns total=0.

	t.Setenv("TMUX", "/tmp/sock_stale,1,0")
	prev := tmuxenv.Run
	// Stub tmux to report stale @muster_inbox=2 but matching session_id.
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "2", "#{session_id}": "$99", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{}`), &buf); err != nil {
		t.Fatal(err)
	}
	// The authoritative total=0 from session_unread must suppress output
	// despite the stale @muster_inbox=2 option.
	if buf.Len() != 0 {
		t.Fatalf("expected no output (authoritative zero suppresses stale badge), got %q", buf.String())
	}
}

// hookAgentPane fetches an alias's stored pane_id through get_agent, for
// asserting ownership-gate outcomes in the tests below.
func hookAgentPane(t *testing.T, alias string) (paneID string, found bool) {
	t.Helper()
	raw, err := callData("get_agent", map[string]any{"alias": alias})
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Found bool      `json:"found"`
		Agent agentFull `json:"agent"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res.Agent.PaneID, res.Found
}

// TestHookSessionStartNoOpForLiveForeignOwner covers the SessionStart gate's
// core no-op case (spec: "a live different pane means a primary already owns
// this identity"): the alias is already registered to the SAME
// (socket_path, session_id) tuple, but its stored pane ('%1') is a different,
// still-live pane than mine ('%2') — SessionStart must not steal it.
func TestHookSessionStartNoOpForLiveForeignOwner(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "start-owner", "socket_path": "/tmp/sockA", "session_id": "$1", "session_created": 100, "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockA,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$1", "#{session_created}": "100",
		"#{session_name}": "start-owner",
		"#{pane_id}":      "%1", // IsPaneAlive("/tmp/sockA","%1") -> alive
	})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("SessionStart: %v", err)
	}
	pane, found := hookAgentPane(t, "start-owner")
	if !found || pane != "%1" {
		t.Fatalf("expected stored pane to remain the live foreign owner '%%1', got pane=%q found=%v", pane, found)
	}
}

// TestHookSessionStartClaims table-drives every case where SessionStart MUST
// claim the identity (register/overwrite to the calling pane), per spec §1:
// dead former owner, empty stored pane, stored pane already mine, no prior
// registration, and a foreign (socket_path, session_id) tuple (cross-session
// takeover).
func TestHookSessionStartClaims(t *testing.T) {
	cases := []struct {
		name        string
		preRegister bool
		socketPath  string
		sessionID   string
		storedPane  string
		paneAlive   string // stub value for the "#{pane_id}" liveness query
	}{
		{name: "foreign pane dead", preRegister: true, socketPath: "/tmp/sockC", sessionID: "$1", storedPane: "%1", paneAlive: ""},
		{name: "stored pane empty", preRegister: true, socketPath: "/tmp/sockC", sessionID: "$1", storedPane: ""},
		{name: "stored pane mine", preRegister: true, socketPath: "/tmp/sockC", sessionID: "$1", storedPane: "%2"},
		{name: "alias absent", preRegister: false},
		{name: "foreign tuple", preRegister: true, socketPath: "/tmp/sockOTHER", sessionID: "$99", storedPane: "%1", paneAlive: "%1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			startTestDaemon(t)
			t.Setenv("MUSTER_DEVICE_NAME", "testdev")
			if c.preRegister {
				// Pre-registered under the seeded form: the incoming derived
				// session name "claim-me" is seeded to "testdev-claim-me"
				// before the hook claims it, so the pre-existing row must be
				// under that same seeded alias for the claim to land on it.
				if _, err := callData("register_agent", map[string]any{
					"alias": "testdev-claim-me", "socket_path": c.socketPath, "session_id": c.sessionID, "pane_id": c.storedPane,
				}); err != nil {
					t.Fatal(err)
				}
			}

			t.Setenv("TMUX", "/tmp/sockC,1,0")
			t.Setenv("TMUX_PANE", "%2")
			t.Setenv("MUSTER_ALIAS", "")
			prev := tmuxenv.Run
			tmuxenv.Run = hookRun(map[string]string{
				"#{session_id}": "$1", "#{session_created}": "100",
				"#{session_name}": "claim-me",
				"#{pane_id}":      c.paneAlive,
			})
			t.Cleanup(func() { tmuxenv.Run = prev })

			var buf bytes.Buffer
			if err := cmdHook([]string{"SessionStart"}, strings.NewReader(""), &buf); err != nil {
				t.Fatalf("SessionStart: %v", err)
			}
			pane, found := hookAgentPane(t, "testdev-claim-me")
			if !found || pane != "%2" {
				t.Fatalf("expected testdev-claim-me claimed to my pane '%%2', got pane=%q found=%v", pane, found)
			}
		})
	}
}

// TestHookStopSilentForNonOwner covers the Stop gate (spec §2): the session's
// registered alias has unread mail, but its stored pane ('%1') is a
// different, live pane than mine ('%2') — the drain decision must not be
// emitted to a sibling pane.
func TestHookStopSilentForNonOwner(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "stop-nonowner", "socket_path": "/tmp/sockB", "session_id": "$5", "session_created": 100, "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "socket_path": "/tmp/otherB", "session_id": "$6", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{
		"from": "other", "to_kind": "agent", "to_target": "stop-nonowner", "subject": "s", "body": "b",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockB,1,0")
	t.Setenv("TMUX_PANE", "%2")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "3", "#{session_id}": "$5", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{}`), &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output for a non-owner pane, got %q", buf.String())
	}
}

// TestHookStopDrainsForOwner is TestHookStopSilentForNonOwner's mirror: same
// setup, but $TMUX_PANE matches the registered alias's stored pane — the
// drain decision must be emitted.
func TestHookStopDrainsForOwner(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "stop-owner", "socket_path": "/tmp/sockD", "session_id": "$7", "session_created": 100, "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "socket_path": "/tmp/otherD", "session_id": "$8", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{
		"from": "other", "to_kind": "agent", "to_target": "stop-owner", "subject": "s", "body": "b",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockD,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "3", "#{session_id}": "$7", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	res := runHook(t)
	if !strings.Contains(res.Reason, "alias 'stop-owner'") {
		t.Fatalf("reason missing owner alias: %q", res.Reason)
	}
}

// TestHookStopFallbackWhenPaneUnknown asserts the existing-tests-keep-passing
// property explicitly (spec §2): a registration with an EMPTY pane_id carries
// no owner information at all, so the Stop gate must not engage — it drains
// exactly as it did before pane ownership existed, regardless of $TMUX_PANE.
func TestHookStopFallbackWhenPaneUnknown(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "stop-unknown-pane", "socket_path": "/tmp/sockE", "session_id": "$9", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "socket_path": "/tmp/otherE", "session_id": "$10", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{
		"from": "other", "to_kind": "agent", "to_target": "stop-unknown-pane", "subject": "s", "body": "b",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockE,1,0")
	t.Setenv("TMUX_PANE", "%9")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "3", "#{session_id}": "$9", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	res := runHook(t)
	if !strings.Contains(res.Reason, "alias 'stop-unknown-pane'") {
		t.Fatalf("reason missing owner alias: %q", res.Reason)
	}
}

// TestHookSessionEndNoOpForNonOwner covers the SessionEnd gate (spec §3): a
// dying sibling pane must not tombstone the primary's registration.
func TestHookSessionEndNoOpForNonOwner(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "end-nonowner", "socket_path": "/tmp/sockF", "session_id": "$1", "session_created": 100, "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockF,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1", "#{session_created}": "100", "#{session_name}": "end-nonowner"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("SessionEnd: %v", err)
	}
	agents := listAgentsForTest(t, "")
	for _, a := range agents {
		if a.Alias == "end-nonowner" && a.Departed {
			t.Fatalf("a non-owner pane's SessionEnd must not tombstone the primary: %+v", a)
		}
	}
}

// TestHookSessionEndDeregistersAllOwnedAliases is the thread-85 identity
// leak: a session that registered a custom alias on top of its session-name
// alias must have BOTH tombstoned at SessionEnd — the tmux session (and its
// @muster_agent option) outlives the Claude session, so a leaked alias is
// silently inherited, inbox and all, by the next agent in that pane. A
// sibling pane's registration on the same session and an agent in a
// different tmux session must both survive the sweep.
func TestHookSessionEndDeregistersAllOwnedAliases(t *testing.T) {
	startTestDaemon(t)
	reg := func(alias, pane string) {
		t.Helper()
		if _, err := callData("register_agent", map[string]any{
			"alias": alias, "socket_path": "/tmp/sockH", "session_id": "$1", "session_created": 100, "pane_id": pane,
		}); err != nil {
			t.Fatal(err)
		}
	}
	reg("end-sweep", "%2")   // the session-name alias
	reg("lake-broker", "%2") // the custom alias that used to leak
	reg("end-paneless", "")  // pane-unset row: owned per the existing gate
	reg("end-sibling", "%7") // sibling pane on the SAME session: not ours
	if _, err := callData("register_agent", map[string]any{
		"alias": "end-elsewhere", "socket_path": "/tmp/sockH", "session_id": "$2", "session_created": 100, "pane_id": "%2",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockH,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1", "#{session_created}": "100", "#{session_name}": "end-sweep"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("SessionEnd: %v", err)
	}
	wantDeparted := map[string]bool{
		"end-sweep": true, "lake-broker": true, "end-paneless": true,
		"end-sibling": false, "end-elsewhere": false,
	}
	for _, a := range listAgentsForTest(t, "") {
		if want, tracked := wantDeparted[a.Alias]; tracked && a.Departed != want {
			t.Errorf("%s: departed=%v, want %v", a.Alias, a.Departed, want)
		}
	}
}

// TestHookSessionEndDeregistersForOwner is the mirror: my pane owns the
// registration, so SessionEnd must deregister (tombstone) it as today.
func TestHookSessionEndDeregistersForOwner(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "end-owner", "socket_path": "/tmp/sockG", "session_id": "$1", "session_created": 100, "pane_id": "%2",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockG,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1", "#{session_created}": "100", "#{session_name}": "end-owner"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("SessionEnd: %v", err)
	}
	agents := listAgentsForTest(t, "")
	found := false
	for _, a := range agents {
		if a.Alias == "end-owner" {
			found = true
			if !a.Departed {
				t.Fatalf("owner's SessionEnd must tombstone the registration, got %+v", a)
			}
		}
	}
	if !found {
		t.Fatalf("expected end-owner's row to survive SessionEnd (tombstoned, not deleted)")
	}
}

// TestHookSessionEndNoOpForForeignTuple: the alias is registered, but to a
// DIFFERENT (socket_path, session_id) than mine (e.g. a stale row from a
// previous incarnation of this session name) — SessionEnd must not touch a
// registration it doesn't own the identity of.
func TestHookSessionEndNoOpForForeignTuple(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "end-foreign-tuple", "socket_path": "/tmp/sockOLD", "session_id": "$OLD", "session_created": 100, "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockH,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$NEW", "#{session_created}": "100", "#{session_name}": "end-foreign-tuple"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("SessionEnd: %v", err)
	}
	agents := listAgentsForTest(t, "")
	for _, a := range agents {
		if a.Alias == "end-foreign-tuple" && a.Departed {
			t.Fatalf("SessionEnd must not tombstone a registration owned by a different (socket,session) tuple: %+v", a)
		}
	}
}

// TestHookSessionEndDoesNotExpandOwnedRowIntoALiveForeignAgent is the review
// finding on this task: hookSessionEnd enumerates rows already resolved off
// the roster (ag.Alias, an exact stored string for THIS tuple) and used to
// hand them to cmdDeregister's explicit-arg branch, which local-first
// expands. A bare row belonging to the dying session ("work" on tuple B) can
// share its bare short name with an unrelated LIVE seeded row on a different
// tuple ("personal-work" on tuple A) — expansion must never let tuple B's
// SessionEnd reach across and tombstone tuple A's agent. Only tuple B's own
// "work" row may depart; "personal-work" must stay live. This is the same
// leftover-identity hazard hookSessionEnd's own doc comment (the lake-broker
// incident) exists to prevent, now via a different mechanism (expansion
// instead of an unswept alias).
func TestHookSessionEndDoesNotExpandOwnedRowIntoALiveForeignAgent(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	if _, err := callData("register_agent", map[string]any{
		"alias": "personal-work", "socket_path": "/tmp/sockA", "session_id": "$A", "session_created": 100, "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "work", "socket_path": "/tmp/sockB", "session_id": "$B", "session_created": 100, "pane_id": "%2",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockB,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$B", "#{session_created}": "100", "#{session_name}": "work"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("SessionEnd: %v", err)
	}

	var local, foreign agentRow
	for _, a := range listAgentsForTest(t, "") {
		switch a.Alias {
		case "personal-work":
			local = a
		case "work":
			foreign = a
		}
	}
	if local.Departed {
		t.Fatalf("session B's SessionEnd must not tombstone session A's live 'personal-work', got %+v", local)
	}
	if !foreign.Departed {
		t.Fatalf("session B's own 'work' row must be departed, got %+v", foreign)
	}
}

func TestHookSessionStartBestEffortWhenDaemonUnreachable(t *testing.T) {
	// No test daemon started, and no tmux identity to fall back on: cmdRegister
	// will fail (can't determine alias / can't reach daemon), but the hook must
	// swallow that and still return nil — a hook must never block a session.
	//
	// MUSTER_HOME must be isolated here (a dead socket path with nothing
	// listening): left unset, paths.SocketPath() falls back to the real
	// ~/.local/share/muster/sock, which on a dev machine running the live
	// muster daemon would silently dial IT instead of exercising the
	// unreachable-daemon branch this test is named for. MUSTER_NO_AUTOSPAWN
	// skips client.dialOrSpawn's auto-start fallback — under `go test`,
	// os.Executable() resolves to the test binary itself, so that fallback
	// would otherwise re-exec the whole suite as a detached child (a fork
	// bomb reproduced in CI) instead of a real `muster serve`.
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	t.Setenv("MUSTER_NO_AUTOSPAWN", "1")
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("MUSTER_ALIAS", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "") // pin: a dev machine's own Claude session must not lend this test an identity
	pinAncestryWalkAway(t)                 // pin: hookCapture's ancestry fallback must not resolve this test process's real tmux pane
	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("hook must never return an error, got %v", err)
	}
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("hook must never return an error, got %v", err)
	}
}

// TestHookSessionStartClaimsOverDepartedRow covers finding 1's SessionStart
// half (a tombstone never owns the identity): the same (socket_path,
// session_id) tuple is registered, then deregistered — the row survives as a
// tombstone (departed=true) with its pane_id ('%1') intact, and that pane is
// still alive-but-foreign (a different, still-live pane than mine). Without
// decoding Departed, the old-owner-alive check would refuse to claim forever
// (the lockout scenario from the review finding). A departed row must be
// claimable regardless of whether its stored pane is alive.
func TestHookSessionStartClaimsOverDepartedRow(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	// Pre-existing row is seeded to match what the incoming derived session
	// name "claim-departed" will be seeded to below.
	if _, err := callData("register_agent", map[string]any{
		"alias": "testdev-claim-departed", "socket_path": "/tmp/sockDep", "session_id": "$1", "session_created": 100, "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("deregister_agent", map[string]any{"alias": "testdev-claim-departed"}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockDep,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$1", "#{session_created}": "100",
		"#{session_name}": "claim-departed",
		"#{pane_id}":      "%1", // the old owner's pane is still alive
	})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("SessionStart: %v", err)
	}
	ag, found, _ := hookGetAgent("testdev-claim-departed")
	if !found {
		t.Fatal("expected testdev-claim-departed to still be registered")
	}
	if ag.PaneID != "%2" {
		t.Fatalf("expected the roster pane to become mine ('%%2'), got %q", ag.PaneID)
	}
	if ag.Departed {
		t.Fatalf("expected testdev-claim-departed revived (Departed=false) after SessionStart claims it, got %+v", ag)
	}
}

// TestHookStopSilentWhenOnlyOwnerIsAnUnclaimedTombstone: the session's only
// registered alias was deregistered without ever being claimed onward via
// Become. Per the one-conversation-one-identity spec (2026-08-21 §2, task 2:
// store.SessionUnread / SessionAliasLineage's base case), such a row is
// excluded from its tuple's lineage — it is indistinguishable from a PRIOR
// conversation's leftover tombstone on a reused tuple, and nothing points
// forward from it to claim its waiting mail. So session_unread correctly
// reports 0 for this tuple and Stop prints nothing, rather than draining
// mail addressed to an identity nobody live owns any more.
//
// This superseded an earlier test (TestHookStopDrainsWhenOnlyNamedOwnerIsDeparted)
// that asserted the opposite — an unconditional drain — under the pre-task-2
// semantics, where a bare departed row still counted toward session_unread.
// hookStopOwnsAnyAlias's own "a tombstoned row neither grants nor denies
// ownership" behavior is unchanged and still exercised whenever a departed
// row DOES survive in scope (i.e. it has a successor — see
// TestBecomeReclaimsDeparted-style chains in the store conformance suite);
// this test instead covers the now-earlier `total <= 0` exit that fires
// first when the only candidate row never chained forward.
func TestHookStopSilentWhenOnlyOwnerIsAnUnclaimedTombstone(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "stop-departed", "socket_path": "/tmp/sockStopDep", "session_id": "$5", "session_created": 100, "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "socket_path": "/tmp/otherStopDep", "session_id": "$6", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{
		"from": "other", "to_kind": "agent", "to_target": "stop-departed", "subject": "s", "body": "b",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("deregister_agent", map[string]any{"alias": "stop-departed"}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockStopDep,1,0")
	t.Setenv("TMUX_PANE", "%2") // not the tombstone's stored pane ('%1')
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "3", "#{session_id}": "$5", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`not json`), &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output — an unclaimed tombstone's mail is orphaned, not drained, got %q", buf.String())
	}
}

// TestHookSessionEndUnresolvableIdentityNeverDialsDaemon covers finding 2:
// outside any resolvable identity (no $MUSTER_ALIAS, no tmux session name —
// e.g. a global hook firing for a non-tmux Claude session), hookOwnsIdentity
// must return false BEFORE ever calling the daemon. Deliberately no
// startTestDaemon and MUSTER_NO_AUTOSPAWN set to "" (which is INACTIVE, not
// unset — the guard checks != "", so an empty string leaves client.Call free
// to auto-spawn): if the fix regressed and this hook dialed the daemon
// anyway, dialOrSpawn would find the socket dead and re-exec the test binary
// itself as "serve", reproducing the CI fork bomb the review finding
// describes. Bounded with a goroutine + timeout so a regression fails fast
// instead of hanging/forking the suite.
func TestHookSessionEndUnresolvableIdentityNeverDialsDaemon(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir) // isolated, dead socket path — nothing listens here
	t.Setenv("MUSTER_NO_AUTOSPAWN", "")
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("MUSTER_ALIAS", "")
	// Pin the harness identity away: with a CLAUDE_CODE_SESSION_ID leaking in
	// from a dev machine's own Claude session, SessionEnd legitimately HAS an
	// identity (the paneless tuple) and dialing would be correct — this test
	// is specifically about the no-identity-at-all branch.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	// Pin the ancestry-walk fallback away too (finding F2 routes SessionEnd
	// through hookCapture, which tries this walk when $TMUX is empty): on a
	// dev machine `go test` itself commonly runs inside a real tmux pane,
	// and a resolved walk would legitimately have a tuple to dial the
	// daemon about — this test is specifically about the walk ALSO coming
	// back empty.
	pinAncestryWalkAway(t)

	done := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		done <- cmdHook([]string{"SessionEnd"}, strings.NewReader(""), &buf)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("hook must never return an error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cmdHook SessionEnd did not return promptly — likely dialed/spawned the daemon with an unresolvable identity")
	}
}

func TestHookReasonLeadsWithLabelWhenPresent(t *testing.T) {
	got := hookReason(2, 1, []string{"timewalk-2"}, "standard 2000")
	if !strings.Contains(got, "You are 'standard 2000' — muster alias 'timewalk-2'") {
		t.Fatalf("single-alias reason must lead with the label, got %q", got)
	}
	if !strings.Contains(got, "get_inbox tool now with alias 'timewalk-2'") {
		t.Fatalf("tool instructions must still use the alias, got %q", got)
	}
}

func TestHookReasonUnlabeledWordingUnchanged(t *testing.T) {
	got := hookReason(1, 0, []string{"dotfiles"}, "")
	if !strings.Contains(got, "Your muster alias is 'dotfiles' (this tmux session).") {
		t.Fatalf("empty label must render today's wording, got %q", got)
	}
	if strings.Contains(got, "You are ''") {
		t.Fatalf("empty label must not render an empty You-are clause: %q", got)
	}
}

func TestHookReasonMultiAliasWithLabel(t *testing.T) {
	got := hookReason(3, 0, []string{"timewalk-2", "timewalk-2002"}, "standard 2000")
	if !strings.Contains(got, "You are 'standard 2000' — muster aliases 'timewalk-2', 'timewalk-2002'") {
		t.Fatalf("multi-alias reason must lead with the label, got %q", got)
	}
}

// TestHookStopStampsHarnessLink covers the repair half of the durable-alias
// spec: a custom alias registered via the MCP tool (no harness link) gets the
// payload's session_id stamped when the Stop hook fires for real mail — so a
// later resume can find the row. The stamp piggybacks on the mail gate: no
// mail, no daemon dials, no stamp.
func TestHookStopStampsHarnessLink(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	if _, err := callData("register_agent", map[string]any{
		"alias": "backend", "socket_path": "/tmp/sock", "session_id": "$1", "session_created": 100, "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "sender", "socket_path": "/tmp/sock", "session_id": "$2", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "backend", "subject": "s", "body": "b",
	}); err != nil {
		t.Fatal(err)
	}
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"@muster_inbox": "1",
		"#{session_id}": "$1", "#{session_created}": "100",
		"#{session_name}": "backend",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{"session_id":"uuid-9"}`), &buf); err != nil {
		t.Fatal(err)
	}
	ag, ok, _ := hookGetAgent("backend")
	if !ok || ag.HarnessSessionID != "uuid-9" {
		t.Fatalf("harness link after Stop = %q (found=%v), want uuid-9", ag.HarnessSessionID, ok)
	}
}

// stubAncestryWalkToPane makes tmuxenv.CaptureFromAncestry resolve a single
// fake pane on socket, the way a hook spawned env-stripped but still a
// descendant of its pane's shell would (see ancestry_test.go's own
// TestCaptureFromAncestryMatchesPanePID for the same list-panes shape). Any
// display-message query not explicitly stubbed in extra falls through to "".
// Returns the socket path CaptureFromAncestry will actually report — a
// full temp-dir path, not the caller's nominal name, since the walk reports
// whatever it globbed.
func stubAncestryWalkToPane(t *testing.T, paneID, sessionID, sessionName string, sessionCreated int64, extra map[string]string) string {
	t.Helper()
	prevAnc, prevDir, prevRun := tmuxenv.AncestorPIDs, tmuxenv.SocketDir, tmuxenv.Run
	t.Cleanup(func() { tmuxenv.AncestorPIDs, tmuxenv.SocketDir, tmuxenv.Run = prevAnc, prevDir, prevRun })

	const fakePID = 4242
	tmuxenv.AncestorPIDs = func() []int { return []int{fakePID} }
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "proj-walk"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tmuxenv.SocketDir = func() string { return dir }
	// CaptureFromAncestry canonicalizes the matched socket path (Finding 1),
	// so the sock this helper hands back to the caller for registration must
	// match what the walk will actually resolve to — exactly as production
	// registration (also routed through tmuxenv) would produce.
	sock := tmuxenv.CanonicalSocketPath(filepath.Join(dir, "proj-walk"))

	tmuxenv.Run = func(args ...string) (string, error) {
		for _, a := range args {
			if a == "list-panes" {
				return fmt.Sprintf("%d\t%s\t%s\t%s\t%d", fakePID, paneID, sessionID, sessionName, sessionCreated), nil
			}
		}
		last := args[len(args)-1]
		if v, ok := extra[last]; ok {
			return v, nil
		}
		return "", nil
	}
	return sock
}

// TestHookStopRepairsHarnessLinkViaAncestryWalk covers finding F1 end to end:
// a harness spawns the Stop hook with $TMUX stripped (the production shape —
// every harness hook runs env-stripped), and the ONLY way to resolve tmux
// identity is the process-ancestry walk hookCapture already uses for
// SessionStart. Before the fix, hookStop gated on the literal $TMUX env var
// and ALWAYS took the paneless branch here, so it could never see the mail,
// never emit the drain decision, and never run stampHarnessLinks (the
// resume spec's repair path) — this alias would stay linkless forever.
func TestHookStopRepairsHarnessLinkViaAncestryWalk(t *testing.T) {
	startTestDaemon(t)
	sock := stubAncestryWalkToPane(t, "%7", "$3", "muster-walk", 555, map[string]string{
		"#{@muster_inbox}": "1",
	})
	if _, err := callData("register_agent", map[string]any{
		"alias": "walked-backend", "socket_path": sock, "session_id": "$3", "session_created": 555, "pane_id": "%7",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "sender", "socket_path": "/tmp/otherWalk", "session_id": "$4", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "walked-backend", "subject": "s", "body": "b",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")

	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{"session_id":"uuid-walk"}`), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "alias 'walked-backend'") {
		t.Fatalf("expected the walked pane's Stop to drain, got %q", buf.String())
	}
	ag, ok, _ := hookGetAgent("walked-backend")
	if !ok || ag.HarnessSessionID != "uuid-walk" {
		t.Fatalf("harness link after walked Stop = %q (found=%v), want uuid-walk", ag.HarnessSessionID, ok)
	}
}

// TestHookStopWalkedSilentWhenNoUnread covers the walked path's cheap gate
// (finding F1): the @muster_inbox option read socket-aware against the
// walked tuple must still gate the daemon calls — a walked pane with no mail
// must produce no output and no stamp attempt.
func TestHookStopWalkedSilentWhenNoUnread(t *testing.T) {
	startTestDaemon(t)
	stubAncestryWalkToPane(t, "%7", "$3", "muster-walk", 555, nil) // "@muster_inbox" unstubbed -> ""
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")

	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{}`), &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected silence with no unread mail, got %q", buf.String())
	}
}

// TestStampHarnessLinksScopesToOwnedPane covers finding F3: two agent panes
// sharing one tmux session (a primary plus a subagent pane, say) must not
// have one pane's Stop hook stamp its harness UUID onto a SIBLING pane's
// link-less alias. Only the calling pane's own alias gets stamped; the
// sibling's stays link-less.
func TestStampHarnessLinksScopesToOwnedPane(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sockPane,1,0")
	t.Setenv("TMUX_PANE", "%1")
	if _, err := callData("register_agent", map[string]any{
		"alias": "mine", "socket_path": "/tmp/sockPane", "session_id": "$1", "session_created": 100, "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "sibling", "socket_path": "/tmp/sockPane", "session_id": "$1", "session_created": 100, "pane_id": "%2",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "sender", "socket_path": "/tmp/otherPane", "session_id": "$2", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "mine", "subject": "s", "body": "b",
	}); err != nil {
		t.Fatal(err)
	}
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"@muster_inbox": "1",
		"#{session_id}": "$1", "#{session_created}": "100",
		"#{session_name}": "mine",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })

	// stampHarnessLinks only ever looks at aliases session_aliases returns
	// (both, since they share the tuple) — call it directly to isolate F3
	// from Stop's ownership gate (hookStopOwnsAnyAlias), which is a separate
	// concern already covered elsewhere.
	stampHarnessLinks([]string{"mine", "sibling"}, harnessenv.Capture{SessionID: "uuid-pane"}, "/tmp/sockPane", "$1", "%1")

	mine, _, _ := hookGetAgent("mine")
	sibling, _, _ := hookGetAgent("sibling")
	if mine.HarnessSessionID != "uuid-pane" {
		t.Fatalf("my own alias must be stamped, got %+v", mine)
	}
	if sibling.HarnessSessionID != "" {
		t.Fatalf("a sibling pane's alias must NOT be stamped, got %+v", sibling)
	}
}

// TestStampHarnessLinksProtectsEmptyPaneRowsExistingLink covers Finding 6: a
// row with an EMPTY pane_id (e.g. registered before its pane was captured,
// or a paneless-shaped row that still shares this tuple) carries no pane-
// level ownership proof, so the sibling-pane check above can't rule out an
// unrelated pane sharing the tuple. Before the fix, stampHarnessLinks
// rewrote such a row's harness link on EVERY Stop from ANY pane in the
// session — so a sibling pane's Stop could clobber a link that genuinely
// belonged to a different conversation/harness session. The stamp must
// still update the link when the caller's transcript PROVES it owns the
// row (Task 8's resume case), but must never overwrite an existing,
// differently-transcripted link on transcript proof alone being absent.
func TestStampHarnessLinksProtectsEmptyPaneRowsExistingLink(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "shared", "socket_path": "/tmp/sockShared", "session_id": "$1", "session_created": 100,
		// no pane_id: ambiguous ownership by design of this test
	}); err != nil {
		t.Fatal(err)
	}
	// Give it an established link belonging to a DIFFERENT conversation.
	if _, err := callData("stamp_harness_session", map[string]any{
		"alias": "shared", "harness_session_id": "uuid-owner", "transcript_path": "/t/owner.jsonl",
	}); err != nil {
		t.Fatal(err)
	}

	// A sibling pane's Stop hook, with no transcript proof that it IS the
	// owning conversation, must not clobber the existing link.
	stampHarnessLinks([]string{"shared"}, harnessenv.Capture{SessionID: "uuid-sibling", TranscriptPath: "/t/sibling.jsonl"}, "/tmp/sockShared", "$1", "%9")
	ag, _, _ := hookGetAgent("shared")
	if ag.HarnessSessionID != "uuid-owner" || ag.TranscriptPath != "/t/owner.jsonl" {
		t.Fatalf("an unrelated sibling must not overwrite the existing link, got %+v", ag)
	}

	// The OWNING conversation (transcript matches) resuming under a new
	// harness session id must still be able to repair the link — this is
	// exactly what Task 8 needed.
	stampHarnessLinks([]string{"shared"}, harnessenv.Capture{SessionID: "uuid-owner-2", TranscriptPath: "/t/owner.jsonl"}, "/tmp/sockShared", "$1", "%9")
	ag, _, _ = hookGetAgent("shared")
	if ag.HarnessSessionID != "uuid-owner-2" {
		t.Fatalf("the owning conversation (transcript match) must be able to repair its own link, got %+v", ag)
	}
}

// TestHookSessionStartResumeReclaimsAlias is the durable-alias spec's core
// scenario end to end: a conversation's alias was registered in a now-dead
// tmux session (tombstoned), mail arrived, and the conversation is resumed
// in a brand-new tmux session. The SessionStart hook with source:"resume"
// must re-register the alias onto the NEW tuple and print a summary line
// (which Claude Code injects into the session's context) naming the alias
// and the backlog — and must NOT additionally register a fresh
// session-name alias.
func TestHookSessionStartResumeReclaimsAlias(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("MUSTER_ALIAS", "")
	seed := func(op string, args map[string]any) {
		t.Helper()
		if _, err := callData(op, args); err != nil {
			t.Fatal(err)
		}
	}
	seed("register_agent", map[string]any{
		"alias": "backend-2", "socket_path": "/tmp/sock", "session_id": "$OLD",
		"session_created": 111, "harness_session_id": "uuid-42", "label": "lake", "label_manual": true,
	})
	seed("register_agent", map[string]any{"alias": "sender", "socket_path": "/tmp/sock", "session_id": "$2", "session_created": 100})
	seed("send_message", map[string]any{"from": "sender", "to_kind": "agent", "to_target": "backend-2", "subject": "s", "body": "b"})
	seed("deregister_agent", map[string]any{"alias": "backend-2"})

	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}":      "$NEW",
		"#{session_name}":    "muster-3",
		"#{session_created}": "222",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(`{"source":"resume","session_id":"uuid-42"}`), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "backend-2") || !strings.Contains(out, "1 unread") {
		t.Fatalf("resume summary missing alias/backlog:\n%s", out)
	}
	ag, ok, _ := hookGetAgent("backend-2")
	if !ok || ag.Departed || ag.SessionID != "$NEW" || ag.Label != "lake" {
		t.Fatalf("reclaimed row = %+v (found=%v), want live on $NEW with label kept", ag, ok)
	}
	// The fresh session-name register this guards against would also seed;
	// check the form it would actually mint, not the unseeded bare name.
	if _, exists, _ := hookGetAgent("testdev-muster-3"); exists {
		t.Fatalf("resume must not also register a fresh session-name alias")
	}
}

// TestHookSessionStartResumeReclaimsAliasByTranscript covers the case
// TestHookSessionStartResumeReclaimsAlias doesn't: a harness that mints a NEW
// session_id on every resume (unlike the tuple-stable id assumed above), so
// matching owned rows by harness_session_id alone finds nothing. The
// transcript_path is what's actually stable across a resume — it's the same
// file — so conversationRows must fall back to matching on it when the
// harness session id changed.
func TestHookSessionStartResumeReclaimsAliasByTranscript(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("MUSTER_ALIAS", "")
	seed := func(op string, args map[string]any) {
		t.Helper()
		if _, err := callData(op, args); err != nil {
			t.Fatal(err)
		}
	}
	seed("register_agent", map[string]any{
		"alias": "backend-2", "socket_path": "/tmp/dead-sock", "session_id": "$OLD",
		"session_created": 111, "harness_session_id": "uuid-A", "transcript_path": "/t/c.jsonl",
		"label": "lake", "label_manual": true,
	})
	seed("register_agent", map[string]any{"alias": "sender", "socket_path": "/tmp/sock", "session_id": "$2", "session_created": 100})
	seed("send_message", map[string]any{"from": "sender", "to_kind": "agent", "to_target": "backend-2", "subject": "s", "body": "b"})
	seed("deregister_agent", map[string]any{"alias": "backend-2"})

	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}":      "$NEW",
		"#{session_name}":    "muster-3",
		"#{session_created}": "222",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(`{"source":"resume","session_id":"uuid-B","transcript_path":"/t/c.jsonl"}`), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "reconnected as 'backend-2'") || !strings.Contains(out, "1 unread") {
		t.Fatalf("resume-by-transcript summary missing alias/backlog:\n%s", out)
	}
	ag, ok, _ := hookGetAgent("backend-2")
	if !ok || ag.Departed || ag.SessionID != "$NEW" || ag.Label != "lake" {
		t.Fatalf("reclaimed row = %+v (found=%v), want live on $NEW with label kept", ag, ok)
	}
	if ag.HarnessSessionID != "uuid-B" {
		t.Fatalf("reclaimed row harness_session_id = %q, want uuid-B (the new resume session id)", ag.HarnessSessionID)
	}
	if _, exists, _ := hookGetAgent("testdev-muster-3"); exists {
		t.Fatalf("resume-by-transcript must not also register a fresh session-name alias")
	}
}

// TestHookSessionStartResumeSkipsLiveCollision: a row still provably live in
// another tmux session is reported, never clobbered — and with nothing
// reclaimed the hook falls through to the normal session-name register.
func TestHookSessionStartResumeSkipsLiveCollision(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("MUSTER_ALIAS", "")
	if _, err := callData("register_agent", map[string]any{
		"alias": "backend-2", "socket_path": "/other-sock", "session_id": "$OLD",
		"session_created": 111, "harness_session_id": "uuid-42",
	}); err != nil {
		t.Fatal(err)
	}
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		// The liveness probe against /other-sock/$OLD must confirm alive.
		for _, a := range args {
			if a == "/other-sock" {
				return "111", nil
			}
		}
		return hookRun(map[string]string{
			"#{session_id}": "$NEW", "#{session_name}": "muster-3", "#{session_created}": "222",
		})(args...)
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(`{"source":"resume","session_id":"uuid-42"}`), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "not reclaimed") {
		t.Fatalf("expected a collision notice, got:\n%s", buf.String())
	}
	ag, _, _ := hookGetAgent("backend-2")
	if ag.SessionID != "$OLD" {
		t.Fatalf("collision row moved to %q — must stay on $OLD", ag.SessionID)
	}
	// The default session-name path seeds, like every other mint site.
	if fallback, found, _ := hookGetAgent("testdev-muster-3"); !found || fallback.Departed || fallback.SessionID != "$NEW" {
		t.Fatalf("nothing reclaimed: expected the default session-name alias 'testdev-muster-3' registered on $NEW, got %+v found=%v", fallback, found)
	}
}

// containsCall reports whether the recorded tmuxenv.Run argument lists
// contain exactly want — the projection's writes are asserted by their full
// argv (the "-S <socket>" prefix is the whole point: hooks run env-stripped,
// so an ambient set-option would land nowhere).
func containsCall(calls [][]string, want []string) bool {
	for _, got := range calls {
		if len(got) != len(want) {
			continue
		}
		same := true
		for i := range got {
			if got[i] != want[i] {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// registerProjectViaDaemon registers a live row carrying BOTH an explicit
// incarnation and a project: the incarnation so set_label's proven-match
// write lands (spec §5.1), the project so the collision warning's
// same-project scope has something to compare.
func registerProjectViaDaemon(t *testing.T, alias, socketPath, sessionID string, created int64, project string) {
	t.Helper()
	if _, err := callData("register_agent", map[string]any{
		"alias": alias, "socket_path": socketPath, "session_id": sessionID,
		"session_created": created, "project": project,
	}); err != nil {
		t.Fatal(err)
	}
}

// agentRowForTest returns one alias's full roster row (agentRow, not
// agentFull: only agentRow carries label_manual, which the projection's bus
// half is judged on).
func agentRowForTest(t *testing.T, alias string) agentRow {
	t.Helper()
	for _, a := range listAgentsForTest(t, "") {
		if a.Alias == alias {
			return a
		}
	}
	t.Fatalf("alias %q not in roster", alias)
	return agentRow{}
}

// captureTmuxCalls swaps tmuxenv.Run for a recorder and returns a pointer to
// the recorded argument lists.
func captureTmuxCalls(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })
	return &calls
}

// writeTranscript writes a one-record custom-title transcript and returns its
// path.
func writeTranscript(t *testing.T, title string) string {
	t.Helper()
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	rec := fmt.Sprintf(`{"type":"custom-title","customTitle":%q,"sessionId":"u1"}`+"\n", title)
	if err := os.WriteFile(tp, []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	return tp
}

// TestHookProjectNameProjectsCustomTitle: a SessionStart whose transcript
// carries a custom-title lands the name on (a) the tmux option pair via
// socket-aware writes and (b) the bus label, manual, on this incarnation.
func TestHookProjectNameProjectsCustomTitle(t *testing.T) {
	tp := writeTranscript(t, "nfl-3")
	calls := captureTmuxCalls(t)

	startCLITestDaemon(t)
	registerAliveViaDaemon(t, "muster-9", "/s", "$1", 200)

	var buf bytes.Buffer
	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", SessionCreated: 200, PaneID: "%5"}
	hookProjectName(c, harnessenv.CustomTitle(tp), &buf)

	// tmux half: option + manual flag + refresh, all socket-aware
	wantOpt := []string{"-S", "/s", "set-option", "-t", "$1", tmuxenv.LabelOption(), "nfl-3"}
	wantMan := []string{"-S", "/s", "set-option", "-t", "$1", tmuxenv.LabelOption() + "_manual", "1"}
	if !containsCall(*calls, wantOpt) || !containsCall(*calls, wantMan) {
		t.Fatalf("missing socket-aware option writes, got %v", *calls)
	}
	if !containsCall(*calls, []string{"-S", "/s", "refresh-client", "-S"}) {
		t.Fatalf("missing socket-aware refresh, got %v", *calls)
	}
	// bus half
	ag := agentRowForTest(t, "muster-9")
	if ag.Label != "nfl-3" || !ag.LabelManual {
		t.Fatalf("bus label = (%q, manual=%v), want (nfl-3, true)", ag.Label, ag.LabelManual)
	}
	if !strings.Contains(buf.String(), `session name "nfl-3"`) {
		t.Fatalf("expected context line, got %q", buf.String())
	}
}

// TestHookProjectNameNoTitleNoOp: no custom-title → no writes, no output
// (spec: never demote, never clear; a fresh unnamed session is untouched).
func TestHookProjectNameNoTitleNoOp(t *testing.T) {
	calls := captureTmuxCalls(t)

	var buf bytes.Buffer
	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", SessionCreated: 200, PaneID: "%5"}
	hookProjectName(c, "", &buf)
	for _, call := range *calls {
		for _, arg := range call {
			if arg == "set-option" {
				t.Fatalf("empty title must write nothing, got %v", *calls)
			}
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("empty title must print nothing, got %q", buf.String())
	}
}

// TestHookProjectNameTmuxFailureLeavesEverythingAsIs: the tmux option write is
// the gate — if tmux is unreachable the bus is never told either, so the
// session degrades to its pre-projection state rather than to a name that
// exists on one surface only.
func TestHookProjectNameTmuxFailureLeavesEverythingAsIs(t *testing.T) {
	tp := writeTranscript(t, "nfl-3")
	prev := tmuxenv.Run
	tmuxenv.Run = func(_ ...string) (string, error) { return "", fmt.Errorf("no server") }
	t.Cleanup(func() { tmuxenv.Run = prev })

	startCLITestDaemon(t)
	registerAliveViaDaemon(t, "muster-9", "/s", "$1", 200)

	var buf bytes.Buffer
	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", SessionCreated: 200, PaneID: "%5"}
	hookProjectName(c, harnessenv.CustomTitle(tp), &buf)

	if ag := agentRowForTest(t, "muster-9"); ag.Label != "" || ag.LabelManual {
		t.Fatalf("tmux failure must not push a bus label, got (%q, manual=%v)", ag.Label, ag.LabelManual)
	}
	if buf.Len() != 0 {
		t.Fatalf("tmux failure must print nothing, got %q", buf.String())
	}
}

// TestHookProjectNameWarnsOnCollision: another live agent in the SAME
// project already holds the name as a manual label → the projection still
// writes its own tuple (tuple-scoped, it cannot steal) but prints a warning
// naming the holder, so the resolver's coming ambiguity error isn't a
// surprise.
func TestHookProjectNameWarnsOnCollision(t *testing.T) {
	tp := writeTranscript(t, "nfl-3")
	prev := tmuxenv.Run
	tmuxenv.Run = func(_ ...string) (string, error) { return "", nil }
	t.Cleanup(func() { tmuxenv.Run = prev })

	startCLITestDaemon(t)
	// mine, and a same-project holder on a DIFFERENT tuple
	registerProjectViaDaemon(t, "muster-9", "/s", "$1", 200, "muster")
	registerProjectViaDaemon(t, "holder", "/s", "$2", 300, "muster")
	if _, err := callData("set_label", map[string]any{
		"socket_path": "/s", "session_id": "$2", "session_created": int64(300),
		"label": "nfl-3", "label_manual": true,
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", SessionCreated: 200, PaneID: "%5"}
	hookProjectName(c, harnessenv.CustomTitle(tp), &buf)
	if !strings.Contains(buf.String(), "also held by") || !strings.Contains(buf.String(), "holder") {
		t.Fatalf("expected collision warning naming the holder, got %q", buf.String())
	}
	// the holder keeps its label: a projection can never steal a name
	if h := agentRowForTest(t, "holder"); h.Label != "nfl-3" || !h.LabelManual {
		t.Fatalf("holder's label must be untouched, got (%q, manual=%v)", h.Label, h.LabelManual)
	}
	if mine := agentRowForTest(t, "muster-9"); mine.Label != "nfl-3" || !mine.LabelManual {
		t.Fatalf("my own tuple must still take the name, got (%q, manual=%v)", mine.Label, mine.LabelManual)
	}
}

// TestHookSessionStartProjectsNameOnFreshStart wires the projection through
// cmdHook itself: a fresh (non-resume) pane SessionStart whose payload names a
// custom-titled transcript registers AND projects, in one hook.
func TestHookSessionStartProjectsNameOnFreshStart(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	tp := writeTranscript(t, "nfl-3")
	t.Setenv("TMUX", "/tmp/sockP,1,0")
	t.Setenv("TMUX_PANE", "%3")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$1", "#{session_created}": "100", "#{session_name}": "proj-start",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	payload := fmt.Sprintf(`{"source":"startup","session_id":"uuid-7","transcript_path":%q}`, tp)
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}
	ag := agentRowForTest(t, "testdev-proj-start")
	if ag.Label != "nfl-3" || !ag.LabelManual {
		t.Fatalf("fresh SessionStart must project the name onto the bus, got (%q, manual=%v)", ag.Label, ag.LabelManual)
	}
	if !strings.Contains(buf.String(), `session name "nfl-3"`) {
		t.Fatalf("expected the context line, got %q", buf.String())
	}
}

// TestHookSessionStartProjectsNameOnResume is the spec's payoff: a resumed
// conversation reclaims its alias AND re-asserts its custom-title as the
// session's manual label on the NEW tuple — zero operator gestures.
func TestHookSessionStartProjectsNameOnResume(t *testing.T) {
	startTestDaemon(t)
	tp := writeTranscript(t, "nfl-3")
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("MUSTER_ALIAS", "")
	if _, err := callData("register_agent", map[string]any{
		"alias": "backend-2", "socket_path": "/tmp/sock", "session_id": "$OLD",
		"session_created": 111, "harness_session_id": "uuid-42",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("deregister_agent", map[string]any{"alias": "backend-2"}); err != nil {
		t.Fatal(err)
	}
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$NEW", "#{session_name}": "muster-3", "#{session_created}": "222",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	payload := fmt.Sprintf(`{"source":"resume","session_id":"uuid-42","transcript_path":%q}`, tp)
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}
	ag := agentRowForTest(t, "backend-2")
	if ag.SessionID != "$NEW" || ag.Label != "nfl-3" || !ag.LabelManual {
		t.Fatalf("resume must reclaim onto $NEW and re-assert the name, got %+v", ag)
	}
}

// TestHookSessionStartSiblingPaneDoesNotStompName is the ownership gate on
// the projection (same rule as the v0.7.1 hook-pane-ownership fix): the
// primary conversation owns pane %1 of $1 and has named itself
// "primary-name"; a SIBLING pane (%2) starts with its own custom-titled
// transcript. hookMayClaimIdentity already refuses the registration — the
// projection must refuse too, because SetSessionLabel rewrites every row on
// the tuple, so an ungated projection would durably rename the primary
// (labels are addresses: `send nfl-3` would then misroute).
func TestHookSessionStartSiblingPaneDoesNotStompName(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	tp := writeTranscript(t, "nfl-3")
	// Pre-existing row is seeded to match what hookAlias will derive from the
	// incoming session name "primary" below — the gate looks the row up by
	// that seeded alias, so the row must live there for ownership to resolve
	// correctly.
	if _, err := callData("register_agent", map[string]any{
		"alias": "testdev-primary", "socket_path": "/tmp/sockS", "session_id": "$1",
		"session_created": 100, "pane_id": "%1",
		"label": "primary-name", "label_manual": true,
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockS,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$1", "#{session_created}": "100",
		"#{session_name}": "primary",
		"#{pane_id}":      "%1", // the primary's pane is still alive
	})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	payload := fmt.Sprintf(`{"source":"startup","session_id":"uuid-9","transcript_path":%q}`, tp)
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}
	ag := agentRowForTest(t, "testdev-primary")
	if ag.Label != "primary-name" || !ag.LabelManual {
		t.Fatalf("a sibling pane stomped the session's name: label = (%q, manual=%v), want (primary-name, true)", ag.Label, ag.LabelManual)
	}
	if buf.Len() != 0 {
		t.Fatalf("a non-owning sibling must project nothing, got %q", buf.String())
	}
}

// writeTeammateTranscript writes a transcript whose teamName record sits at
// line 3 — the same fixture shape as harnessenv's TestIsTeammateDetectsMemberTranscript
// (a fleet member spawned into a pane of some primary's tmux session).
func writeTeammateTranscript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "member.jsonl")
	lines := []string{
		`{"type":"mode","mode":"normal","sessionId":"u1"}`,
		`{"type":"permission-mode","permissionMode":"auto","sessionId":"u1"}`,
		`{"parentUuid":null,"isSidechain":false,"teamName":"session-b41c21dd","agentName":"l5-mlb-measure","type":"user","message":{"role":"user","content":"go"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubTeammateGateTmux swaps tmuxenv.Run for a recorder seeded with values,
// keyed by the last arg (mirroring hookRun) — the tmux answers a teammate's
// SessionStart/SessionEnd/Stop would actually see firing from a sibling pane
// of the primary's own session. Once the gate is in place none of this is
// ever consulted, so the recorded calls come back empty; absent the gate,
// these are exactly the answers the pre-existing pane-anchored logic
// (hookCapture, hookMayClaimIdentity, hookStopWalked, ...) used on
// 2026-08-06 to resolve pane %2's tuple onto the primary's own session $1
// and stomp it.
func stubTeammateGateTmux(t *testing.T, values map[string]string) *[][]string {
	t.Helper()
	var calls [][]string
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if v, ok := values[args[len(args)-1]]; ok {
			return v, nil
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })
	return &calls
}

// registerGuardedPrimary registers the primary alias on session $1 (socket
// /s, incarnation 200) with NO stored pane_id — the unprovable-claim shape
// of the 2026-08-06 incident: hookMayClaimIdentity's pane check
// (`ag.PaneID == "" || ag.PaneID == c.PaneID`) treats an empty stored pane
// as "anyone may claim", exactly what let a teammate's sibling-pane
// SessionStart win the primary's own identity. It then gives the row a
// manual label via the set_label op — the row shape the incident stomped.
func registerGuardedPrimary(t *testing.T) {
	t.Helper()
	if _, err := callData("register_agent", map[string]any{
		"alias": "primary", "socket_path": "/s", "session_id": "$1",
		"session_created": int64(200),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("set_label", map[string]any{
		"socket_path": "/s", "session_id": "$1", "session_created": int64(200),
		"label": "primary-name", "label_manual": true,
	}); err != nil {
		t.Fatal(err)
	}
}

// teammateGateTmuxValues are the tmux stub answers shared by all three
// teammate-gate regression tests: the primary's own session identity, plus
// a non-zero @muster_inbox so Stop's cheap gate doesn't short-circuit
// before ever reaching the daemon — so that (absent the identity gate) the
// pre-existing logic would resolve pane %2 onto the primary's tuple exactly
// as the incident did.
var teammateGateTmuxValues = map[string]string{
	"#{session_id}":      "$1",
	"#{session_created}": "200",
	"#{session_name}":    "primary",
	"@muster_inbox":      "1",
}

// TestHookTeammateSessionStartTouchesNothing is the 2026-08-06 incident as a
// regression test: a primary registered on session $1 with no stored pane
// claim (unprovable, exactly like the incident row); a teammate's
// SessionStart fires from pane %2 of the SAME session with a teamName-bearing
// transcript. The row must be byte-for-byte untouched (pane, harness id,
// label) and the roster must gain no alias. Without the gate this actually
// stomps: hookMayClaimIdentity's empty-pane rule lets pane %2 win the
// register, and the upsert overwrites PaneID/HarnessSessionID and clears the
// manual label.
func TestHookTeammateSessionStartTouchesNothing(t *testing.T) {
	tp := writeTeammateTranscript(t)
	calls := stubTeammateGateTmux(t, teammateGateTmuxValues)

	startCLITestDaemon(t)
	registerGuardedPrimary(t)
	before := agentRowForTest(t, "primary")

	t.Setenv("TMUX", "/s,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")

	var buf bytes.Buffer
	payload := fmt.Sprintf(`{"source":"startup","session_id":"uuid-tm","transcript_path":%q}`, tp)
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}

	if buf.Len() != 0 {
		t.Fatalf("a teammate SessionStart must print nothing, got %q", buf.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("a teammate SessionStart must make no tmux calls, got %v", *calls)
	}
	agents := listAgentsForTest(t, "")
	if len(agents) != 1 {
		t.Fatalf("the roster must gain no alias, got %d rows: %+v", len(agents), agents)
	}
	after := agentRowForTest(t, "primary")
	if after.PaneID != before.PaneID || after.HarnessSessionID != before.HarnessSessionID ||
		after.Label != before.Label || after.LabelManual != before.LabelManual {
		t.Fatalf("the primary row must be untouched: before=%+v after=%+v", before, after)
	}
}

// TestHookTeammateSessionEndTombstonesNothing is TestHookTeammateSessionStartTouchesNothing's
// SessionEnd counterpart: the same teammate transcript firing SessionEnd from
// the sibling pane must not tombstone the primary's row.
func TestHookTeammateSessionEndTombstonesNothing(t *testing.T) {
	tp := writeTeammateTranscript(t)
	calls := stubTeammateGateTmux(t, teammateGateTmuxValues)

	startCLITestDaemon(t)
	registerGuardedPrimary(t)

	t.Setenv("TMUX", "/s,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")

	var buf bytes.Buffer
	payload := fmt.Sprintf(`{"session_id":"uuid-tm","transcript_path":%q}`, tp)
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}

	if buf.Len() != 0 {
		t.Fatalf("a teammate SessionEnd must print nothing, got %q", buf.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("a teammate SessionEnd must make no tmux calls, got %v", *calls)
	}
	if ag := agentRowForTest(t, "primary"); ag.Departed {
		t.Fatal("a teammate SessionEnd must not tombstone the primary")
	}
}

// TestHookTeammateStopEmitsNoWake is the Stop counterpart: with an unread
// message waiting for the primary, the same teammate transcript's Stop must
// print no wake decision AND must not run stampHarnessLinks, which — absent
// the gate — would stamp the teammate's harness session UUID onto the
// primary's link-less alias (hookStop never marks mail read itself, so
// unread count is not the vector here; the harness-id stamp is).
func TestHookTeammateStopEmitsNoWake(t *testing.T) {
	tp := writeTeammateTranscript(t)
	calls := stubTeammateGateTmux(t, teammateGateTmuxValues)

	startCLITestDaemon(t)
	registerGuardedPrimary(t)
	if _, err := callData("send_message", map[string]any{
		"from": "peer", "to_kind": "agent", "to_target": "primary", "subject": "s", "body": "b",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/s,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")

	var buf bytes.Buffer
	payload := fmt.Sprintf(`{"session_id":"uuid-tm","transcript_path":%q}`, tp)
	if err := cmdHook([]string{"Stop"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}

	if buf.Len() != 0 {
		t.Fatalf("a teammate Stop must emit no wake text, got %q", buf.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("a teammate Stop must make no tmux calls, got %v", *calls)
	}
	if ag := agentRowForTest(t, "primary"); ag.HarnessSessionID != "" {
		t.Fatalf("a teammate Stop must not stamp its harness session id onto the primary, got %q", ag.HarnessSessionID)
	}
}

// TestHookTeammateSessionStartArgvGate is the v0.10.1 live-acceptance FAILURE
// as a regression test (spec §3a): the teammate's SessionStart fires BEFORE
// its transcript file exists, so the transcript predicate fail-opens and the
// shipped gate let the theft through. The durable signal is process argv —
// the teammate's claude process carries --team-name from birth and the hook is
// its descendant. Same incident row as TestHookTeammateSessionStartTouchesNothing
// (a primary with no provable pane claim), but the payload points at a
// transcript path that does NOT exist yet.
func TestHookTeammateSessionStartArgvGate(t *testing.T) {
	calls := stubTeammateGateTmux(t, teammateGateTmuxValues)
	pinTeammateArgv(t, "claude --agent-id a1 --agent-name l5-mlb-measure --team-name session-b41c21dd --parent-session-id p9")

	startCLITestDaemon(t)
	registerGuardedPrimary(t)
	before := agentRowForTest(t, "primary")

	t.Setenv("TMUX", "/s,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")

	// The transcript the harness WILL write, but hasn't yet at hook-fire time.
	unwritten := filepath.Join(t.TempDir(), "not-yet.jsonl")
	var buf bytes.Buffer
	payload := fmt.Sprintf(`{"source":"startup","session_id":"uuid-tm","transcript_path":%q}`, unwritten)
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}

	if buf.Len() != 0 {
		t.Fatalf("a teammate SessionStart must print nothing, got %q", buf.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("a teammate SessionStart must make no tmux calls, got %v", *calls)
	}
	if agents := listAgentsForTest(t, ""); len(agents) != 1 {
		t.Fatalf("the roster must gain no alias, got %d rows: %+v", len(agents), agents)
	}
	after := agentRowForTest(t, "primary")
	if after.PaneID != before.PaneID || after.HarnessSessionID != before.HarnessSessionID ||
		after.Label != before.Label || after.LabelManual != before.LabelManual {
		t.Fatalf("the primary row must be untouched: before=%+v after=%+v", before, after)
	}
}

// TestHookMayClaimIdentityFailsClosedOnDaemonError is the second half of the
// v0.10.1 acceptance failure (spec §3a): the installer's LaunchAgent restart
// put the daemon mid-restart exactly when a foreign pane's SessionStart ran
// its ownership check, and hookGetAgent's dial error read as "no row" — i.e.
// "claimable". An unanswerable roster is not evidence of a free identity, so
// the gate now declines to write for this event; the next hook event re-runs
// the check once the daemon is back.
func TestHookMayClaimIdentityFailsClosedOnDaemonError(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir) // nothing is listening on this socket path
	// Skip client.dialOrSpawn's auto-start: under `go test` os.Executable() is
	// the test binary, so the fallback re-execs the whole suite (see
	// TestHookSessionStartBestEffortWhenDaemonUnreachable).
	t.Setenv("MUSTER_NO_AUTOSPAWN", "1")
	t.Setenv("MUSTER_ALIAS", "")

	c := tmuxenv.Capture{SocketPath: "/s", PaneID: "%2", SessionID: "$1", SessionName: "primary"}
	if hookMayClaimIdentity(c) {
		t.Fatal("an unreachable daemon must NOT read as a claimable identity")
	}
}

// TestHookMayClaimIdentityClaimsWhenRowAbsent is the fail-closed change's
// counterweight: a daemon that ANSWERS "no such alias" still means the name is
// free, and a fresh session must claim it exactly as before.
func TestHookMayClaimIdentityClaimsWhenRowAbsent(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_ALIAS", "")
	c := tmuxenv.Capture{SocketPath: "/s", PaneID: "%2", SessionID: "$1", SessionName: "nobody-here"}
	if !hookMayClaimIdentity(c) {
		t.Fatal("an absent row must stay claimable")
	}
}

// TestHookPrimaryMentioningTeamFlagStillRegisters is the false-positive guard
// on the argv signal: a PRIMARY whose command line merely mentions
// --team-name (a prompt quoting it, a wrapper's `sh -c`, this very spec under
// review) carries the token without being a teammate. The gate requires the
// launch PAIR (--team-name AND --agent-id on one ancestor), so this session
// must register and project exactly as any primary does — a false positive
// here would silently disable a primary's whole identity machinery.
func TestHookPrimaryMentioningTeamFlagStillRegisters(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	pinTeammateArgv(t, `claude -p why does the hook check --team-name here`)
	tp := writeTranscript(t, "nfl-3")
	t.Setenv("TMUX", "/tmp/sockQ,1,0")
	t.Setenv("TMUX_PANE", "%3")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$1", "#{session_created}": "100", "#{session_name}": "prim-start",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	payload := fmt.Sprintf(`{"source":"startup","session_id":"uuid-p","transcript_path":%q}`, tp)
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatal(err)
	}
	// The derived session name is seeded, like every other mint site.
	if ag := agentRowForTest(t, "testdev-prim-start"); ag.Label != "nfl-3" || !ag.LabelManual {
		t.Fatalf("a primary that merely mentions --team-name must still register and project, got (%q, manual=%v)", ag.Label, ag.LabelManual)
	}
}

// TestHookOutputKeepsTheFullAlias is the humancli-side half of the
// human/model alias split (see mcpserver.TestModelSurfacesKeepTheFullAlias
// for the full rationale). hookSessionStartResume's reconnect line is
// injected straight into an agent's context by the harness, so it is a MODEL
// surface: the alias it tells the model to call get_inbox with must be the
// full stored alias, never the device-stripped short form CLI/station
// surfaces render for a human. If this test fails because someone made hook
// text match the short display form elsewhere, that inconsistency was the
// point — a short alias here re-resolves against the READING machine's own
// device prefix, not the one that minted it, and silently lands on a
// different, real agent with nothing to report the misroute.
func TestHookOutputKeepsTheFullAlias(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")

	line := reconnectLine("personal-dotfiles/main", "revived", 3)
	if !strings.Contains(line, "personal-dotfiles/main") {
		t.Fatalf("hook line %q dropped the full alias", line)
	}
	if strings.Contains(line, "'dotfiles/main'") {
		t.Fatalf("hook line %q used the device-stripped short alias instead of the full one", line)
	}
}

// TestSessionAliasesForHookSeedsTheFallback: when session_aliases answers
// with nothing, the hook still needs SOMETHING to address, and it falls back
// to the tmux session name. That fallback flows into two places where a bare
// string is now permanently wrong: hookReason's "call get_inbox with alias
// '%s'" — a MODEL surface, which must carry the full stored alias — and
// hookStopOwnsAnyAlias's roster lookups, which match on the stored alias.
// "session name == alias" stopped being true when every minted alias started
// carrying this machine's name, so the fallback has to mint the same way
// registration does.
func TestSessionAliasesForHookSeedsTheFallback(t *testing.T) {
	startTestDaemon(t) // empty roster: session_aliases answers with nothing
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_name}": "dotfiles"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	got := sessionAliasesForHook("/tmp/sock", "$1", 100)
	want := []string{"testdev-dotfiles"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("fallback aliases = %v, want %v", got, want)
	}
}

// TestSessionAliasesForHookFallbackStaysEmptyWithNoSessionName: tmux
// unreachable means there is no name to seed, and device.Seed's empty-alias
// guard must keep the fallback from becoming a bare "testdev-" that resolves
// to nothing and reads as a real alias in the injected hook text.
func TestSessionAliasesForHookFallbackStaysEmptyWithNoSessionName(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
	prev := tmuxenv.Run
	tmuxenv.Run = func(_ ...string) (string, error) { return "", errors.New("no tmux") }
	t.Cleanup(func() { tmuxenv.Run = prev })

	got := sessionAliasesForHook("/tmp/sock", "$1", 100)
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("fallback aliases = %q, want a single empty string (nothing to seed)", got)
	}
}
