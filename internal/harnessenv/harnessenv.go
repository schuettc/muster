// Package harnessenv captures a coding-agent harness session's identity —
// the paneless counterpart to tmuxenv. Harness runtimes that host sessions
// in a daemon (Claude Code's daemon-hosted sessions, background jobs) run
// them outside any tmux pane: $TMUX/$TMUX_PANE never reach the process even
// when the operator launched from a pane, so tmuxenv.CaptureEnv comes back
// empty. Identity then comes from what the harness DOES provide: every hook's
// stdin payload carries session_id and cwd, and the process environment
// carries $CLAUDE_CODE_SESSION_ID and a working directory. Like tmuxenv,
// this package is the single canonical capture path for that identity —
// callers must not read those variables directly.
package harnessenv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Capture holds the identity fields available to a paneless session. A
// paneless registration stores SessionID in the roster row's session_id with
// an empty socket_path — the tuple ("", SessionID) groups sibling aliases and
// scopes hook ownership exactly as (socket, tmux session id) does for panes.
type Capture struct {
	// SessionID is the harness's session UUID — stable for the session's
	// lifetime, present in every hook payload and in the session's own
	// process environment. "" when not running under a known harness.
	SessionID string
	// CWD is the session's working directory, the raw material for Alias and
	// Project derivation.
	CWD string
}

// FromEnv captures identity from the process environment: the harness
// session UUID and the working directory.
func FromEnv() Capture {
	cwd, _ := os.Getwd()
	return Capture{SessionID: os.Getenv("CLAUDE_CODE_SESSION_ID"), CWD: cwd}
}

// FromHookPayload captures identity from a hook's stdin payload (Claude Code
// and Codex both send {"session_id": ..., "cwd": ..., ...}), falling back to
// FromEnv for any field the payload doesn't carry. Invalid or empty JSON is
// tolerated — hooks must never fail on input shape.
func FromHookPayload(payload []byte) Capture {
	var p struct {
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
	}
	_ = json.Unmarshal(payload, &p)
	c := FromEnv()
	if p.SessionID != "" {
		c.SessionID = p.SessionID
	}
	if p.CWD != "" {
		c.CWD = p.CWD
	}
	return c
}

// Alias derives the fallback agent alias from the working directory — its
// basename, mirroring how the operator's per-project tmux sessions are named
// after their directory. "" when there is no usable directory ($MUSTER_ALIAS
// and explicit arguments always take precedence over this in callers).
func (c Capture) Alias() string {
	base := filepath.Base(c.CWD)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return ""
	}
	return base
}

// Project derives the project name by walking up from CWD to the enclosing
// git checkout. For a linked worktree (.git is a pointer file, "gitdir:
// <main>/.git/worktrees/<name>"), the project is the MAIN checkout's
// directory name, so every worktree of one repo shares its project — the
// same grouping the per-project tmux sockets ("proj-<project>") give
// pane-anchored agents. "" when CWD is not inside a git checkout.
func (c Capture) Project() string {
	dir := c.CWD
	for dir != "" {
		gitPath := filepath.Join(dir, ".git")
		if fi, err := os.Stat(gitPath); err == nil {
			if !fi.IsDir() {
				if main := mainCheckoutFromGitFile(gitPath); main != "" {
					return filepath.Base(main)
				}
			}
			return filepath.Base(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// mainCheckoutFromGitFile resolves a linked worktree's .git pointer file to
// the main checkout's path: "gitdir: /main/.git/worktrees/<name>" → "/main".
// "" when the file doesn't parse as a worktree pointer.
func mainCheckoutFromGitFile(gitPath string) string {
	b, err := os.ReadFile(gitPath)
	if err != nil {
		return ""
	}
	gd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
	sep := string(filepath.Separator)
	if i := strings.Index(gd, sep+".git"+sep); i > 0 {
		return gd[:i]
	}
	return ""
}
