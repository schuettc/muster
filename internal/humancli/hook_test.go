package humancli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

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

func TestHookStopNoTmux(t *testing.T) {
	t.Setenv("TMUX", "")
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
		"socket_path": "/tmp/sock", "session_id": "$1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "role": "peer", "model_type": "claude",
		"socket_path": "/tmp/other", "session_id": "$2",
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
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "3", "#{session_id}": "$1"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	res := runHook(t)
	if !strings.Contains(res.Reason, "alias 'backend'") || !strings.Contains(res.Reason, "1 unread muster thread(s)") {
		t.Fatalf("reason missing expected fields: %q", res.Reason)
	}
	if strings.Contains(res.Reason, "needing action") {
		t.Fatalf("no action-requested thread: reason must not mention action count: %q", res.Reason)
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
			"socket_path": "/tmp/sock2", "session_id": "$5",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "role": "peer", "model_type": "claude",
		"socket_path": "/tmp/other2", "session_id": "$6",
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
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "9", "#{session_id}": "$5"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	res := runHook(t)
	if !strings.Contains(res.Reason, "Your muster aliases are 'alpha', 'zeta'") {
		t.Fatalf("reason must list both aliases sorted: %q", res.Reason)
	}
	if !strings.Contains(res.Reason, "For EACH alias call get_inbox") {
		t.Fatalf("reason must carry the for-each drain instruction: %q", res.Reason)
	}
}

// TestHookStopActionCountAppearsWhenActionable: an action-requested unread
// thread must append ", N needing action" to the count line (spec §2).
func TestHookStopActionCountAppearsWhenActionable(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "worker", "role": "peer", "model_type": "claude",
		"socket_path": "/tmp/sock3", "session_id": "$7",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "role": "peer", "model_type": "claude",
		"socket_path": "/tmp/other3", "session_id": "$8",
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
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "1", "#{session_id}": "$7"})
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
		"socket_path": "/tmp/sock4", "session_id": "$9",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "role": "peer", "model_type": "claude",
		"socket_path": "/tmp/other4", "session_id": "$10",
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
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "1", "#{session_id}": "$9"})
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
// today's single session-name wording (spec §3).
func TestHookStopSessionAliasesFailureFallsBackToSessionName(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sockY,1,0")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "2", "#{session_name}": "fallback-session"})
	t.Cleanup(func() { tmuxenv.Run = prev })

	res := runHook(t)
	if !strings.Contains(res.Reason, "alias 'fallback-session'") {
		t.Fatalf("reason must fall back to the session-name wording: %q", res.Reason)
	}
	if strings.Contains(res.Reason, "aliases are") {
		t.Fatalf("fallback must use singular wording, not the multi-alias form: %q", res.Reason)
	}
}

func TestHookSessionStartAndEnd(t *testing.T) {
	startTestDaemon(t)
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
		if a.Alias == "muster-hook" && a.ModelType == "codex" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected muster-hook registered via SessionStart hook: %+v", agents)
	}

	buf.Reset()
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("SessionEnd: %v", err)
	}
	agents = listAgentsForTest(t, "")
	found = false
	for _, a := range agents {
		if a.Alias == "muster-hook" {
			found = true
			if !a.Departed {
				t.Fatalf("expected muster-hook tombstoned (Departed=true) via SessionEnd hook, got %+v", a)
			}
		}
	}
	if !found {
		t.Fatalf("expected muster-hook's row to SURVIVE SessionEnd (tombstoned, not deleted): %+v", agents)
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
		"socket_path": "/tmp/sock_stale", "session_id": "$99",
	}); err != nil {
		t.Fatal(err)
	}
	// Deliberately send NO messages, so session_unread returns total=0.

	t.Setenv("TMUX", "/tmp/sock_stale,1,0")
	prev := tmuxenv.Run
	// Stub tmux to report stale @muster_inbox=2 but matching session_id.
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "2", "#{session_id}": "$99"})
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
		"alias": "start-owner", "socket_path": "/tmp/sockA", "session_id": "$1", "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockA,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}":   "$1",
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
			if c.preRegister {
				if _, err := callData("register_agent", map[string]any{
					"alias": "claim-me", "socket_path": c.socketPath, "session_id": c.sessionID, "pane_id": c.storedPane,
				}); err != nil {
					t.Fatal(err)
				}
			}

			t.Setenv("TMUX", "/tmp/sockC,1,0")
			t.Setenv("TMUX_PANE", "%2")
			t.Setenv("MUSTER_ALIAS", "")
			prev := tmuxenv.Run
			tmuxenv.Run = hookRun(map[string]string{
				"#{session_id}":   "$1",
				"#{session_name}": "claim-me",
				"#{pane_id}":      c.paneAlive,
			})
			t.Cleanup(func() { tmuxenv.Run = prev })

			var buf bytes.Buffer
			if err := cmdHook([]string{"SessionStart"}, strings.NewReader(""), &buf); err != nil {
				t.Fatalf("SessionStart: %v", err)
			}
			pane, found := hookAgentPane(t, "claim-me")
			if !found || pane != "%2" {
				t.Fatalf("expected claim-me claimed to my pane '%%2', got pane=%q found=%v", pane, found)
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
		"alias": "stop-nonowner", "socket_path": "/tmp/sockB", "session_id": "$5", "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "socket_path": "/tmp/otherB", "session_id": "$6",
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
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "3", "#{session_id}": "$5"})
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
		"alias": "stop-owner", "socket_path": "/tmp/sockD", "session_id": "$7", "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "socket_path": "/tmp/otherD", "session_id": "$8",
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
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "3", "#{session_id}": "$7"})
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
		"alias": "stop-unknown-pane", "socket_path": "/tmp/sockE", "session_id": "$9",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "other", "socket_path": "/tmp/otherE", "session_id": "$10",
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
	tmuxenv.Run = hookRun(map[string]string{"@muster_inbox": "3", "#{session_id}": "$9"})
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
		"alias": "end-nonowner", "socket_path": "/tmp/sockF", "session_id": "$1", "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockF,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1", "#{session_name}": "end-nonowner"})
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

// TestHookSessionEndDeregistersForOwner is the mirror: my pane owns the
// registration, so SessionEnd must deregister (tombstone) it as today.
func TestHookSessionEndDeregistersForOwner(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "end-owner", "socket_path": "/tmp/sockG", "session_id": "$1", "pane_id": "%2",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockG,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1", "#{session_name}": "end-owner"})
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
		"alias": "end-foreign-tuple", "socket_path": "/tmp/sockOLD", "session_id": "$OLD", "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", "/tmp/sockH,1,0")
	t.Setenv("TMUX_PANE", "%2")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$NEW", "#{session_name}": "end-foreign-tuple"})
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

func TestHookSessionStartBestEffortWhenDaemonUnreachable(t *testing.T) {
	// No test daemon started, and no tmux identity to fall back on: cmdRegister
	// will fail (can't determine alias / can't reach daemon), but the hook must
	// swallow that and still return nil — a hook must never block a session.
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("MUSTER_ALIAS", "")
	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("hook must never return an error, got %v", err)
	}
	if err := cmdHook([]string{"SessionEnd"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("hook must never return an error, got %v", err)
	}
}
