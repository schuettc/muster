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

// TestAncestorArgvContains covers the teammate signal the hook gate leans on
// (teammate-identity-refusal spec §3a): a fleet teammate's claude process is
// launched with `--team-name <x>` and the hook is its descendant, so the flag
// is findable by the same ancestor walk CaptureFromAncestry does. Matching is
// per argv TOKEN, never a substring — `--team-names-file` must not read as a
// team marker — and every failure mode (empty chain, unreadable argv) comes
// back false, because a hook must never block a session.
func TestAncestorArgvContains(t *testing.T) {
	prevAnc, prevArgv := AncestorPIDs, ProcessArgv
	t.Cleanup(func() { AncestorPIDs, ProcessArgv = prevAnc, prevArgv })

	argv := map[int]string{}
	AncestorPIDs = func() []int { return []int{10, 20, 30} }
	ProcessArgv = func(pid int) string { return argv[pid] }

	// The live shape (verified 2026-08-06): the hook process, its shell, then
	// the teammate's claude process carrying the flag.
	argv = map[int]string{
		10: "muster hook SessionStart claude",
		20: "-zsh",
		30: "claude --agent-id a1 --agent-name l5-mlb-measure --team-name session-b41c21dd --parent-session-id p9",
	}
	if !AncestorArgvContains("--team-name") {
		t.Fatal("an ancestor launched with --team-name must read as a teammate")
	}

	// A primary's chain: same walk, no marker anywhere.
	argv = map[int]string{
		10: "muster hook SessionStart claude",
		20: "-zsh",
		30: "claude",
	}
	if AncestorArgvContains("--team-name") {
		t.Fatal("a chain with no --team-name must not read as a teammate")
	}

	// Token discipline: a flag that merely CONTAINS the token is not it.
	argv = map[int]string{30: "claude --team-names-file /tmp/x --no-team-name-check"}
	if AncestorArgvContains("--team-name") {
		t.Fatal("--team-names-file must not match the --team-name token")
	}

	// The =-joined spelling is still a discrete token for the same flag.
	argv = map[int]string{30: "claude --team-name=session-b41c21dd"}
	if !AncestorArgvContains("--team-name") {
		t.Fatal("--team-name=<x> must read as the same flag")
	}

	// Fail-open: an unreadable argv (ps failed) and an empty chain (walk
	// failed) both mean "not a teammate", never a blocked session.
	argv = map[int]string{}
	if AncestorArgvContains("--team-name") {
		t.Fatal("unreadable argv must fail open to not-a-teammate")
	}
	AncestorPIDs = func() []int { return nil }
	if AncestorArgvContains("--team-name") {
		t.Fatal("an empty ancestor chain must fail open to not-a-teammate")
	}
	if AncestorArgvContains("") {
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
