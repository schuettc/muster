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
// The reply clause is deliberately conditional ("if the sender needs
// something"), not imperative: the unconditional "reply on the thread"
// wording manufactured ack-loops — every closing ack woke the peer into an
// instruction that said reply, which produced one more closing ack (measured
// 2026-07-30: 14% of all bus entries were acknowledgments).
const message = "📬 check your muster inbox: call get_inbox, read each new thread with get_thread, handle the request, and reply on the thread only if the sender needs something from you — act autonomously. Never reply just to acknowledge an ack or closure; to close out a thread yourself, reply with fyi=true so nobody is woken. (No muster MCP tools? The muster CLI is equivalent: muster inbox / thread / reply [--fyi].)"

// pasteSubmitDelay is the pause between typing the nudge text and sending a
// standalone Enter for harnesses whose TUI treats an Enter bundled with (or
// immediately following) pasted send-keys text as part of the paste, not a
// submit. A lone Enter after a short delay submits reliably for both Codex and
// Cursor Agent; empirically a zero delay fails (and for Cursor, leaves the
// text stuck in the composer so the next paste concatenates). 300ms is the
// shortest delay that cleared a Cursor Agent composer in delayed-Enter
// probes; shorter values often submitted and left residue. Applies to both
// Codex and Cursor. Claude submits with no delay.
const pasteSubmitDelay = 300 * time.Millisecond

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

// TypeLine types text into the pane and optionally submits it — the general
// form of Nudge, for callers with their own line to deliver (e.g. `muster
// label`'s /rename sync). Submit semantics are per-model, identical to Nudge:
// claude accepts an immediate Enter; codex and cursor need pasteSubmitDelay
// first; unknown model types are typed-only (submitted=false) so the caller
// can tell the operator to press Enter.
func (n TmuxNudger) TypeLine(socketPath, paneID, modelType, text string, submit bool) (bool, error) {
	if socketPath == "" || paneID == "" {
		return false, fmt.Errorf("agent has no tmux pane (not registered from inside tmux)")
	}
	if err := n.run("-S", socketPath, "send-keys", "-t", paneID, "-l", text); err != nil {
		return false, fmt.Errorf("send-keys failed (pane may be gone): %w", err)
	}
	if !submit {
		return false, nil
	}
	switch modelType {
	case "claude":
		// Immediate Enter submits.
	case "codex", "cursor":
		n.sleep(pasteSubmitDelay) // let the TUI finish processing the paste before Enter
	default:
		return false, nil // unknown submit behavior → typed-only
	}
	if err := n.run("-S", socketPath, "send-keys", "-t", paneID, "Enter"); err != nil {
		return false, fmt.Errorf("submit failed: %w", err)
	}
	return true, nil
}

// Nudge types the check-inbox line into the pane and (optionally) submits it.
func (n TmuxNudger) Nudge(socketPath, paneID, modelType string, submit bool) (bool, error) {
	return n.TypeLine(socketPath, paneID, modelType, message, submit)
}
