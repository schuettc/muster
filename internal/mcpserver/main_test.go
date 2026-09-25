package mcpserver

import (
	"os"
	"testing"
)

// TestMain clears the variables tools-common/harness resolves a caller's
// session from. `go test` on a dev machine runs inside a Claude or pi session
// (or a pi-claude-bridge child, which also carries AGENT_SESSION_CHILD=1), and
// any of them would otherwise decide the caller identity these tests assert.
// Tests that need an identity set it with t.Setenv.
func TestMain(m *testing.M) {
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "AGENT_SESSION_ID", "AGENT_SESSION_CHILD"} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
