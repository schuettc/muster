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
