// Package wake delivers best-effort "notify" signals to agents' tmux sessions
// by setting/clearing a per-session tmux option (which the operator's status
// bar surfaces). It never types into a pane — see internal/nudge for that.
package wake

import (
	"context"
	"os/exec"
	"time"
)

// Notifier flags (or clears) a recipient's tmux session so the operator is
// notified of bus activity. Best-effort: errors mean the signal couldn't be
// delivered and should be ignored (the inbox is authoritative).
type Notifier interface {
	Notify(socketPath, sessionID string) error
	Clear(socketPath, sessionID string) error
}

// TmuxNotifier sets/clears a tmux user-option on a session, socket-aware, each
// call bounded by Timeout. Run is the command executor (nil → real tmux).
type TmuxNotifier struct {
	Option  string        // e.g. "@claude_attn"
	Timeout time.Duration // per tmux subprocess
	Run     func(ctx context.Context, args ...string) error
}

// NewTmuxNotifier returns a TmuxNotifier backed by the real tmux binary.
func NewTmuxNotifier(option string, timeout time.Duration) TmuxNotifier {
	return TmuxNotifier{Option: option, Timeout: timeout, Run: runTmux}
}

func runTmux(ctx context.Context, args ...string) error {
	return exec.CommandContext(ctx, "tmux", args...).Run()
}

func (n TmuxNotifier) run(args ...string) error {
	run := n.Run
	if run == nil {
		run = runTmux
	}
	to := n.Timeout
	if to <= 0 {
		to = 500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	return run(ctx, args...)
}

// Notify sets the option on the session and repaints that session's clients so
// a title-based banner updates. Best-effort; a missing client is fine.
func (n TmuxNotifier) Notify(socketPath, sessionID string) error {
	if err := n.run("-S", socketPath, "set-option", "-t", sessionID, n.Option, "1"); err != nil {
		return err
	}
	// Repaint each client attached to the session (a bare refresh-client from the
	// daemon has no client of its own). Best-effort: ignore listing/refresh errors.
	_ = n.refreshSessionClients(socketPath, sessionID)
	return nil
}

// Clear unsets the option on the session.
func (n TmuxNotifier) Clear(socketPath, sessionID string) error {
	return n.run("-S", socketPath, "set-option", "-t", sessionID, "-u", n.Option)
}

func (n TmuxNotifier) refreshSessionClients(socketPath, sessionID string) error {
	// list-clients then refresh each; errors are non-fatal.
	// We can't capture stdout via Run(error-only), so issue a broad refresh-client
	// targeted at the session; tmux accepts -t <session> for refresh-client to
	// repaint clients attached to it.
	return n.run("-S", socketPath, "refresh-client", "-t", sessionID)
}
