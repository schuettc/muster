package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// The channel MCP server is spawned env-stripped by some harnesses (Claude
// Code): $TMUX/$TMUX_PANE never reach it, so tmuxenv.CaptureEnv comes back
// paneless and the carrier idles — mail lands in the inbox but never wakes the
// session. The SessionStart hook survives this by walking process ancestry to
// find its pane; the channel must do the same, or the two disagree and only
// the hook resolves an identity (the 2026-09 "no tmux identity" report). This
// pins the env-else-ancestry fallback: env empty, but an ancestor IS a pane
// pid, so channelCapture must resolve that pane rather than go paneless.
func TestChannelCaptureFallsBackToAncestry(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")

	prevRun, prevAnc, prevDir := tmuxenv.Run, tmuxenv.AncestorPIDs, tmuxenv.SocketDir
	t.Cleanup(func() { tmuxenv.Run, tmuxenv.AncestorPIDs, tmuxenv.SocketDir = prevRun, prevAnc, prevDir })

	tmuxenv.AncestorPIDs = func() []int { return []int{4242, 1} }
	tmuxenv.Run = func(args ...string) (string, error) {
		for _, a := range args {
			if a == "list-panes" {
				return "4242\t%7\t$3\tmuster-2\t555", nil
			}
		}
		return "", nil
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "proj-muster"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tmuxenv.SocketDir = func() string { return dir }

	c := channelCapture()
	if c.SocketPath == "" || c.PaneID != "%7" || c.SessionID != "$3" || c.SessionCreated != 555 {
		t.Fatalf("channelCapture() = %+v, want the ancestry pane %%7 on $3 created 555 (env-stripped must fall back, not go paneless)", c)
	}
}

// A SIGTERM must end `muster channel` even while the client still holds its
// stdio pipes open. It did not: signal.NotifyContext swallowed the signal
// and the server loop kept waiting on stdin, so every client that spawned
// the channel and called kill() leaked a process (found by pi-channels).
func TestChannelExitsOnSIGTERMWithPipesHeldOpen(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "muster")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "channel")
	// Paneless on purpose: TMUX unset keeps the carrier idle and away from
	// any real daemon socket. MUSTER_NO_AUTOSPAWN is belt-and-braces.
	cmd.Env = append(os.Environ(), "TMUX=", "TMUX_PANE=", "MUSTER_NO_AUTOSPAWN=1", "MUSTER_HOME="+t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Prove it is serving: an initialize must be answered before we signal.
	if _, err := io.WriteString(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := stdout.Read(buf); err != nil {
		t.Fatalf("no initialize reply: %v", err)
	}
	// Pipes stay open; only the signal can end it.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("muster channel ignored SIGTERM while its pipes were held open")
	}
}
