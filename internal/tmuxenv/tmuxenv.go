// Package tmuxenv is muster's single point of contact with tmux from outside
// the daemon: capturing the current pane's identity, deriving the project from
// the per-project socket, checking session liveness, and reading the session
// label. All tmux execution goes through Run, which tests override.
package tmuxenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// Every field is empty (LabelManual false, SessionCreated 0) when not running
// inside tmux.
type Capture struct {
	SocketPath  string
	PaneID      string
	SessionID   string
	SessionName string
	Project     string
	Label       string
	LabelManual bool
	// SessionCreated is the session's creation time (#{session_created},
	// unix seconds) — the incarnation half of the identity: tmux recycles
	// session IDs from $0 whenever a server restarts, so (socket_path,
	// session_id) alone cannot tell a registration from a dead server
	// incarnation apart from one on today's session that reused its ID.
	// Creation time is immutable for a session's lifetime (unlike its
	// name), so a mismatch is proof of a recycled ID. 0 = unknown (not in
	// tmux, or a pre-upgrade registration).
	SessionCreated int64
}

// SocketFromEnv returns the tmux socket path from $TMUX ("<socket>,<pid>,<idx>").
func SocketFromEnv() string {
	tmux := os.Getenv("TMUX")
	if tmux == "" {
		return ""
	}
	p := strings.SplitN(tmux, ",", 2)[0]
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
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

// IsSessionAlive reports whether the tmux session a registration was
// captured from still exists on the socket AS THE SAME INCARNATION. tmux
// recycles session IDs across server restarts, so existence alone proves
// nothing: the stored session_created must equal the live session's. Rows
// with created == 0 (pre-v0.8.0 registrations that never captured it) can
// never prove their incarnation and therefore NEVER read alive — the
// 2026-08-05 rule (conversation-identity spec §5.1) replacing the old
// spare-legacy fallback, after a tombstoned 0-row rode a recycled $1 into a
// live session's badge. Reaping stays separate: DepartStaleSiblings still
// deliberately spares 0-rows (attribution and tombstoning are distinct
// decisions); operator-run `muster gc` is the sweep that retires them, and
// a tombstone revives with history intact on its next register.
func IsSessionAlive(socket, sessionID string, created int64) bool {
	if socket == "" || sessionID == "" || created == 0 {
		return false
	}
	out := query(socket, sessionID, "#{session_created}")
	if out == "" {
		return false // session gone (or tmux unreachable): dead either way
	}
	return out == strconv.FormatInt(created, 10)
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

// IsPaneAlive reports whether paneID still exists on socket — the client-side
// liveness check hook ownership gates use to tell a dead former owner from a
// live one (spec: "first live claimant wins"), via the same query seam as
// IsSessionAlive.
func IsPaneAlive(socket, paneID string) bool {
	return query(socket, paneID, "#{pane_id}") != ""
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

// SessionOption reads a tmux user option's raw value for a specific
// (socket, target) pair — the socket-aware counterpart to
// CurrentSessionOption, via the same query seam as SessionLabel/SessionName.
// A hook spawned with $TMUX stripped from its environment (the harness-hosted
// production case) has no ambient session for CurrentSessionOption to read;
// when identity instead came from tmuxenv.CaptureFromAncestry, this is how a
// caller reads an option (e.g. "@muster_inbox") for that WALKED tuple rather
// than trusting an environment that isn't there.
func SessionOption(socket, target, name string) string {
	return query(socket, target, "#{"+name+"}")
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

// CurrentSessionCreated returns the ambient session's #{session_created} —
// the incarnation half of tmux identity, the ambient counterpart of the
// value Capture carries (see CaptureEnv). tmux recycles session IDs across
// server restarts, so every session-scoped query that must prove WHICH
// incarnation it means pairs the session_id with this (spec §5.1). Returns 0
// if tmux isn't reachable or the value doesn't parse — the same "no proof"
// value a pre-v0.8.0 stored row carries.
func CurrentSessionCreated() int64 {
	out, err := Run("display-message", "-p", "#{session_created}")
	if err != nil {
		return 0
	}
	created, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return 0
	}
	return created
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

// SetSessionOptionOn sets a tmux user option on an explicit socket + target
// session — the env-stripped counterpart of SetSessionOption, for callers
// (the SessionStart projection) that resolved their coordinates via
// CaptureFromAncestry rather than the ambient environment.
func SetSessionOptionOn(socket, target, name, value string) error {
	_, err := Run("-S", socket, "set-option", "-t", target, name, value)
	return err
}

// RefreshClientOn repaints the attached clients of the server on socket —
// the env-stripped counterpart of RefreshClient. Best-effort, like its
// ambient sibling.
func RefreshClientOn(socket string) error {
	_, err := Run("-S", socket, "refresh-client", "-S")
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
	c.SessionCreated, _ = strconv.ParseInt(query(socket, pane, "#{session_created}"), 10, 64)
	c.Label, c.LabelManual = SessionLabel(socket, pane)
	return c
}
