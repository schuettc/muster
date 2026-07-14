// Package nudge delivers an operator-triggered "check your inbox" prompt into an
// agent's tmux pane via send-keys. This is the ONLY place muster types into a
// pane; automated bus activity uses internal/wake (notify) instead.
package nudge

import (
	"fmt"
	"os/exec"
)

const message = "📬 check your muster inbox (call get_inbox)"

// TmuxNudger types a nudge into a pane and optionally submits it. Run is the
// command executor (nil → real tmux).
type TmuxNudger struct {
	Run func(args ...string) error
}

func (n TmuxNudger) run(args ...string) error {
	run := n.Run
	if run == nil {
		run = func(a ...string) error { return exec.Command("tmux", a...).Run() }
	}
	return run(args...)
}

// Nudge types the check-inbox line into the pane. When submit is requested it
// presses Enter ONLY for model types known to accept it (claude); codex holds
// send-keys text in its composer, so it is typed-only and submitted=false is
// returned so the caller can tell the operator to press Enter.
func (n TmuxNudger) Nudge(socketPath, paneID, modelType string, submit bool) (bool, error) {
	if socketPath == "" || paneID == "" {
		return false, fmt.Errorf("agent has no tmux pane (not registered from inside tmux)")
	}
	if err := n.run("-S", socketPath, "send-keys", "-t", paneID, "-l", message); err != nil {
		return false, fmt.Errorf("send-keys failed (pane may be gone): %w", err)
	}
	if submit && modelType == "claude" {
		if err := n.run("-S", socketPath, "send-keys", "-t", paneID, "Enter"); err != nil {
			return false, fmt.Errorf("submit failed: %w", err)
		}
		return true, nil
	}
	return false, nil
}
