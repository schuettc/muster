package cli

import (
	"os"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// TestMain pins the ancestor-argv seam away for the whole package. cmdHook's
// teammate gate asks tmuxenv.AncestorArgvContainsAll whether an ANCESTOR of
// this process was launched as a teammate — and this test binary is routinely
// run by a fleet teammate (`go test` under an agent's shell), which would make
// every cmdHook test a silent no-op and fail the suite only on the machines
// where it matters (measured 2026-08-06: 25 tests fail without this pin when
// the suite runs under a teammate). Tests that mean to exercise the gate opt
// in explicitly via pinTeammateArgv.
func TestMain(m *testing.M) {
	tmuxenv.ProcessArgv = func(int) string { return "" }
	os.Exit(m.Run())
}

// pinTeammateArgv pins the ancestry-argv walk to a single ancestor with the
// given command line — the seam pair (AncestorPIDs + ProcessArgv) the real
// walk reads, stubbed exactly as tmuxenv's own ancestry tests stub it.
func pinTeammateArgv(t *testing.T, argv string) {
	t.Helper()
	prevAnc, prevArgv := tmuxenv.AncestorPIDs, tmuxenv.ProcessArgv
	tmuxenv.AncestorPIDs = func() []int { return []int{4242} }
	tmuxenv.ProcessArgv = func(int) string { return argv }
	t.Cleanup(func() { tmuxenv.AncestorPIDs, tmuxenv.ProcessArgv = prevAnc, prevArgv })
}
