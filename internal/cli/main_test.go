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
//
// It also clears the harness identity (see unsetHarnessIdentity): tests that
// need an identity set it with t.Setenv.
func TestMain(m *testing.M) {
	tmuxenv.ProcessArgv = func(int) string { return "" }
	unsetHarnessIdentity()
	os.Exit(m.Run())
}

// unsetHarnessIdentity clears the variables tools-common/harness resolves a
// session from. `go test` on a dev machine runs inside a Claude or pi session
// (or a pi-claude-bridge child, which also carries AGENT_SESSION_CHILD=1), and
// any of them would otherwise decide which id a hook payload or a paneless
// register resolves to — measured 2026-09-25: 11 tests here fail under
// AGENT_SESSION_CHILD=1 AGENT_SESSION_ID=x CLAUDE_CODE_SESSION_ID=y.
func unsetHarnessIdentity() {
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "AGENT_SESSION_ID", "AGENT_SESSION_CHILD"} {
		_ = os.Unsetenv(k)
	}
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
