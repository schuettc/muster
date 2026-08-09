package humancli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// TestBecomeClaimsSingleAliasSession: the happy path — one live alias on
// this session; become claims the new name and reports the trade.
func TestBecomeClaimsSingleAliasSession(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$1", "#{session_name}": "muster-2", "#{session_created}": "111",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })
	if _, err := callData("register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$1", "session_created": 111,
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Dispatch([]string{"become", "alias-routing"}, &buf); err != nil {
		t.Fatalf("become: %v", err)
	}
	if !strings.Contains(buf.String(), "you are now 'alias-routing' (was 'muster-2')") {
		t.Fatalf("output = %q", buf.String())
	}
	ag, ok, _ := hookGetAgent("alias-routing")
	if !ok || ag.Departed || ag.SessionID != "$1" {
		t.Fatalf("claimed row = %+v (ok=%v)", ag, ok)
	}
}

// TestBecomeRequiresFromWhenSplit: two live aliases on one session — never
// guess which identity is being claimed over.
func TestBecomeRequiresFromWhenSplit(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })
	for _, a := range []string{"muster-2", "cost-audit"} {
		if _, err := callData("register_agent", map[string]any{
			"alias": a, "socket_path": "/tmp/sock", "session_id": "$1", "session_created": 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	err := Dispatch([]string{"become", "alias-routing"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "--from") {
		t.Fatalf("want --from requirement error, got %v (out %q)", err, buf.String())
	}
	if err := Dispatch([]string{"become", "alias-routing", "--from", "cost-audit"}, &buf); err != nil {
		t.Fatalf("explicit --from: %v", err)
	}
}

// TestBecomeRejectsEmptyAlias is finding F4: `muster become ""` (or a
// whitespace-only name) must error before dialing the daemon at all,
// mirroring cmdRegister's empty-alias rejection — a blank claimed name would
// round-trip into a row nothing could ever address again.
func TestBecomeRejectsEmptyAlias(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })
	if _, err := callData("register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$1", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"", "   "} {
		var buf bytes.Buffer
		err := Dispatch([]string{"become", name}, &buf)
		if err == nil {
			t.Fatalf("become %q: want an error, got none (out %q)", name, buf.String())
		}
		if buf.Len() != 0 {
			t.Fatalf("become %q: must not print anything before erroring, got %q", name, buf.String())
		}
	}

	// No row must have been created for either rejected name.
	if _, found, _ := hookGetAgent(""); found {
		t.Fatal("empty alias must never reach the daemon")
	}
	if _, found, _ := hookGetAgent("   "); found {
		t.Fatal("whitespace-only alias must never reach the daemon")
	}
}

// TestBecomeRenamesLiveClaudePane: prefix-T delegates the whole rename to
// become (dotfiles session-identity spec 2026-08-08), so a successful claim
// must also type "/rename <name>" into this session's registered live
// Claude pane — the tmux name, the bus alias, and the harness session name
// move as ONE identity. The injected name is the FULL claimed alias
// (<project>/<work>), never a bare work segment: an operator may be
// troubleshooting "error" in several projects at once.
func TestBecomeRenamesLiveClaudePane(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%5")
	if _, err := callData("register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$1",
		"pane_id": "%5", "model_type": "claude", "session_created": 1700000000,
	}); err != nil {
		t.Fatal(err)
	}

	var sent [][]string
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		last := args[len(args)-1]
		switch last {
		case "#{session_id}":
			return "$1", nil
		case "#{pane_id}":
			return "%5", nil // pane-alive probe answers: alive
		case "#{session_created}":
			return "1700000000", nil // matches the row's recorded incarnation
		}
		if len(args) > 2 && args[2] == "send-keys" {
			sent = append(sent, append([]string(nil), args...))
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := Dispatch([]string{"become", "dotfiles/error"}, &buf); err != nil {
		t.Fatalf("become: %v", err)
	}
	if len(sent) != 2 {
		t.Fatalf("expected /rename type + Enter submit, got %v", sent)
	}
	if got := sent[0][len(sent[0])-1]; got != "/rename dotfiles/error" {
		t.Fatalf("typed %q, want %q", got, "/rename dotfiles/error")
	}
	if sent[1][len(sent[1])-1] != "Enter" {
		t.Fatalf("expected Enter submit, got %v", sent[1])
	}
	if !strings.Contains(buf.String(), "renamed claude session") {
		t.Fatalf("expected rename confirmation in output, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "you are now 'dotfiles/error'") {
		t.Fatalf("expected claim summary in output, got %q", buf.String())
	}
}

// TestBecomeNoInjectSkipsRename: --no-inject claims the alias but never
// touches the pane — for callers whose name ALREADY came from the harness
// side (the statusline promoting a name that originated from a /rename the
// agent itself typed): re-injecting it would loop the same text back into a
// live pane. This is the flag statusline.sh has called since the
// session-identity plan; before this test the flag didn't parse at all and
// every such call silently failed, stranding the bus alias on rename.
func TestBecomeNoInjectSkipsRename(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%5")
	if _, err := callData("register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$1",
		"pane_id": "%5", "model_type": "claude", "session_created": 1700000000,
	}); err != nil {
		t.Fatal(err)
	}

	var sent [][]string
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		last := args[len(args)-1]
		switch last {
		case "#{session_id}":
			return "$1", nil
		case "#{pane_id}":
			return "%5", nil
		case "#{session_created}":
			return "1700000000", nil
		}
		if len(args) > 2 && args[2] == "send-keys" {
			sent = append(sent, append([]string(nil), args...))
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := Dispatch([]string{"become", "--no-inject", "dotfiles/error"}, &buf); err != nil {
		t.Fatalf("become --no-inject: %v", err)
	}
	if len(sent) != 0 {
		t.Fatalf("--no-inject must never type into the pane, got %v", sent)
	}
	if ag, ok, _ := hookGetAgent("dotfiles/error"); !ok || ag.Departed {
		t.Fatalf("claim must still land with --no-inject, got %+v (ok=%v)", ag, ok)
	}
}

// TestBecomeToExistsErrorHasNoPrefixStutter is finding F3, live rig:
// `muster become` on an already-claimed name used to print "become: become:
// alias ..." — the daemon's own error text for this guard baked in a
// "become: " prefix, and callData (the ONE place that renders "<op>:
// <daemon error>" for every op) added a second one on top. cmd/muster's
// main() then prints "muster: " + err.Error() verbatim, so the doubled
// prefix reached the operator's terminal unchanged. The error text
// Dispatch returns here is exactly what main() prints after "muster: ", so
// asserting on err.Error() catches the stutter at its source.
func TestBecomeToExistsErrorHasNoPrefixStutter(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })
	if _, err := callData("register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$1", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{"alias": "taken"}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	err := Dispatch([]string{"become", "taken"}, &buf)
	if err == nil {
		t.Fatal("want an error claiming an already-existing alias")
	}
	if n := strings.Count(err.Error(), "become:"); n != 1 {
		t.Fatalf("error must carry exactly one 'become:' prefix, got %d: %q", n, err.Error())
	}
	if strings.Contains(err.Error(), "become: become:") {
		t.Fatalf("prefix stutter survived: %q", err.Error())
	}
}
