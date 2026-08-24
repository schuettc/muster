package channel

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/mustertest"
	"github.com/schuettc/muster/internal/store"
)

// A real daemon on a temp socket; two registered agents; the carrier for
// "worker" sees exactly the mail meant for it.
func TestCarrierAgainstRealDaemon(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	t.Setenv("MUSTER_NO_AUTOSPAWN", "1")
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sock := filepath.Join(dir, "sock")
	d, err := daemon.Serve(sock, s, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	call := DaemonClient(sock)
	must := func(op string, args map[string]any) {
		t.Helper()
		if _, err := call(op, args); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	must("register_agent", map[string]any{"alias": "worker", "role": "producer", "model_type": "claude",
		"socket_path": "/tmp/tsock", "session_id": "$3", "pane_id": "%9", "session_name": "worker", "session_created": 1700000000})
	must("register_agent", map[string]any{"alias": "lead", "role": "producer", "model_type": "claude",
		"socket_path": "/tmp/tsock", "session_id": "$4", "pane_id": "%10", "session_name": "lead", "session_created": 1700000001})

	p := &pushes{}
	c := &Carrier{Call: call, Notify: p.notify, Errw: &strings.Builder{},
		Ident: Identity{SocketPath: "/tmp/tsock", SessionID: "$3", PaneID: "%9", SessionCreated: 1700000000}}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 0 {
		t.Fatalf("no unread at start, no summary: %v", p.got)
	}

	must("send_message", map[string]any{"from": "lead", "to_kind": "agent", "to_target": "worker",
		"subject": "review the branch", "body": "please", "intent": "action-requested"})
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 1 || !strings.Contains(p.got[0], `action-requested from lead on thread #`) || !strings.Contains(p.got[0], `"review the branch"`) {
		t.Fatalf("send → one push: %v", p.got)
	}
	threadID := threadFromPush(t, p.got[0])

	must("reply", map[string]any{"from": "lead", "thread_id": threadID, "body": "and one more thing"})
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 2 || !strings.HasPrefix(p.got[1], "muster: reply from lead") {
		t.Fatalf("reply → second push: %v", p.got)
	}

	must("get_inbox", map[string]any{"alias": "worker"})
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 2 {
		t.Fatalf("reading mail must not push: %v", p.got)
	}

	// worker's own outbound mail never comes back at it.
	must("send_message", map[string]any{"from": "worker", "to_kind": "agent", "to_target": "lead", "subject": "done", "body": "ok"})
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 2 {
		t.Fatalf("self-authored send must not push: %v", p.got)
	}
	if !strings.Contains(c.Status(), "aliases: worker") {
		t.Errorf("status: %q", c.Status())
	}
}

// threadFromPush parses "thread #N" out of a single-event content line.
func threadFromPush(t *testing.T, content string) int64 {
	t.Helper()
	i := strings.Index(content, "thread #")
	if i < 0 {
		t.Fatalf("no thread id in %q", content)
	}
	var id int64
	for _, r := range content[i+len("thread #"):] {
		if r < '0' || r > '9' {
			break
		}
		id = id*10 + int64(r-'0')
	}
	if id == 0 {
		t.Fatalf("thread id parse failed for %q", content)
	}
	return id
}
