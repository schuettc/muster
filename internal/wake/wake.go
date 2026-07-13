// Package wake delivers "knock" notifications to agents' tmux panes.
package wake

import "os/exec"

// Waker knocks on an agent's tmux pane to signal that new bus activity awaits.
type Waker interface {
	// Wake injects message into the pane identified by socketPath+paneID.
	// It is best-effort; an error means the knock could not be delivered
	// (e.g. the pane is gone) and callers should ignore it.
	Wake(socketPath, paneID, message string) error
}

// TmuxWaker knocks via `tmux -S <socket> send-keys`. Run is the command
// executor; when nil it runs the real tmux binary. It is exported for tests.
type TmuxWaker struct {
	Run func(args ...string) error
}

// NewTmuxWaker returns a TmuxWaker backed by the real tmux binary.
func NewTmuxWaker() TmuxWaker {
	return TmuxWaker{Run: func(args ...string) error {
		return exec.Command("tmux", args...).Run()
	}}
}

// Wake types the literal message into the pane, then presses Enter. Both calls
// are socket-aware. If the literal send fails (pane gone), Enter is skipped.
func (w TmuxWaker) Wake(socketPath, paneID, message string) error {
	run := w.Run
	if run == nil {
		run = func(args ...string) error { return exec.Command("tmux", args...).Run() }
	}
	if err := run("-S", socketPath, "send-keys", "-t", paneID, "-l", message); err != nil {
		return err
	}
	return run("-S", socketPath, "send-keys", "-t", paneID, "Enter")
}
