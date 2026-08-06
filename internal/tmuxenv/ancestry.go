package tmuxenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ancestryGuard bounds the parent-chain walk; 30 generations is far beyond
// any real hook → claude → shell nesting (mirrors claude-notify.sh's guard).
const ancestryGuard = 30

// AncestorPIDs returns this process's parent chain, nearest first, stopping
// at pid 0/1. Injectable for tests. The real implementation shells out to
// `ps -o ppid= -p <pid>` per hop — portable across macOS/Linux without cgo.
var AncestorPIDs = func() []int {
	pids := []int{os.Getpid()}
	pid := os.Getpid()
	for i := 0; i < ancestryGuard; i++ {
		out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			break
		}
		next, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil || next <= 1 {
			break
		}
		pids = append(pids, next)
		pid = next
	}
	return pids
}

// ProcessArgv returns pid's command line as `ps` renders it ("claude
// --agent-id a1 --team-name session-b41c"), or "" when it can't be read.
// Injectable for tests — the sibling seam to AncestorPIDs, shelling out the
// same way for the same reason: portable across macOS/Linux without cgo.
//
// -ww is load-bearing, not decoration: several procps builds truncate the
// command column to the terminal width (or 80 columns when there is no tty,
// which is every hook), and the flags this reads sit ~130 characters into a
// teammate's command line. A truncated read is indistinguishable from an
// absent flag, so without -ww teammate detection would silently degrade back
// to the transcript-only hole on exactly those Linux hosts. BSD ps (macOS)
// accepts -ww and ignores the widening, so one spelling serves both.
var ProcessArgv = func(pid int) string {
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// AncestorArgvContainsAll reports whether SOME ONE process in this process's
// parent chain was launched with EVERY token in its argv — the same walk, the
// same bounds, and the same fail-safe posture as CaptureFromAncestry, reading
// each hop's command line instead of matching it against pane PIDs.
//
// The hook layer uses it to recognize a fleet TEAMMATE: teammate Claude
// processes launch as `claude --agent-id … --team-name … --parent-session-id …`
// while primaries and team leads carry none of those, and a hook is a
// DESCENDANT of the process that spawned it — so the launch shape is reachable
// from here and, unlike the transcript, exists from process birth (a
// teammate's transcript file is not written until after its SessionStart hooks
// have already run).
//
// Why a set rather than one flag: a single token can appear in a NON-teammate's
// argv by coincidence — a primary prompted with `claude -p "…--team-name…"`, a
// wrapper's `sh -c`, a grep. Requiring the pair on one process's argv means
// only an actual launch shape matches; nothing but a teammate is launched with
// both. The tokens must share an ancestor: two flags on two different
// processes are not one launch.
//
// Matching is per argv token — a bare token, or the `token=value` spelling of
// the same flag — never a substring, so `--team-names-file` is not a match.
// Every failure (walk failed, ps unreadable, no tokens, an empty token)
// returns false: a hook must never block a session on an ancestry it couldn't
// read.
func AncestorArgvContainsAll(tokens ...string) bool {
	if len(tokens) == 0 {
		return false
	}
	for _, tok := range tokens {
		if tok == "" {
			return false
		}
	}
	for _, pid := range AncestorPIDs() {
		fields := strings.Fields(ProcessArgv(pid))
		if len(fields) == 0 {
			continue
		}
		all := true
		for _, tok := range tokens {
			if !argvHasToken(fields, tok) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// argvHasToken reports whether fields carries tok as a discrete argv token —
// bare, or as the `tok=value` spelling of the same flag. Never a substring
// match: `--team-names-file` does not carry `--team-name`.
func argvHasToken(fields []string, tok string) bool {
	for _, field := range fields {
		if field == tok || strings.HasPrefix(field, tok+"=") {
			return true
		}
	}
	return false
}

// SocketDir returns the directory holding this user's tmux server sockets:
// $TMUX_TMPDIR if set (tmux's own knob — our operator-tunable too), else
// /tmp/tmux-<uid>, tmux's default. Injectable for tests.
var SocketDir = func() string {
	if d := os.Getenv("TMUX_TMPDIR"); d != "" {
		return filepath.Join(d, "tmux-"+strconv.Itoa(os.Getuid()))
	}
	return filepath.Join("/tmp", "tmux-"+strconv.Itoa(os.Getuid()))
}

// capturePane assembles a Capture for a pane already located on sock, the
// way CaptureEnv does for its fields: socket/pane/session id/name/created
// come directly from the matched list-panes row, while Project and Label
// are read through the SAME query/SessionLabel helpers CaptureEnv uses —
// one derivation path for both entry points, not a forked copy.
func capturePane(sock, paneID, sessionID, sessionName string, created int64) Capture {
	c := Capture{
		SocketPath:     sock,
		PaneID:         paneID,
		SessionID:      sessionID,
		SessionName:    sessionName,
		SessionCreated: created,
		Project:        ProjectFromSocket(sock),
	}
	c.Label, c.LabelManual = SessionLabel(sock, paneID)
	return c
}

// CaptureFromAncestry locates the tmux pane this PROCESS runs under by
// walking its parent chain and matching pane PIDs across every tmux server
// socket on the machine, then captures the same identity CaptureEnv reads
// from $TMUX/$TMUX_PANE. Hooks need this: harnesses spawn them with a
// stripped environment (no $TMUX), but the hook is still a descendant of the
// pane's shell, so the ancestry names the pane even when the env can't
// (the claude-notify.sh technique, generalized across per-project sockets).
// Requires the hook to run synchronously — an async hook is reparented and
// the chain breaks. Fail-safe: no match returns the zero Capture (paneless
// behavior), NEVER a cwd-derived guess — two sessions in one directory is
// normal, and mis-anchoring an identity is worse than not anchoring it.
func CaptureFromAncestry() Capture {
	ancestors := AncestorPIDs()
	sockets, _ := filepath.Glob(filepath.Join(SocketDir(), "*"))
	for _, sock := range sockets {
		out, err := Run("-S", sock, "list-panes", "-aF",
			"#{pane_pid}\t#{pane_id}\t#{session_id}\t#{session_name}\t#{session_created}")
		if err != nil || out == "" {
			continue
		}
		byPID := map[int][]string{}
		for _, line := range strings.Split(out, "\n") {
			f := strings.Split(line, "\t")
			if len(f) != 5 {
				continue
			}
			if pid, err := strconv.Atoi(f[0]); err == nil {
				byPID[pid] = f
			}
		}
		for _, pid := range ancestors {
			if f, hit := byPID[pid]; hit {
				created, _ := strconv.ParseInt(f[4], 10, 64)
				return capturePane(sock, f[1], f[2], f[3], created)
			}
		}
	}
	return Capture{}
}
