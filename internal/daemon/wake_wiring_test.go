package daemon

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

type fakeNotifier struct {
	mu       sync.Mutex
	notified []string // session IDs Notify'd
	cleared  []string // session IDs Clear'd
}

func (f *fakeNotifier) Notify(_, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notified = append(f.notified, sessionID)
	return nil
}

func (f *fakeNotifier) Clear(_, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, sessionID)
	return nil
}

func (f *fakeNotifier) snap(which *[]string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(*which))
	copy(out, *which)
	return out
}

func startWithNotifier(t *testing.T, n *fakeNotifier) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := Serve(paths.SocketPath(), s, n)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return paths.SocketPath()
}

func call(t *testing.T, sock, op string, args map[string]any) proto.Response {
	t.Helper()
	resp, err := client.Call(sock, proto.Request{Op: op, Args: args})
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	return resp
}

func TestNotifyDirectedExcludesActorBySession(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "backend", "role": "producer", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex", "socket_path": "/s", "session_id": "$2"})
	call(t, sock, "send_message", map[string]any{"from": "backend", "to_kind": "agent", "to_target": "consumer", "subject": "hi", "body": "x"})
	got := n.snap(&n.notified)
	if len(got) != 1 || got[0] != "$2" {
		t.Fatalf("expected only consumer session $2 notified, got %v", got)
	}
}

func TestNotifySkipsAgentsWithoutSession(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	// no session_id → not notifiable
	call(t, sock, "register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex", "socket_path": "/s"})
	call(t, sock, "send_message", map[string]any{"from": "backend", "to_kind": "agent", "to_target": "consumer", "subject": "hi", "body": "x"})
	if got := n.snap(&n.notified); len(got) != 0 {
		t.Fatalf("agent without session_id must not be notified, got %v", got)
	}
}

func TestNilNotifierIsSafe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := Serve(paths.SocketPath(), s, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	call(t, paths.SocketPath(), "register_agent", map[string]any{"alias": "a", "role": "r", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	if resp := call(t, paths.SocketPath(), "send_message", map[string]any{"from": "a", "to_kind": "broadcast", "subject": "s", "body": "b"}); !resp.OK {
		t.Fatalf("op should succeed with nil notifier: %+v", resp)
	}
}
