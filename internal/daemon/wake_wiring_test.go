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

type fakeWaker struct {
	mu    sync.Mutex
	panes []string // pane IDs knocked
}

func (f *fakeWaker) Wake(_, paneID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panes = append(f.panes, paneID)
	return nil
}

func (f *fakeWaker) knocked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.panes))
	copy(out, f.panes)
	return out
}

func startWithWaker(t *testing.T, w *fakeWaker) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := Serve(paths.SocketPath(), s, w)
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

func TestWakeDirectedExcludesActor(t *testing.T) {
	w := &fakeWaker{}
	sock := startWithWaker(t, w)
	call(t, sock, "register_agent", map[string]any{"alias": "backend", "role": "producer", "model_type": "claude", "socket_path": "/s", "pane_id": "%1"})
	call(t, sock, "register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex", "socket_path": "/s", "pane_id": "%2"})

	call(t, sock, "send_message", map[string]any{"from": "backend", "to_kind": "agent", "to_target": "consumer", "subject": "hi", "body": "x"})

	got := w.knocked()
	if len(got) != 1 || got[0] != "%2" {
		t.Fatalf("expected only consumer (%%2) knocked, got %v", got)
	}
}

func TestWakeRoleFanoutAndBroadcast(t *testing.T) {
	w := &fakeWaker{}
	sock := startWithWaker(t, w)
	call(t, sock, "register_agent", map[string]any{"alias": "backend", "role": "producer", "model_type": "claude", "socket_path": "/s", "pane_id": "%1"})
	call(t, sock, "register_agent", map[string]any{"alias": "rev1", "role": "reviewer", "model_type": "codex", "socket_path": "/s", "pane_id": "%2"})
	call(t, sock, "register_agent", map[string]any{"alias": "rev2", "role": "reviewer", "model_type": "codex", "socket_path": "/s", "pane_id": "%3"})

	call(t, sock, "task_create", map[string]any{"from": "backend", "to_kind": "role", "to_target": "reviewer", "subject": "review", "body": "y"})
	got := w.knocked()
	if len(got) != 2 {
		t.Fatalf("role fan-out should knock both reviewers, got %v", got)
	}
}

func TestNoWakeWhenWakerNil(t *testing.T) {
	// Sanity: a nil waker must not panic on an op.
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
	call(t, paths.SocketPath(), "register_agent", map[string]any{"alias": "a", "role": "r", "model_type": "claude", "socket_path": "/s", "pane_id": "%1"})
	resp := call(t, paths.SocketPath(), "send_message", map[string]any{"from": "a", "to_kind": "broadcast", "subject": "s", "body": "b"})
	if !resp.OK {
		t.Fatalf("op should succeed with nil waker: %+v", resp)
	}
}
