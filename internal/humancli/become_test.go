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
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
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
	// The confirmation and the typed-into-pane name stay short; only the
	// daemon claim underneath is seeded.
	if !strings.Contains(buf.String(), "you are now 'alias-routing' (was 'muster-2')") {
		t.Fatalf("output = %q", buf.String())
	}
	ag, ok, _ := hookGetAgent("testdev-alias-routing")
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
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
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
	// The daemon claim is seeded even though the pane never sees the prefix.
	if ag, ok, _ := hookGetAgent("testdev-dotfiles/error"); !ok || ag.Departed {
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
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
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
	// Pre-existing row is seeded to match what cmdBecome will seed "taken" to
	// below, so the claim genuinely collides.
	if _, err := callData("register_agent", map[string]any{"alias": "testdev-taken"}); err != nil {
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

// TestBecomeFromExpandsLocalFirst covers become.go's --from bypass site:
// this session owns a live seeded alias, and a foreign live alias of the
// SAME short name also happens to exist on the bus (registered directly,
// unseeded, standing in for another device's bare row). Typing the short
// name into --from must claim from THIS session's own live alias, never the
// foreign one — becomeLiveAliases already restricts candidates to this
// session's tuple, so this proves the short name expands to the row become
// is actually allowed to touch, not a same-named row elsewhere on the bus.
func TestBecomeFromExpandsLocalFirst(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })
	if _, err := callData("register_agent", map[string]any{
		"alias": "personal-cost-audit", "socket_path": "/tmp/sock", "session_id": "$1", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}
	// A foreign row with the bare short name, on a DIFFERENT session tuple —
	// not live on this session, so becomeLiveAliases would never offer it,
	// but it must also not be what --from "cost-audit" silently expands to.
	if _, err := callData("register_agent", map[string]any{
		"alias": "cost-audit", "socket_path": "/tmp/othersock", "session_id": "$9", "session_created": 100,
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Dispatch([]string{"become", "alias-routing", "--from", "cost-audit"}, &buf); err != nil {
		t.Fatalf("become --from cost-audit: %v", err)
	}
	if !strings.Contains(buf.String(), "was 'cost-audit'") {
		t.Fatalf("output = %q", buf.String())
	}
	// The LOCAL row (personal-cost-audit) must be the one retired by become
	// (superseded); the foreign row must be untouched.
	local, ok, _ := hookGetAgent("personal-cost-audit")
	if !ok || local.SupersededBy == "" {
		t.Fatalf("local row must be retired by the claim, got %+v (ok=%v)", local, ok)
	}
	foreign, ok, _ := hookGetAgent("cost-audit")
	if !ok || foreign.SupersededBy != "" || foreign.Departed {
		t.Fatalf("foreign row must be untouched, got %+v (ok=%v)", foreign, ok)
	}
}

// TestBecomeFromRefusesAmbiguousLegacyTwin covers the review finding: this
// session holds BOTH a legacy unprefixed row ("dotfiles") and its seeded
// twin ("personal-dotfiles"), both live on the SAME session tuple.
// `--from dotfiles` must not silently expand to the twin and retire it while
// reporting "was 'dotfiles'" — the operator named the legacy row, and
// expandAlias's local-first bias would pick the OTHER one. become must
// refuse instead, naming both candidates, and must retire neither.
func TestBecomeFromRefusesAmbiguousLegacyTwin(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1", "#{session_created}": "100"})
	t.Cleanup(func() { tmuxenv.Run = prev })
	for _, a := range []string{"dotfiles", "personal-dotfiles"} {
		if _, err := callData("register_agent", map[string]any{
			"alias": a, "socket_path": "/tmp/sock", "session_id": "$1", "session_created": 100,
		}); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	err := Dispatch([]string{"become", "alias-routing", "--from", "dotfiles"}, &buf)
	if err == nil {
		t.Fatalf("want an ambiguity refusal, got none (out %q)", buf.String())
	}
	if !strings.Contains(err.Error(), "dotfiles") || !strings.Contains(err.Error(), "personal-dotfiles") {
		t.Fatalf("error must name both candidates, got %q", err.Error())
	}
	if buf.Len() != 0 {
		t.Fatalf("must not print anything before refusing, got %q", buf.String())
	}

	// Neither row was touched: no claim, no supersession, no departure.
	legacy, ok, _ := hookGetAgent("dotfiles")
	if !ok || legacy.SupersededBy != "" || legacy.Departed {
		t.Fatalf("legacy row must be untouched, got %+v (ok=%v)", legacy, ok)
	}
	twin, ok, _ := hookGetAgent("personal-dotfiles")
	if !ok || twin.SupersededBy != "" || twin.Departed {
		t.Fatalf("twin row must be untouched, got %+v (ok=%v)", twin, ok)
	}
}

// TestBecomeSeedsTheClaimNotTheInjectedName pins the split inside become: the
// stored alias carries the device prefix, while the name typed into the pane
// and reported to the operator stays short. Injecting the seeded name would
// put the prefix straight back into the tmux session name and the title bar.
func TestBecomeSeedsTheClaimNotTheInjectedName(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	if got, want := seedAlias("galley/design"), "personal-galley/design"; got != want {
		t.Fatalf("claim = %q, want %q", got, want)
	}
	// A full alias pasted back from the other machine claims the same row.
	if got, want := seedAlias("personal-galley/design"), "personal-galley/design"; got != want {
		t.Fatalf("re-seeded claim = %q, want %q", got, want)
	}
}

// TestBecomeInjectsTheStrippedNameForAFullAlias is the round trip
// device.Seed's idempotence guard exists to support: an operator reads a full
// alias off ANOTHER machine (where it renders unstripped) and pastes it back
// here. The claim underneath is identical either way — that is what
// idempotence buys — but the name typed into the pane is a human surface, and
// injecting the pasted prefix puts the device name straight back into the
// title bar this design clears. `become galley/design` and
// `become testdev-galley/design` must therefore type the SAME thing.
func TestBecomeInjectsTheStrippedNameForAFullAlias(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "testdev")
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
	if err := Dispatch([]string{"become", "testdev-galley/design"}, &buf); err != nil {
		t.Fatalf("become: %v", err)
	}
	if len(sent) != 2 {
		t.Fatalf("expected /rename type + Enter submit, got %v", sent)
	}
	if got := sent[0][len(sent[0])-1]; got != "/rename galley/design" {
		t.Fatalf("typed %q, want %q — the device prefix must not be re-injected", got, "/rename galley/design")
	}
	// The claim itself is the seeded form, unchanged by the paste.
	if ag, ok, _ := hookGetAgent("testdev-galley/design"); !ok || ag.Departed {
		t.Fatalf("claimed row = %+v (ok=%v)", ag, ok)
	}
	if !strings.Contains(buf.String(), "you are now 'galley/design'") {
		t.Fatalf("expected the stripped claim summary, got %q", buf.String())
	}
}
