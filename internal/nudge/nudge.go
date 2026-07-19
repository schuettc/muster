// Package nudge delivers an operator-triggered "check your inbox" prompt into an
// agent's tmux pane via send-keys. This is the ONLY place muster types into a
// pane; automated bus activity uses internal/wake (notify) instead.
package nudge

import (
	"fmt"
	"os/exec"
	"time"
)

// message is the nudge's typed line. It carries the full drain-and-act
// instruction, not just "check your inbox" (spec §3b): a live incident
// (2026-07-16, thread 27) showed a nudged agent list a new thread and then
// idle — checking the inbox satisfied the old wording, and by then its own
// get_inbox had already cleared the flag, so the Stop hook never escalated.
const message = "📬 check your muster inbox: call get_inbox, read each new thread with get_thread, handle the request, and reply on the thread — act autonomously. (No muster MCP tools? The muster CLI is equivalent: muster inbox / thread / reply.)"

// codexSubmitDelay is the pause between typing the nudge text and sending a
// standalone Enter for codex. codex's TUI treats an Enter that is bundled with
// (or immediately follows) pasted send-keys text as part of the paste, not a
// submit; a lone Enter sent after a short delay submits reliably. Empirically a
// zero delay fails and a few hundred ms works, so this is a conservative
// default. claude submits with no delay.
const codexSubmitDelay = 500 * time.Millisecond

// TmuxNudger types a nudge into a pane and optionally submits it. Run is the
// command executor (nil → real tmux) and Sleep is the delay function (nil →
// time.Sleep); both are injectable for testing.
type TmuxNudger struct {
	Run   func(args ...string) error
	Sleep func(d time.Duration)
}

func (n TmuxNudger) run(args ...string) error {
	run := n.Run
	if run == nil {
		run = func(a ...string) error { return exec.Command("tmux", a...).Run() }
	}
	return run(args...)
}

func (n TmuxNudger) sleep(d time.Duration) {
	if n.Sleep != nil {
		n.Sleep(d)
		return
	}
	time.Sleep(d)
}

// Nudge types the check-inbox line into the pane. When submit is requested it
// presses Enter to submit the turn: claude accepts the Enter immediately, while
// codex needs a short delay after the text before a standalone Enter registers
// as a submit (see codexSubmitDelay). Unknown model types are typed-only
// (submitted=false) because their send-keys submit behavior is unverified, so
// the caller can tell the operator to press Enter.
func (n TmuxNudger) Nudge(socketPath, paneID, modelType string, submit bool) (bool, error) {
	if socketPath == "" || paneID == "" {
		return false, fmt.Errorf("agent has no tmux pane (not registered from inside tmux)")
	}
	if err := n.run("-S", socketPath, "send-keys", "-t", paneID, "-l", message); err != nil {
		return false, fmt.Errorf("send-keys failed (pane may be gone): %w", err)
	}
	if !submit {
		return false, nil
	}
	switch modelType {
	case "claude":
		// Immediate Enter submits.
	case "codex":
		n.sleep(codexSubmitDelay) // let codex finish processing the paste before Enter
	default:
		return false, nil // unknown submit behavior → typed-only
	}
	if err := n.run("-S", socketPath, "send-keys", "-t", paneID, "Enter"); err != nil {
		return false, fmt.Errorf("submit failed: %w", err)
	}
	return true, nil
}
