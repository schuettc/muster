// Package tmuxenv is muster's single point of contact with tmux from outside
// the daemon: capturing the current pane's identity, deriving the project from
// the per-project socket, checking session liveness, and reading the session
// label. All tmux execution goes through Run, which tests override.
package tmuxenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Run executes `tmux <args>` and returns trimmed stdout. Overridable in tests.
var Run = func(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Capture holds the identity fields for registering an agent from a tmux pane.
// Every field is empty (LabelManual false) when not running inside tmux.
type Capture struct {
	SocketPath  string
	PaneID      string
	SessionID   string
	SessionName string
	Project     string
	Label       string
	LabelManual bool
}

// SocketFromEnv returns the tmux socket path from $TMUX ("<socket>,<pid>,<idx>").
func SocketFromEnv() string {
	tmux := os.Getenv("TMUX")
	if tmux == "" {
		return ""
	}
	return strings.SplitN(tmux, ",", 2)[0]
}

// ProjectFromSocket derives the project name from a per-project socket path
// ("proj-<project>"). Returns "" for non-proj-managed sockets (e.g. "default").
func ProjectFromSocket(socket string) string {
	if socket == "" {
		return ""
	}
	base := filepath.Base(socket)
	if !strings.HasPrefix(base, "proj-") {
		return ""
	}
	return strings.TrimPrefix(base, "proj-")
}

// LabelOption returns the tmux session option holding the label, defaulting to
// "@claude_task", overridable via $MUSTER_LABEL_OPTION.
func LabelOption() string {
	if v := os.Getenv("MUSTER_LABEL_OPTION"); v != "" {
		return v
	}
	return "@claude_task"
}

func query(socket, target, format string) string {
	if socket == "" || target == "" {
		return ""
	}
	out, err := Run("-S", socket, "display-message", "-p", "-t", target, format)
	if err != nil {
		return ""
	}
	return out
}

// IsSessionAlive reports whether the tmux session still exists on the socket.
func IsSessionAlive(socket, sessionID string) bool {
	if socket == "" || sessionID == "" {
		return false
	}
	_, err := Run("-S", socket, "has-session", "-t", sessionID)
	return err == nil
}

// SessionAttached reports whether at least one human tmux client is
// currently attached to the session (station spec iteration-5 Tier 1: the
// attach marker) — via the same query seam as IsSessionAlive/SessionLabel
// (display-message -p -t <session> '#{session_attached}'), which reports the
// session's attached-client count as a decimal string. Any non-empty,
// non-"0" result counts as attached; an empty socket/session or a query
// failure (dead session, no such socket) reads as not attached, exactly like
// query's other callers.
func SessionAttached(socket, sessionID string) bool {
	out := query(socket, sessionID, "#{session_attached}")
	return out != "" && out != "0"
}

// SessionName reads the LIVE session name for target (a pane or session ID)
// on socket, via the same query seam as SessionAttached/SessionLabel. Session
// names are mutable — tmux lets an operator rename a session at any time —
// so a value captured at register_agent time (store.Agent.SessionName) goes
// stale the moment that happens. Callers that need the name to reflect
// reality right now (e.g. `muster nudge`'s "nudging X → session Y" line)
// should call this instead of trusting the stored snapshot, falling back to
// it (or further, to the alias) only when this returns "" — an empty socket
// or target, an unreachable tmux, or a session that no longer exists all
// read as "" here, exactly like query's other callers.
func SessionName(socket, target string) string {
	return query(socket, target, "#{session_name}")
}

// SessionLabel reads the label option and its manual flag for target (a pane or
// session) on socket. manual is true only when <option>_manual == "1".
func SessionLabel(socket, target string) (string, bool) {
	opt := LabelOption()
	raw := query(socket, target, "#{"+opt+"}\x1f#{"+opt+"_manual}")
	if raw == "" {
		return "", false
	}
	parts := strings.SplitN(raw, "\x1f", 2)
	label := parts[0]
	manual := len(parts) > 1 && parts[1] == "1"
	return label, manual
}

// CurrentSessionOption reads a tmux user option's raw value for the ambient
// session — no -S/-t, relying on $TMUX in the process environment, as when
// running inside a hook or shell spawned from a tmux pane. Returns "" if
// tmux isn't reachable or the option is unset.
func CurrentSessionOption(name string) string {
	out, err := Run("show-options", "-qv", name)
	if err != nil {
		return ""
	}
	return out
}

// CurrentSessionName returns the ambient session's name (no -S/-t), or "" if
// tmux isn't reachable (e.g. not running inside tmux).
func CurrentSessionName() string {
	out, err := Run("display-message", "-p", "#{session_name}")
	if err != nil {
		return ""
	}
	return out
}

// CurrentSessionID returns the ambient session's tmux session_id (e.g. "$3"),
// the stable identity half of the (socket_path, session_id) tuple used to
// group sibling aliases (spec §3) — no -S/-t, relying on $TMUX in the process
// environment. Returns "" if tmux isn't reachable (e.g. not running inside
// tmux).
func CurrentSessionID() string {
	out, err := Run("display-message", "-p", "#{session_id}")
	if err != nil {
		return ""
	}
	return out
}

// SetSessionOption sets a tmux user option on the ambient session.
func SetSessionOption(name, value string) error {
	_, err := Run("set-option", name, value)
	return err
}

// UnsetSessionOption unsets a tmux user option on the ambient session.
func UnsetSessionOption(name string) error {
	_, err := Run("set-option", "-u", name)
	return err
}

// RefreshClient repaints the ambient session's attached clients (e.g. so a
// title bar reflects a just-changed label). Best-effort: callers should treat
// a returned error as non-fatal.
func RefreshClient() error {
	_, err := Run("refresh-client", "-S")
	return err
}

// CaptureEnv reads the current process's tmux environment into a Capture.
func CaptureEnv() Capture {
	socket := SocketFromEnv()
	pane := os.Getenv("TMUX_PANE")
	c := Capture{SocketPath: socket, PaneID: pane, Project: ProjectFromSocket(socket)}
	if socket == "" || pane == "" {
		return c
	}
	c.SessionID = query(socket, pane, "#{session_id}")
	c.SessionName = query(socket, pane, "#{session_name}")
	c.Label, c.LabelManual = SessionLabel(socket, pane)
	return c
}
