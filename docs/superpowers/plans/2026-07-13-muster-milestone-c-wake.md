# muster Milestone C — tmux Wake — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make the bus *live* — when an agent sends a message, creates/updates a task, or replies, the daemon "knocks" every other affected agent's tmux pane via socket-aware `send-keys`, so idle agents are woken instead of only seeing activity on their next poll.

**Architecture:** A new `internal/wake` package defines a `Waker` interface and a `TmuxWaker` (socket-aware `tmux send-keys`, injectable command runner for tests). The daemon gains a `Waker` and, after a successful `send_message`/`task_create`/`reply`/`task_transition`, resolves the thread's affected agents (originator + recipients, minus the actor) and knocks each live pane. Content still travels by poll (`get_inbox`/`get_thread`); the knock is just the push.

**Tech Stack:** Go 1.26 · stdlib `os/exec` for tmux · existing `internal/{daemon,store,client,proto,paths}`.

## Global Constraints

- **No new module dependencies.** Wake shells out to the `tmux` binary via `os/exec`.
- **Socket-aware.** Every tmux invocation targets the agent's absolute socket: `tmux -S <socket_path> ...`. This machine runs one tmux server per project, so the default socket is wrong.
- **Best-effort, passive.** A wake to a dead/absent pane must NOT error the originating op — the message already persisted; the knock is skipped. Liveness is checked implicitly (a failed `send-keys` is swallowed).
- **Actor is never woken.** The agent who performed the op does not knock itself.
- **cgo-free**, `just verify` green (fmt, lint, race, build). Lint is strict (errcheck, revive [doc comments on exported symbols; ignored params named `_`], gocritic, staticcheck).
- **Tests must not require a real tmux** except the one explicitly-marked integration test — use a fake `Waker` / fake command runner elsewhere.
- **macOS test note:** run tests with `TMPDIR=/tmp` prefix (unix-socket path-length limit with `t.TempDir()`), and `GODEBUG=netdns=go` if a go command hits proxy.golang.org DNS.

---

## File Structure

```
internal/wake/
├── wake.go            # Waker interface + TmuxWaker (injectable runner)
└── wake_test.go       # command-shape test via fake runner
internal/daemon/
├── daemon.go          # MODIFY: Daemon holds a Waker; Serve takes it; wakeForThread; call after 4 ops
├── daemon_test.go     # MODIFY: existing Serve(sock,s) calls → Serve(sock,s,nil)
└── wake_wiring_test.go # NEW: fake Waker asserts who gets woken per op
internal/mcpserver/call_test.go   # MODIFY: startTestDaemon's Serve call → pass nil waker
cmd/muster/main.go                # MODIFY: runServe passes a real wake.TmuxWaker
```

---

### Task 1: `internal/wake` package (Waker + TmuxWaker)

**Files:**
- Create: `internal/wake/wake.go`, `internal/wake/wake_test.go`

**Interfaces:**
- Produces:
  - `type Waker interface { Wake(socketPath, paneID, message string) error }`
  - `type TmuxWaker struct { Run func(args ...string) error }` — if `Run` is nil, defaults to executing `tmux <args...>`.
  - `func NewTmuxWaker() TmuxWaker` — returns one with the default exec runner.
  - `TmuxWaker.Wake` sends the literal message then Enter, both socket-aware; if the literal send fails (pane gone), it returns the error and does NOT send Enter.

- [ ] **Step 1: Write the failing test**

Create `internal/wake/wake_test.go`:
```go
package wake

import (
	"reflect"
	"testing"
)

func TestTmuxWakerSendsLiteralThenEnter(t *testing.T) {
	var calls [][]string
	w := TmuxWaker{Run: func(args ...string) error {
		calls = append(calls, args)
		return nil
	}}
	if err := w.Wake("/tmp/tmux-501/proj-x", "%6", "hello there"); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	want := [][]string{
		{"-S", "/tmp/tmux-501/proj-x", "send-keys", "-t", "%6", "-l", "hello there"},
		{"-S", "/tmp/tmux-501/proj-x", "send-keys", "-t", "%6", "Enter"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("unexpected tmux calls:\n got %v\nwant %v", calls, want)
	}
}

func TestTmuxWakerSkipsEnterWhenLiteralFails(t *testing.T) {
	var calls int
	boom := func(args ...string) error { calls++; if calls == 1 { return errPaneGone }; return nil }
	w := TmuxWaker{Run: boom}
	if err := w.Wake("/s", "%9", "hi"); err == nil {
		t.Fatalf("expected error when literal send fails")
	}
	if calls != 1 {
		t.Fatalf("Enter should not be sent after a failed literal; calls=%d", calls)
	}
}

var errPaneGone = &wakeErr{"pane gone"}

type wakeErr struct{ s string }

func (e *wakeErr) Error() string { return e.s }
```

- [ ] **Step 2: Run to verify it fails**

Run: `TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/wake/ -v`
Expected: FAIL — package `wake` has no `TmuxWaker`.

- [ ] **Step 3: Implement `wake.go`**

Create `internal/wake/wake.go`:
```go
// Package wake delivers "knock" notifications to agents' tmux panes.
package wake

import "os/exec"

// Waker knocks on an agent's tmux pane to signal that new bus activity awaits.
type Waker interface {
	// Wake injects message into the pane identified by socketPath+paneID.
	// It is best-effort; an error means the knock could not be delivered
	// (e.g. the pane is gone) and callers should ignore it.
	Wake(socketPath, paneID, message string) error
}

// TmuxWaker knocks via `tmux -S <socket> send-keys`. Run is the command
// executor; when nil it runs the real tmux binary. It is exported for tests.
type TmuxWaker struct {
	Run func(args ...string) error
}

// NewTmuxWaker returns a TmuxWaker backed by the real tmux binary.
func NewTmuxWaker() TmuxWaker {
	return TmuxWaker{Run: func(args ...string) error {
		return exec.Command("tmux", args...).Run()
	}}
}

// Wake types the literal message into the pane, then presses Enter. Both calls
// are socket-aware. If the literal send fails (pane gone), Enter is skipped.
func (w TmuxWaker) Wake(socketPath, paneID, message string) error {
	run := w.Run
	if run == nil {
		run = func(args ...string) error { return exec.Command("tmux", args...).Run() }
	}
	if err := run("-S", socketPath, "send-keys", "-t", paneID, "-l", message); err != nil {
		return err
	}
	return run("-S", socketPath, "send-keys", "-t", paneID, "Enter")
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/wake/ -v`
Expected: PASS (both).

- [ ] **Step 5: Commit**
```bash
git add internal/wake/
git commit -m "feat(wake): Waker interface + socket-aware TmuxWaker

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 2: Daemon wake wiring (resolve recipients + knock after ops)

**Files:**
- Modify: `internal/daemon/daemon.go`, `internal/daemon/daemon_test.go`, `internal/mcpserver/call_test.go`
- Create: `internal/daemon/wake_wiring_test.go`

**Interfaces:**
- `Serve(socketPath string, s *store.Store, w wake.Waker) (*Daemon, error)` — `w` may be nil (no knocks).
- After a successful `send_message`/`task_create`/`reply`/`task_transition`, the daemon calls `d.wakeForThread(threadID, actor)`.
- `wakeForThread` resolves affected agents = {thread.FromAgent} ∪ resolve(ToKind, ToTarget), minus `actor`, and knocks each whose socket_path+pane_id are non-empty.

- [ ] **Step 1: Update `Serve` and add the waker field**

In `internal/daemon/daemon.go`: add `"github.com/schuettc/muster/internal/wake"` and (for the message) `"fmt"` to imports. Change the struct and constructor:
```go
type Daemon struct {
	ln net.Listener
	s  *store.Store
	w  wake.Waker
}

// Serve binds socketPath (replacing any stale socket) and serves in a
// goroutine. w may be nil, in which case no wake knocks are delivered.
func Serve(socketPath string, s *store.Store, w wake.Waker) (*Daemon, error) {
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	d := &Daemon{ln: ln, s: s, w: w}
	go d.acceptLoop()
	return d, nil
}
```

- [ ] **Step 2: Add `wakeForThread`**

Add to `internal/daemon/daemon.go`:
```go
// wakeForThread knocks every agent affected by activity on threadID — the
// thread's originator plus its recipients (by agent, role, or broadcast) —
// except the actor who just acted. Best-effort; failures are ignored.
func (d *Daemon) wakeForThread(threadID int64, actor string) {
	if d.w == nil {
		return
	}
	th, _, err := d.s.GetThread(threadID)
	if err != nil {
		return
	}
	agents, err := d.s.ListAgents()
	if err != nil {
		return
	}
	byAlias := make(map[string]store.Agent, len(agents))
	for _, a := range agents {
		byAlias[a.Alias] = a
	}
	recipients := map[string]struct{}{th.FromAgent: {}}
	switch th.ToKind {
	case "agent":
		recipients[th.ToTarget] = struct{}{}
	case "role":
		for _, a := range agents {
			if a.Role == th.ToTarget {
				recipients[a.Alias] = struct{}{}
			}
		}
	case "broadcast":
		for _, a := range agents {
			recipients[a.Alias] = struct{}{}
		}
	}
	delete(recipients, actor)

	msg := fmt.Sprintf("📬 muster: activity on %q from %s — call get_inbox to read", th.Subject, actor)
	for alias := range recipients {
		a, ok := byAlias[alias]
		if !ok || a.SocketPath == "" || a.PaneID == "" {
			continue
		}
		_ = d.w.Wake(a.SocketPath, a.PaneID, msg)
	}
}
```

- [ ] **Step 3: Call it after the four ops**

In `dispatch`, after each op's store call succeeds and before `return ok(...)`, add the wake call:
- `send_message` case (actor is the sender): after `id, err := d.s.CreateThread(...)` success →
  ```go
  d.wakeForThread(id, str(a, "from"))
  return ok(map[string]any{"thread_id": id})
  ```
- `task_create` case (actor is the creator): same pattern →
  ```go
  d.wakeForThread(id, str(a, "from"))
  return ok(map[string]any{"thread_id": id})
  ```
- `reply` case (actor is the replier): after `id, err := d.s.AppendEntry(...)` success →
  ```go
  d.wakeForThread(i64(a, "thread_id"), str(a, "from"))
  return ok(map[string]any{"entry_id": id})
  ```
- `task_transition` case (actor made the change): after `TransitionTask` success →
  ```go
  d.wakeForThread(i64(a, "thread_id"), str(a, "by"))
  return ok(nil)
  ```
(Do NOT wake on register/list/get/kv/claim. Note: `task_claim` is intentionally not woken here — a claim's status change is surfaced when the requester next reads; keep C scoped to the four ops above.)

- [ ] **Step 4: Fix the two existing callers**

- `internal/daemon/daemon_test.go`: the `Serve(sock, s)` call → `Serve(sock, s, nil)`.
- `internal/mcpserver/call_test.go`: in `startTestDaemon`, `daemon.Serve(paths.SocketPath(), s)` → `daemon.Serve(paths.SocketPath(), s, nil)`.

- [ ] **Step 5: Write the wake-wiring test**

Create `internal/daemon/wake_wiring_test.go`:
```go
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

func (f *fakeWaker) Wake(socketPath, paneID, message string) error {
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
```

- [ ] **Step 6: Run to verify fail → pass**

Run: `TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/daemon/ -v`
Expected: initially FAIL (Serve arity / undefined), then PASS after Steps 1-4. Also run `./internal/mcpserver/` to confirm the startTestDaemon edit compiles.

- [ ] **Step 7: `just verify` + commit**

Run: `TMPDIR=/tmp GODEBUG=netdns=go just verify` → green.
```bash
git add internal/daemon/ internal/mcpserver/call_test.go
git commit -m "feat(daemon): knock affected agents' panes after message/task ops

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 3: Wire the real TmuxWaker into `muster serve` + live integration test

**Files:**
- Modify: `cmd/muster/main.go`
- Create: `internal/daemon/wake_integration_test.go`

**Interfaces:**
- `runServe` constructs `wake.NewTmuxWaker()` and passes it to `daemon.Serve`.
- A `-tags integration`-free but tmux-gated test proves a real `send-keys` lands in a real pane.

- [ ] **Step 1: Wire the real waker**

In `cmd/muster/main.go`, add `"github.com/schuettc/muster/internal/wake"` import, and in `runServe` change the `daemon.Serve(paths.SocketPath(), s)` call to:
```go
	d, err := daemon.Serve(paths.SocketPath(), s, wake.NewTmuxWaker())
```

- [ ] **Step 2: Write the live tmux integration test (skips if tmux absent)**

Create `internal/daemon/wake_integration_test.go`:
```go
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
```

- [ ] **Step 3: Run the integration test**

Run: `TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/daemon/ -run TestWakeLandsInRealPane -v`
Expected: PASS (or SKIP if tmux absent — but tmux is installed on this machine). If it flakes on timing, the 2s poll window should cover it; do not shorten.

- [ ] **Step 4: `just verify` + commit**

Run: `TMPDIR=/tmp GODEBUG=netdns=go just verify` → green.
```bash
git add cmd/muster/main.go internal/daemon/wake_integration_test.go
git commit -m "feat(serve): use real TmuxWaker; live pane-knock integration test

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

## Milestone C Definition of Done

- `just verify` green (fmt, lint, race, build) locally and CI.
- A `send_message`/`task_create`/`reply`/`task_transition` knocks every affected agent's pane (originator + recipients, minus actor), resolving agent/role/broadcast; misses are silently skipped.
- Knocks are socket-aware (cross per-project tmux servers).
- A live integration test proves a real `send-keys` lands in a real pane.
- Nil waker path (existing tests / non-tmux use) is a safe no-op.

## Self-Review notes (author)
- **Spec coverage:** the "wake channel / knock on the door" from the design (poll for content, push the wake), socket-aware for per-project servers, passive liveness (skip dead panes), actor excluded. Uses the `socket_path`/`pane_id` that Milestone B's `register_agent` already captures.
- **No placeholders:** all code complete.
- **Type consistency:** `wake.Waker`/`TmuxWaker`/`NewTmuxWaker`, `daemon.Serve(sock, store, waker)`, `wakeForThread(threadID, actor)`; the four wake call-sites use `from` (send/create/reply) and `by` (transition) as actor, matching the existing dispatch arg keys.
- **Known risk:** the live integration test depends on tmux timing/`cat`-pane echo behavior; the 2s poll and `send-keys -l` (literal) mitigate. If `cat` doesn't echo on this tmux build, an alternative is a pane running an interactive shell — but `cat` echoing stdin to the pane buffer is standard.
