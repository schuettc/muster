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
