package tmuxenv

import (
	"os"
	"path/filepath"
	"testing"
)

// contains reports whether want is present anywhere in args.
func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// containsSuffix reports whether any arg's basename equals suffix — used to
// match the "-S <path>" socket argument by its socket-file basename rather
// than a full temp-dir path.
func containsSuffix(args []string, suffix string) bool {
	for _, a := range args {
		if filepath.Base(a) == suffix {
			return true
		}
	}
	return false
}

// TestAncestorArgvContainsAll covers the teammate signal the hook gate leans
// on (teammate-identity-refusal spec §3a): a fleet teammate's claude process is
// launched with `--agent-id <x> --team-name <y>` and the hook is its
// descendant, so the pair is findable by the same ancestor walk
// CaptureFromAncestry does. All tokens must sit on ONE ancestor's argv, each
// matched as a discrete token (never a substring), and every failure mode
// (empty chain, unreadable argv, no tokens) comes back false — a hook must
// never block a session.
func TestAncestorArgvContainsAll(t *testing.T) {
	prevAnc, prevArgv := AncestorPIDs, ProcessArgv
	t.Cleanup(func() { AncestorPIDs, ProcessArgv = prevAnc, prevArgv })

	argv := map[int]string{}
	AncestorPIDs = func() []int { return []int{10, 20, 30} }
	ProcessArgv = func(pid int) string { return argv[pid] }
	teammate := func() bool { return AncestorArgvContainsAll("--team-name", "--agent-id") }

	// The live shape (verified 2026-08-06): the hook process, its shell, then
	// the teammate's claude process carrying both flags.
	argv = map[int]string{
		10: "muster hook SessionStart claude",
		20: "-zsh",
		30: "claude --agent-id a1 --agent-name l5-mlb-measure --team-name session-b41c21dd --parent-session-id p9",
	}
	if !teammate() {
		t.Fatal("an ancestor launched with --agent-id and --team-name must read as a teammate")
	}

	// A primary's chain: same walk, no marker anywhere.
	argv = map[int]string{
		10: "muster hook SessionStart claude",
		20: "-zsh",
		30: "claude",
	}
	if teammate() {
		t.Fatal("a chain with no teammate flags must not read as a teammate")
	}

	// The false-positive path the second token exists to close: a primary that
	// merely MENTIONS the flag — in a prompt, a wrapper's `sh -c`, a grep of
	// this very spec — carries the standalone token without being a teammate.
	// One token alone is not a launch shape; the PAIR is.
	argv = map[int]string{30: `claude -p why does the hook check --team-name here`}
	if teammate() {
		t.Fatal("a standalone --team-name token in a primary's argv must not read as a teammate")
	}
	argv = map[int]string{30: "claude --agent-id a1"}
	if teammate() {
		t.Fatal("--agent-id alone must not read as a teammate")
	}

	// Both tokens present, but on DIFFERENT ancestors: not one process's launch
	// shape, so not a teammate.
	argv = map[int]string{
		20: "sh -c wrapper --agent-id a1",
		30: "claude --team-name session-b41c21dd",
	}
	if teammate() {
		t.Fatal("tokens split across two ancestors must not read as a teammate")
	}

	// Token discipline: a flag that merely CONTAINS a token is not it.
	argv = map[int]string{30: "claude --team-names-file /tmp/x --agent-identity z"}
	if teammate() {
		t.Fatal("--team-names-file/--agent-identity must not match the teammate tokens")
	}

	// The =-joined spelling is still a discrete token for the same flag.
	argv = map[int]string{30: "claude --agent-id=a1 --team-name=session-b41c21dd"}
	if !teammate() {
		t.Fatal("--flag=<x> must read as the same flag")
	}

	// Fail-open: an unreadable argv (ps failed) and an empty chain (walk
	// failed) both mean "not a teammate", never a blocked session.
	argv = map[int]string{}
	if teammate() {
		t.Fatal("unreadable argv must fail open to not-a-teammate")
	}
	AncestorPIDs = func() []int { return nil }
	if teammate() {
		t.Fatal("an empty ancestor chain must fail open to not-a-teammate")
	}
	if AncestorArgvContainsAll() {
		t.Fatal("no tokens must never match")
	}
	if AncestorArgvContainsAll("--team-name", "") {
		t.Fatal("an empty token must never match")
	}
}

// TestCaptureFromAncestryMatchesPanePID: the walk must find the pane whose
// #{pane_pid} is one of this process's ancestors, capture the full tuple
// from THAT socket, and come back empty (fail-safe) when no pane matches —
// never a guess.
func TestCaptureFromAncestryMatchesPanePID(t *testing.T) {
	prevRun, prevAnc, prevDir := Run, AncestorPIDs, SocketDir
	t.Cleanup(func() { Run, AncestorPIDs, SocketDir = prevRun, prevAnc, prevDir })

	AncestorPIDs = func() []int { return []int{999, 4242, 1} }

	Run = func(args ...string) (string, error) {
		// list-panes across sockets: only the proj-muster socket has our pane.
		switch {
		case contains(args, "list-panes") && containsSuffix(args, "proj-muster"):
			return "4242\t%7\t$3\tmuster-2\t555", nil
		case contains(args, "list-panes"):
			return "100\t%1\t$1\tother\t111", nil
		default:
			return "", nil
		}
	}
	// Socket enumeration: SocketDir points at a temp dir holding two fake
	// socket files so CaptureFromAncestry's glob finds both "servers".
	dir := t.TempDir()
	for _, name := range []string{"proj-other", "proj-muster"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	SocketDir = func() string { return dir }

	c := CaptureFromAncestry()
	if c.SocketPath == "" || c.PaneID != "%7" || c.SessionID != "$3" || c.SessionCreated != 555 {
		t.Fatalf("capture = %+v, want pane %%7 on $3 created 555", c)
	}

	AncestorPIDs = func() []int { return []int{999, 1} } // no pane pid in chain
	if c := CaptureFromAncestry(); c.SocketPath != "" || c.PaneID != "" {
		t.Fatalf("no-match capture = %+v, want zero value (fail-safe)", c)
	}
}
