package daemon

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
	"github.com/schuettc/muster/internal/wake"
)

// TestWakeLandsInRealPane starts a real tmux session on a dedicated socket,
// registers an agent pointing at its pane, sends a message from another agent,
// and asserts the knock text appears in the pane via capture-pane.
func TestWakeLandsInRealPane(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sockDir := t.TempDir()
	tmuxSock := filepath.Join(sockDir, "t.sock")
	sess := "musterwake"
	// Start a detached session running `cat` so the pane stays alive and
	// echoes typed input into its buffer.
	if err := exec.Command("tmux", "-S", tmuxSock, "new-session", "-d", "-s", sess, "cat").Run(); err != nil {
		t.Fatalf("tmux new-session: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", tmuxSock, "kill-server").Run() })

	paneID := strings.TrimSpace(mustOut(t, exec.Command("tmux", "-S", tmuxSock, "list-panes", "-t", sess, "-F", "#{pane_id}")))

	dir := t.TempDir()
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := Serve(paths.SocketPath(), s, wake.NewTmuxWaker())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	reg := func(alias, pane string) {
		resp, err := client.Call(paths.SocketPath(), proto.Request{Op: "register_agent", Args: map[string]any{
			"alias": alias, "role": alias, "model_type": "claude", "socket_path": tmuxSock, "pane_id": pane,
		}})
		if err != nil || !resp.OK {
			t.Fatalf("register %s: err=%v resp=%+v", alias, err, resp)
		}
	}
	reg("sender", "%999") // sender's pane is irrelevant (it's the actor, never knocked)
	reg("target", paneID)

	resp, err := client.Call(paths.SocketPath(), proto.Request{Op: "send_message", Args: map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "target", "subject": "ping", "body": "x",
	}})
	if err != nil || !resp.OK {
		t.Fatalf("send: err=%v resp=%+v", err, resp)
	}

	// Give tmux a moment, then capture the pane and look for the knock text.
	var buf string
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		buf = mustOut(t, exec.Command("tmux", "-S", tmuxSock, "capture-pane", "-t", paneID, "-p"))
		if strings.Contains(buf, "muster: activity on") {
			return // success
		}
	}
	t.Fatalf("knock text not found in pane buffer; got:\n%s", buf)
}

func mustOut(t *testing.T, c *exec.Cmd) string {
	t.Helper()
	out, err := c.Output()
	if err != nil {
		t.Fatalf("%v: %v", c.Args, err)
	}
	return string(out)
}
