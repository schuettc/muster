package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// stubAncestryMatch wires AncestorPIDs/SocketDir/Run so the ancestry walk
// resolves pane %7 / $3 / muster-2 / created 555 on a fake proj-muster
// socket — the same fixture tmuxenv's own ancestry test uses.
func stubAncestryMatch(t *testing.T) {
	t.Helper()
	prevRun, prevAnc, prevDir := tmuxenv.Run, tmuxenv.AncestorPIDs, tmuxenv.SocketDir
	t.Cleanup(func() { tmuxenv.Run, tmuxenv.AncestorPIDs, tmuxenv.SocketDir = prevRun, prevAnc, prevDir })

	tmuxenv.AncestorPIDs = func() []int { return []int{999, 4242, 1} }
	tmuxenv.Run = func(args ...string) (string, error) {
		for _, a := range args {
			if filepath.Base(a) == "proj-muster" {
				for _, x := range args {
					if x == "list-panes" {
						return "4242\t%7\t$3\tmuster-2\t555", nil
					}
				}
			}
		}
		return "", nil
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "proj-muster"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tmuxenv.SocketDir = func() string { return dir }
}

// TestWhereamiPrintsResolvedTuple: the verb is the walk's CLI face, for
// muster's own hooks and operators (dotfiles keeps its own walk — the two
// share a behavioral contract, not a binary; thread 149).
func TestWhereamiPrintsResolvedTuple(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	stubAncestryMatch(t)

	var buf bytes.Buffer
	if err := Dispatch([]string{"whereami"}, &buf); err != nil {
		t.Fatalf("whereami: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"session_id=$3", "pane=%7", "session_name=muster-2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("whereami output missing %q:\n%s", want, out)
		}
	}
}

// TestWhereamiFailsWhenUnresolvable: no env, no ancestry match → empty
// stdout and a non-nil error (the CLI maps it to a nonzero exit).
func TestWhereamiFailsWhenUnresolvable(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	prevRun, prevAnc, prevDir := tmuxenv.Run, tmuxenv.AncestorPIDs, tmuxenv.SocketDir
	t.Cleanup(func() { tmuxenv.Run, tmuxenv.AncestorPIDs, tmuxenv.SocketDir = prevRun, prevAnc, prevDir })
	tmuxenv.AncestorPIDs = func() []int { return []int{999, 1} }
	tmuxenv.Run = func(_ ...string) (string, error) { return "", nil }
	tmuxenv.SocketDir = func() string { return t.TempDir() }

	var buf bytes.Buffer
	if err := Dispatch([]string{"whereami"}, &buf); err == nil {
		t.Fatalf("expected an error when no pane resolves, got output %q", buf.String())
	}
}
