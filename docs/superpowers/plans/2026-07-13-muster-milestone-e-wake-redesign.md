# muster Milestone E — Wake Redesign (notify + operator-nudge) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Replace the daemon's blind `tmux send-keys` wake with (1) a socket-aware, best-effort **notify** (sets a tmux option on the recipient's *session* so the operator's banner lights up — never types into a pane) and (2) an operator-triggered **`muster nudge`** CLI (the only place `send-keys` lives).

**Architecture:** Registration captures a stable tmux `session_id`. The daemon's `notifyForThread` calls a `wake.Notifier` that sets/clears a tmux user-option per session (bounded by a timeout, never `send-keys`). `get_inbox` clears the flag. A separate `internal/nudge.TmuxNudger`, driven only by the `muster nudge` CLI command, does the send-keys poke (agent-aware submit).

**Tech Stack:** Go 1.26 · stdlib `os/exec`, `context`, `time` · existing `internal/{store,daemon,mcpserver,humancli,client,proto,paths}`.

## Global Constraints

- No launch changes to Claude/Codex; no new module deps; cgo-free.
- **The daemon must never call `tmux send-keys`.** Automated bus activity only *notifies*. `send-keys` lives solely in `internal/nudge` (CLI-only).
- Notify targets a stable **`session_id`** (e.g. `$1`), socket-aware (`tmux -S <socket> …`); never targets by session name.
- Notify/clear are **best-effort + bounded** — each tmux subprocess uses `exec.CommandContext` with a timeout; failures are swallowed (inbox is authoritative).
- Default notify option: **`@claude_attn`** (reuses the operator's existing banner), configurable.
- `just verify` green (fmt, lint [errcheck, revive doc-comments + `_` ignored params, gocritic, staticcheck], `go test -race`, build). **Run tests/verify with `TMPDIR=/tmp` on macOS** (unix-socket path length) and `GODEBUG=netdns=go` if `proxy.golang.org` DNS flakes.
- Spec: `docs/superpowers/specs/2026-07-13-wake-redesign-design.md`.

---

## File Structure

```
internal/store/models.go       # +SessionID on Agent
internal/store/schema.sql      # +session_id column on agents
internal/store/agents.go       # persist/read session_id; +GetAgent(alias)
internal/wake/wake.go          # REPLACE Waker → Notifier + TmuxNotifier (Notify/Clear, timeout, injectable)
internal/wake/wake_test.go     # rewrite for Notifier
internal/daemon/daemon.go      # wakeForThread→notifyForThread (Notifier by session, bounded, no send-keys); Serve(…, Notifier); get_inbox clears; +get_agent op
internal/daemon/wake_wiring_test.go   # rewrite: fake Notifier asserts session notify + clear
internal/daemon/wake_integration_test.go  # DELETE (send-keys moved to nudge)
internal/mcpserver/tools_registry.go  # register_agent resolves session_id via socket-aware display-message
internal/mcpserver/call_test.go       # startTestDaemon: Serve(…, nil)
internal/nudge/nudge.go        # NEW: TmuxNudger (send-keys, agent-aware submit)
internal/nudge/nudge_test.go   # NEW
internal/humancli/humancli.go  # NEW `nudge` subcommand
cmd/muster/main.go             # runServe wires TmuxNotifier; `nudge` routed via humancli
README.md                      # document notify + nudge
```

Shared test note: `internal/daemon` tests use a `fakeNotifier` recording `Notify`/`Clear` calls; `internal/nudge` tests use an injectable command runner; `internal/humancli` reuses its `startTestDaemon`.

---

### Task 1: Store — `session_id` on Agent + `GetAgent`

**Files:** Modify `internal/store/models.go`, `internal/store/schema.sql`, `internal/store/agents.go`; Test `internal/store/agents_test.go`

**Interfaces:**
- Produces: `Agent.SessionID string` (`json:"session_id"`); `RegisterAgent`/`ListAgents` round-trip it; `(*Store) GetAgent(alias string) (Agent, bool, error)` (ok=false if absent).

- [ ] **Step 1: Add the column + field**

`internal/store/schema.sql` — add `session_id` to the `agents` table (after `session_name`):
```sql
    session_name  TEXT NOT NULL DEFAULT '',
    session_id    TEXT NOT NULL DEFAULT '',
```
`internal/store/models.go` — add to `Agent` (after `SessionName`):
```go
	SessionName  string `json:"session_name"`
	SessionID    string `json:"session_id"`
```

- [ ] **Step 2: Write the failing test**

Append to `internal/store/agents_test.go`:
```go
func TestRegisterAgentRoundTripsSessionIDAndGetAgent(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "backend", Role: "producer", ModelType: "claude", SocketPath: "/s", PaneID: "%1", SessionName: "muster", SessionID: "$3"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok, err := s.GetAgent("backend")
	if err != nil || !ok {
		t.Fatalf("GetAgent: ok=%v err=%v", ok, err)
	}
	if got.SessionID != "$3" || got.SessionName != "muster" {
		t.Fatalf("session fields not round-tripped: %+v", got)
	}
	if _, ok, _ := s.GetAgent("nope"); ok {
		t.Fatalf("GetAgent should report ok=false for unknown alias")
	}
}
```

- [ ] **Step 3: Run — expect FAIL** (`undefined: (*Store).GetAgent`, and SessionID unknown):
`TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/store/ -run TestRegisterAgentRoundTripsSessionIDAndGetAgent -v`

- [ ] **Step 4: Implement**

In `internal/store/agents.go`, update `RegisterAgent`'s INSERT (add `session_id`) and its `ON CONFLICT` SET list (add `session_id=excluded.session_id`), update the column lists + `Scan` in `ListAgents`, and add `GetAgent`. The `agents` columns are now `alias, role, model_type, socket_path, pane_id, session_name, session_id, registered_at, last_seen`. Example additions:
```go
// RegisterAgent INSERT column list + values gain session_id (8th value, before registered_at):
//   INSERT INTO agents (alias, role, model_type, socket_path, pane_id, session_name, session_id, registered_at, last_seen)
//   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
// with a.SessionID passed in the matching position, and ON CONFLICT adding:
//   session_id=excluded.session_id,

// ListAgents SELECT + Scan gain session_id in the same position.

// New:
func (s *Store) GetAgent(alias string) (Agent, bool, error) {
	var a Agent
	err := s.db.QueryRow(`
SELECT alias, role, model_type, socket_path, pane_id, session_name, session_id, registered_at, last_seen
FROM agents WHERE alias=?`, alias).
		Scan(&a.Alias, &a.Role, &a.ModelType, &a.SocketPath, &a.PaneID, &a.SessionName, &a.SessionID, &a.RegisteredAt, &a.LastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, false, nil
	}
	if err != nil {
		return Agent{}, false, err
	}
	return a, true, nil
}
```
Add `"database/sql"` and `"errors"` imports to `agents.go` if not present. Update `RegisterAgent`/`ListAgents` fully (all column lists + Scan args) — do not leave a partial mismatch.

- [ ] **Step 5: Run — expect PASS.** Then `TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/store/ -v` (all store tests still green — the added column has a default so existing rows/inserts are fine).

- [ ] **Step 6: Commit**
```bash
git add internal/store/
git commit -m "feat(store): capture session_id on agents + GetAgent lookup

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 2: `wake` — replace `Waker` with `Notifier` + `TmuxNotifier`

**Files:** Rewrite `internal/wake/wake.go`, `internal/wake/wake_test.go`

**Interfaces:**
- Produces:
  - `type Notifier interface { Notify(socketPath, sessionID string) error; Clear(socketPath, sessionID string) error }`
  - `type TmuxNotifier struct { Option string; Timeout time.Duration; Run func(ctx context.Context, args ...string) error }`
  - `func NewTmuxNotifier(option string, timeout time.Duration) TmuxNotifier` (real tmux runner; `option` default caller-supplied e.g. `@claude_attn`)
  - `Notify` sets the option to `1` on the session then refreshes that session's clients; `Clear` unsets it. Both socket-aware, each tmux call bounded by `Timeout` via `exec.CommandContext`.

- [ ] **Step 1: Write the failing test**

Replace `internal/wake/wake_test.go` with:
```go
package wake

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTmuxNotifierNotifySetsOptionAndRefreshes(t *testing.T) {
	var calls [][]string
	n := TmuxNotifier{Option: "@claude_attn", Timeout: time.Second, Run: func(_ context.Context, args ...string) error {
		calls = append(calls, args)
		return nil
	}}
	if err := n.Notify("/sock", "$3"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	// First call must set the option on the session, socket-aware. No send-keys anywhere.
	if len(calls) == 0 {
		t.Fatal("no tmux calls")
	}
	set := strings.Join(calls[0], " ")
	if !strings.Contains(set, "-S /sock") || !strings.Contains(set, "set-option") || !strings.Contains(set, "-t $3") || !strings.Contains(set, "@claude_attn 1") {
		t.Fatalf("first call not a socket-aware set-option: %v", calls[0])
	}
	for _, c := range calls {
		if strings.Contains(strings.Join(c, " "), "send-keys") {
			t.Fatalf("Notify must NEVER send-keys, got: %v", c)
		}
	}
}

func TestTmuxNotifierClearUnsetsOption(t *testing.T) {
	var calls [][]string
	n := TmuxNotifier{Option: "@claude_attn", Timeout: time.Second, Run: func(_ context.Context, args ...string) error {
		calls = append(calls, args)
		return nil
	}}
	if err := n.Clear("/sock", "$3"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	got := strings.Join(calls[0], " ")
	if !strings.Contains(got, "-S /sock") || !strings.Contains(got, "set-option") || !strings.Contains(got, "-u") || !strings.Contains(got, "@claude_attn") || !strings.Contains(got, "-t $3") {
		t.Fatalf("Clear not a socket-aware unset: %v", calls[0])
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (package still has old `Waker`):
`TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/wake/ -v`

- [ ] **Step 3: Implement — replace `internal/wake/wake.go`**
```go
// Package wake delivers best-effort "notify" signals to agents' tmux sessions
// by setting/clearing a per-session tmux option (which the operator's status
// bar surfaces). It never types into a pane — see internal/nudge for that.
package wake

import (
	"context"
	"os/exec"
	"time"
)

// Notifier flags (or clears) a recipient's tmux session so the operator is
// notified of bus activity. Best-effort: errors mean the signal couldn't be
// delivered and should be ignored (the inbox is authoritative).
type Notifier interface {
	Notify(socketPath, sessionID string) error
	Clear(socketPath, sessionID string) error
}

// TmuxNotifier sets/clears a tmux user-option on a session, socket-aware, each
// call bounded by Timeout. Run is the command executor (nil → real tmux).
type TmuxNotifier struct {
	Option  string        // e.g. "@claude_attn"
	Timeout time.Duration // per tmux subprocess
	Run     func(ctx context.Context, args ...string) error
}

// NewTmuxNotifier returns a TmuxNotifier backed by the real tmux binary.
func NewTmuxNotifier(option string, timeout time.Duration) TmuxNotifier {
	return TmuxNotifier{Option: option, Timeout: timeout, Run: runTmux}
}

func runTmux(ctx context.Context, args ...string) error {
	return exec.CommandContext(ctx, "tmux", args...).Run()
}

func (n TmuxNotifier) run(args ...string) error {
	run := n.Run
	if run == nil {
		run = runTmux
	}
	to := n.Timeout
	if to <= 0 {
		to = 500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	return run(ctx, args...)
}

// Notify sets the option on the session and repaints that session's clients so
// a title-based banner updates. Best-effort; a missing client is fine.
func (n TmuxNotifier) Notify(socketPath, sessionID string) error {
	if err := n.run("-S", socketPath, "set-option", "-t", sessionID, n.Option, "1"); err != nil {
		return err
	}
	// Repaint each client attached to the session (a bare refresh-client from the
	// daemon has no client of its own). Best-effort: ignore listing/refresh errors.
	_ = n.refreshSessionClients(socketPath, sessionID)
	return nil
}

// Clear unsets the option on the session.
func (n TmuxNotifier) Clear(socketPath, sessionID string) error {
	return n.run("-S", socketPath, "set-option", "-t", sessionID, "-u", n.Option)
}

func (n TmuxNotifier) refreshSessionClients(socketPath, sessionID string) error {
	// list-clients then refresh each; errors are non-fatal.
	// We can't capture stdout via Run(error-only), so issue a broad refresh-client
	// targeted at the session; tmux accepts -t <session> for refresh-client to
	// repaint clients attached to it.
	return n.run("-S", socketPath, "refresh-client", "-t", sessionID)
}
```
Note on refresh: `refresh-client -t <session-id>` targets clients attached to that session (simpler than enumerating client names and adequate here). If a future test/behavior shows it's insufficient, enumerate via `list-clients -t <session>` — but keep this task's scope to the option set/clear, which is what the notify semantics depend on.

- [ ] **Step 4: Run — expect PASS** (both tests). `TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/wake/ -v`

- [ ] **Step 5: Commit**
```bash
git add internal/wake/
git commit -m "feat(wake): replace Waker with session Notifier (set/clear tmux option, bounded)

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 3: Daemon — `notifyForThread` (notify-only) + `Serve(Notifier)` + delete send-keys integration test

**Files:** Modify `internal/daemon/daemon.go`; rewrite `internal/daemon/wake_wiring_test.go`; delete `internal/daemon/wake_integration_test.go`; modify `internal/mcpserver/call_test.go`

**Interfaces:**
- Consumes: `wake.Notifier`, `store.ListAgents`/`GetThread`.
- Produces: `Serve(socketPath string, s *store.Store, n wake.Notifier) (*Daemon, error)` (n may be nil → no-op); the daemon notifies affected recipients' *sessions* after the 4 activity ops.

- [ ] **Step 1: Swap the daemon's dependency + rename**

In `internal/daemon/daemon.go`: change import `wake` usage; struct field `w wake.Waker` → `n wake.Notifier`; `Serve(socketPath string, s *store.Store, n wake.Notifier)` sets `d.n = n`. Rename `wakeForThread` → `notifyForThread` and rewrite its body to notify sessions instead of send-keys:
```go
// notifyForThread flags every agent affected by activity on threadID — the
// thread's originator plus its recipients (agent/role/broadcast), minus the
// actor — by notifying their tmux SESSION. Best-effort; never types into a pane.
func (d *Daemon) notifyForThread(threadID int64, actor string) {
	if d.n == nil {
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
			if a.Role == th.ToTarget && th.ToTarget != "" {
				recipients[a.Alias] = struct{}{}
			}
		}
	case "broadcast":
		for _, a := range agents {
			recipients[a.Alias] = struct{}{}
		}
	}
	delete(recipients, actor)
	for alias := range recipients {
		a, ok := byAlias[alias]
		if !ok || a.SocketPath == "" || a.SessionID == "" {
			continue
		}
		_ = d.n.Notify(a.SocketPath, a.SessionID)
	}
}
```
Update the 4 call sites in `dispatch` (`send_message`, `task_create`, `reply`, `task_transition`) from `d.wakeForThread(...)` → `d.notifyForThread(...)` (same args/actors: `from` for send/create/reply, `by` for transition).

- [ ] **Step 2: Fix the two `Serve` callers**
- `internal/mcpserver/call_test.go` `startTestDaemon`: `daemon.Serve(paths.SocketPath(), s, nil)` (already 3-arg; keep `nil`).
- `internal/daemon/wake_wiring_test.go`: rewritten below (uses a fakeNotifier).

- [ ] **Step 3: Delete the obsolete send-keys integration test**
```bash
git rm internal/daemon/wake_integration_test.go
```
(Its send-keys behavior moves to `internal/nudge` in Task 6.)

- [ ] **Step 4: Rewrite `wake_wiring_test.go` (fake Notifier)**

Replace `internal/daemon/wake_wiring_test.go` with:
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

type fakeNotifier struct {
	mu       sync.Mutex
	notified []string // session IDs Notify'd
	cleared  []string // session IDs Clear'd
}

func (f *fakeNotifier) Notify(socketPath, sessionID string) error {
	f.mu.Lock(); defer f.mu.Unlock()
	f.notified = append(f.notified, sessionID); return nil
}
func (f *fakeNotifier) Clear(socketPath, sessionID string) error {
	f.mu.Lock(); defer f.mu.Unlock()
	f.cleared = append(f.cleared, sessionID); return nil
}
func (f *fakeNotifier) snap(which *[]string) []string {
	f.mu.Lock(); defer f.mu.Unlock()
	out := make([]string, len(*which)); copy(out, *which); return out
}

func startWithNotifier(t *testing.T, n *fakeNotifier) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = s.Close() })
	d, err := Serve(paths.SocketPath(), s, n)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = d.Close() })
	return paths.SocketPath()
}

func call(t *testing.T, sock, op string, args map[string]any) proto.Response {
	t.Helper()
	resp, err := client.Call(sock, proto.Request{Op: op, Args: args})
	if err != nil { t.Fatalf("%s: %v", op, err) }
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
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = s.Close() })
	d, err := Serve(paths.SocketPath(), s, nil)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = d.Close() })
	call(t, paths.SocketPath(), "register_agent", map[string]any{"alias": "a", "role": "r", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	if resp := call(t, paths.SocketPath(), "send_message", map[string]any{"from": "a", "to_kind": "broadcast", "subject": "s", "body": "b"}); !resp.OK {
		t.Fatalf("op should succeed with nil notifier: %+v", resp)
	}
}
```
Note: this task's `register_agent` calls pass `session_id` directly as a daemon arg — so the daemon's `register_agent` dispatch must read `session_id` (add `"session_id": str(a,"session_id")` mapping in the dispatch's register case if not already present; the MCP-layer resolution comes in Task 4).

- [ ] **Step 5: Ensure the daemon `register_agent` op accepts `session_id`**

In `dispatch`'s `register_agent` case, add `SessionID: str(a, "session_id")` to the `store.Agent{...}` it builds.

- [ ] **Step 6: Run — fail→pass**
`TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/daemon/ ./internal/mcpserver/ -v` (daemon notify tests pass; mcpserver still compiles/passes).

- [ ] **Step 7: `just verify` + commit**
```bash
TMPDIR=/tmp GODEBUG=netdns=go just verify
git add internal/daemon/ internal/mcpserver/call_test.go
git commit -m "feat(daemon): notify recipient sessions (notify-only, no send-keys); Serve(Notifier)

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 4: `register_agent` resolves `session_id` via socket-aware `display-message`

**Files:** Modify `internal/mcpserver/tools_registry.go`; Test `internal/mcpserver/tools_registry_test.go`

**Interfaces:**
- Consumes: `callDaemon`, `$TMUX`/`$TMUX_PANE`.
- Produces: `register_agent` derives `session_id` + `session_name` from the pane via `tmux -S <socket> display-message -p -t <pane> '#{session_id}'` (and `#{session_name}`), using an injectable runner `tmuxQuery` (var, overridable in tests).

- [ ] **Step 1: Write the failing test**

Append to `internal/mcpserver/tools_registry_test.go`:
```go
func TestRegisterAgentResolvesSessionID(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/private/tmp/tmux-501/proj-muster,123,4")
	t.Setenv("TMUX_PANE", "%2")
	// stub the tmux query so we don't need a real tmux
	orig := tmuxQuery
	tmuxQuery = func(socket, pane, format string) string {
		switch format {
		case "#{session_id}":
			return "$7"
		case "#{session_name}":
			return "muster-2"
		}
		return ""
	}
	t.Cleanup(func() { tmuxQuery = orig })

	if _, _, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "reviewer", Role: "reviewer", ModelType: "codex"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	raw, err := callDaemon("list_agents", nil)
	if err != nil { t.Fatalf("list: %v", err) }
	var agents []AgentView
	_ = json.Unmarshal(raw, &agents)
	if len(agents) != 1 || agents[0].SessionName != "muster-2" {
		t.Fatalf("session_name not captured: %+v", agents)
	}
}
```
(Requires `AgentView` to include `SessionName` — it already does. `session_id` isn't surfaced in `AgentView`; the test asserts `session_name` as the observable proxy, and Task 3's daemon test already covers session_id-driven notify.)

- [ ] **Step 2: Run — expect FAIL** (`undefined: tmuxQuery`).

- [ ] **Step 3: Implement**

In `internal/mcpserver/tools_registry.go`, add the injectable query + use it in the handler:
```go
// tmuxQuery resolves a tmux format for a pane, socket-aware. Overridable in tests.
var tmuxQuery = func(socket, pane, format string) string {
	if socket == "" || pane == "" {
		return ""
	}
	out, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "-t", pane, format).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
```
In `registerAgentHandler`, after computing `socket := tmuxSocketPath()` and `pane := os.Getenv("TMUX_PANE")`, resolve the session and include it in the daemon args:
```go
	socket := tmuxSocketPath()
	pane := os.Getenv("TMUX_PANE")
	sessionID := tmuxQuery(socket, pane, "#{session_id}")
	sessionName := in.SessionName
	if sessionName == "" {
		sessionName = tmuxQuery(socket, pane, "#{session_name}")
	}
	_, err := callDaemon("register_agent", map[string]any{
		"alias": in.Alias, "role": in.Role, "model_type": in.ModelType,
		"session_name": sessionName, "session_id": sessionID,
		"socket_path": socket, "pane_id": pane,
	})
```
Add imports `"os/exec"`, `"strings"` (keep `"os"`). Keep `RegisterAgentIn.SessionName` (optional override; `omitempty` stays).

- [ ] **Step 4: Run — expect PASS.** Then `TMPDIR=/tmp GODEBUG=netdns=go just verify`.

- [ ] **Step 5: Commit**
```bash
git add internal/mcpserver/
git commit -m "feat(mcp): register_agent resolves stable session_id via socket-aware display-message

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 5: `get_inbox` clears the notify flag

**Files:** Modify `internal/daemon/daemon.go`; Test `internal/daemon/wake_wiring_test.go`

**Interfaces:**
- Consumes: `store.GetAgent`, `wake.Notifier.Clear`.
- Produces: the daemon's `get_inbox` op clears the caller's session flag after fetching the inbox (unconditional).

- [ ] **Step 1: Write the failing test**

Append to `internal/daemon/wake_wiring_test.go`:
```go
func TestGetInboxClearsFlag(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "reviewer", "role": "reviewer", "model_type": "codex", "socket_path": "/s", "session_id": "$5"})
	call(t, sock, "get_inbox", map[string]any{"alias": "reviewer"})
	got := n.snap(&n.cleared)
	if len(got) != 1 || got[0] != "$5" {
		t.Fatalf("get_inbox should clear reviewer's session $5, got %v", got)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (no clear happens yet).

- [ ] **Step 3: Implement**

In `dispatch`'s `get_inbox` case, after computing the threads and before returning, clear the caller's flag:
```go
	case "get_inbox":
		alias := str(a, "alias")
		threads, err := d.s.Inbox(alias)
		if err != nil {
			return fail(err)
		}
		if d.n != nil {
			if ag, ok, _ := d.s.GetAgent(alias); ok && ag.SocketPath != "" && ag.SessionID != "" {
				_ = d.n.Clear(ag.SocketPath, ag.SessionID)
			}
		}
		return ok(threads)
```

- [ ] **Step 4: Run — expect PASS.** Then `TMPDIR=/tmp GODEBUG=netdns=go just verify`.

- [ ] **Step 5: Commit**
```bash
git add internal/daemon/
git commit -m "feat(daemon): clear notify flag on get_inbox (reading = acknowledged)

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 6: `internal/nudge` + `get_agent` daemon op + `muster nudge` CLI

**Files:** Create `internal/nudge/nudge.go`, `internal/nudge/nudge_test.go`; Modify `internal/daemon/daemon.go` (+`get_agent` op), `internal/humancli/humancli.go`, `cmd/muster/main.go`; Test `internal/humancli/humancli_test.go`

**Interfaces:**
- Consumes: `store.Agent` fields (socket/pane/model_type), `callData`.
- Produces:
  - `nudge.TmuxNudger{ Run func(args ...string) error }`; `(TmuxNudger) Nudge(socketPath, paneID, modelType string, submit bool) (submitted bool, err error)` — types the literal check-inbox line; sends Enter when `submit && modelType=="claude"`; for `"codex"` types-only (returns submitted=false); best-effort.
  - daemon op `get_agent` → returns the full agent (socket/pane/model_type/session) for an alias.
  - `muster nudge <alias> [--no-submit]` in humancli.

- [ ] **Step 1: Write the failing nudge test**

Create `internal/nudge/nudge_test.go`:
```go
package nudge

import (
	"strings"
	"testing"
)

func recorder() (*[][]string, func(args ...string) error) {
	var calls [][]string
	return &calls, func(args ...string) error { calls = append(calls, args); return nil }
}

func TestNudgeClaudeTypesAndSubmits(t *testing.T) {
	calls, run := recorder()
	n := TmuxNudger{Run: run}
	submitted, err := n.Nudge("/s", "%1", "claude", true)
	if err != nil || !submitted {
		t.Fatalf("claude submit: submitted=%v err=%v", submitted, err)
	}
	joined := ""
	for _, c := range *calls { joined += strings.Join(c, " ") + "\n" }
	if !strings.Contains(joined, "send-keys") || !strings.Contains(joined, "-t %1") || !strings.Contains(joined, "Enter") {
		t.Fatalf("expected send-keys + Enter for claude:\n%s", joined)
	}
}

func TestNudgeCodexTypesOnly(t *testing.T) {
	calls, run := recorder()
	n := TmuxNudger{Run: run}
	submitted, err := n.Nudge("/s", "%2", "codex", true)
	if err != nil {
		t.Fatalf("codex: %v", err)
	}
	if submitted {
		t.Fatalf("codex must report submitted=false (its TUI ignores send-keys Enter)")
	}
	joined := ""
	for _, c := range *calls { joined += strings.Join(c, " ") + "\n" }
	if strings.Contains(joined, "Enter") {
		t.Fatalf("codex nudge must not send Enter:\n%s", joined)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (package doesn't exist).

- [ ] **Step 3: Implement `internal/nudge/nudge.go`**
```go
// Package nudge delivers an operator-triggered "check your inbox" prompt into an
// agent's tmux pane via send-keys. This is the ONLY place muster types into a
// pane; automated bus activity uses internal/wake (notify) instead.
package nudge

import (
	"fmt"
	"os/exec"
)

const message = "📬 check your muster inbox (call get_inbox)"

// TmuxNudger types a nudge into a pane and optionally submits it. Run is the
// command executor (nil → real tmux).
type TmuxNudger struct {
	Run func(args ...string) error
}

func (n TmuxNudger) run(args ...string) error {
	run := n.Run
	if run == nil {
		run = func(a ...string) error { return exec.Command("tmux", a...).Run() }
	}
	return run(args...)
}

// Nudge types the check-inbox line into the pane. When submit is requested it
// presses Enter ONLY for model types known to accept it (claude); codex holds
// send-keys text in its composer, so it is typed-only and submitted=false is
// returned so the caller can tell the operator to press Enter.
func (n TmuxNudger) Nudge(socketPath, paneID, modelType string, submit bool) (bool, error) {
	if socketPath == "" || paneID == "" {
		return false, fmt.Errorf("agent has no tmux pane (not registered from inside tmux)")
	}
	if err := n.run("-S", socketPath, "send-keys", "-t", paneID, "-l", message); err != nil {
		return false, fmt.Errorf("send-keys failed (pane may be gone): %w", err)
	}
	if submit && modelType == "claude" {
		if err := n.run("-S", socketPath, "send-keys", "-t", paneID, "Enter"); err != nil {
			return false, fmt.Errorf("submit failed: %w", err)
		}
		return true, nil
	}
	return false, nil
}
```
**Build-time investigation (do this in this task):** try to make Codex submit — e.g. send the text with `send-keys` (no `-l`, so it's typed not paste) then `Enter`, or `load-buffer`/`paste-buffer` then `Enter` — against a live `codex` pane. If a variant reliably submits, extend `Nudge` to submit for `codex` too and update `TestNudgeCodexTypesOnly` accordingly (document what worked in the commit). If none works reliably, keep types-only as specified.

- [ ] **Step 4: Run — expect PASS.**

- [ ] **Step 5: Add the `get_agent` daemon op**

In `internal/daemon/daemon.go` `dispatch`, add:
```go
	case "get_agent":
		ag, found, err := d.s.GetAgent(str(a, "alias"))
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"found": found, "agent": ag})
```

- [ ] **Step 6: Add the `nudge` CLI command (failing test first)**

Append to `internal/humancli/humancli_test.go`:
```go
func TestNudgeCommandRejectsUnknownAlias(t *testing.T) {
	startTestDaemon(t)
	var buf bytes.Buffer
	if err := Dispatch([]string{"nudge", "ghost"}, &buf); err == nil {
		t.Fatalf("expected error nudging an unregistered alias")
	}
}

func TestNudgeCommandResolvesAndNudges(t *testing.T) {
	startTestDaemon(t)
	// register an agent with a pane via the daemon op directly
	if _, err := callData("register_agent", map[string]any{"alias": "rev", "role": "reviewer", "model_type": "codex", "socket_path": "/s", "pane_id": "%2", "session_id": "$1"}); err != nil {
		t.Fatal(err)
	}
	var recorded [][]string
	origNudge := nudgeRun
	nudgeRun = func(args ...string) error { recorded = append(recorded, args); return nil }
	t.Cleanup(func() { nudgeRun = origNudge })

	var buf bytes.Buffer
	if err := Dispatch([]string{"nudge", "rev"}, &buf); err != nil {
		t.Fatalf("nudge: %v", err)
	}
	if !strings.Contains(buf.String(), "rev") || len(recorded) == 0 {
		t.Fatalf("expected resolved-target output + a send-keys call; out=%q calls=%v", buf.String(), recorded)
	}
}
```

- [ ] **Step 7: Implement the `nudge` command**

In `internal/humancli/humancli.go`: add `"github.com/schuettc/muster/internal/nudge"` and `"flag"` (if not present). Add an overridable runner + the command:
```go
// nudgeRun lets tests intercept the tmux command executor for nudges.
var nudgeRun func(args ...string) error

type agentFull struct {
	Alias       string `json:"alias"`
	ModelType   string `json:"model_type"`
	SocketPath  string `json:"socket_path"`
	PaneID      string `json:"pane_id"`
	SessionName string `json:"session_name"`
}

func cmdNudge(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("nudge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	noSubmit := fs.Bool("no-submit", false, "type the nudge but do not press Enter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("usage: muster nudge <alias> [--no-submit]")
	}
	alias := rest[0]
	raw, err := callData("get_agent", map[string]any{"alias": alias})
	if err != nil {
		return err
	}
	var res struct {
		Found bool      `json:"found"`
		Agent agentFull `json:"agent"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	if !res.Found {
		return fmt.Errorf("no agent registered as %q", alias)
	}
	ag := res.Agent
	fmt.Fprintf(out, "nudging %s → session %s / pane %s on %s\n", ag.Alias, ag.SessionName, ag.PaneID, ag.SocketPath)
	n := nudge.TmuxNudger{Run: nudgeRun} // nil in prod → real tmux
	submitted, err := n.Nudge(ag.SocketPath, ag.PaneID, ag.ModelType, !*noSubmit)
	if err != nil {
		return err
	}
	if submitted {
		fmt.Fprintln(out, "delivered + submitted.")
	} else {
		fmt.Fprintf(out, "delivered (not auto-submitted for %s — press Enter in that pane).\n", ag.ModelType)
	}
	return nil
}
```
Add `case "nudge": return cmdNudge(args[1:], out)` to `Dispatch`, and `"nudge"` to `main.go`'s combined subcommand case + usage string.

- [ ] **Step 8: Run — fail→pass**, then `TMPDIR=/tmp GODEBUG=netdns=go just verify`.

- [ ] **Step 9: Commit**
```bash
git add internal/nudge/ internal/daemon/daemon.go internal/humancli/ cmd/muster/main.go
git commit -m "feat(nudge): operator-triggered muster nudge (send-keys, agent-aware submit) + get_agent op

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 7: Wire the real `TmuxNotifier` into `serve` + README

**Files:** Modify `cmd/muster/main.go`, `README.md`

**Interfaces:**
- Consumes: `wake.NewTmuxNotifier`.
- Produces: `runServe` constructs the real notifier; docs updated.

- [ ] **Step 1: Wire runServe**

In `cmd/muster/main.go`, add `"github.com/schuettc/muster/internal/wake"` and `"time"`, and change the `daemon.Serve` call in `runServe`:
```go
	d, err := daemon.Serve(paths.SocketPath(), s, wake.NewTmuxNotifier("@claude_attn", 500*time.Millisecond))
```

- [ ] **Step 2: Build + boot check**
```bash
TMPDIR=/tmp GODEBUG=netdns=go just build
./bin/muster 2>&1 | head -1   # usage shows serve|debug|mcp|agents|inbox|send|tasks|nudge
```

- [ ] **Step 3: README — document the new behavior**

Update the `## CLI` section (and MCP notes) to add:
```markdown
### Notifications & nudging

When bus activity is addressed to an agent, muster **notifies** its tmux session
(sets `@claude_attn`, which lights the status-bar banner for tabs you're not
looking at) — it never types into a pane. The flag clears when that agent next
reads its inbox (`get_inbox`).

To actively poke an agent to act now:

```bash
muster nudge <alias>              # types "check your inbox" into the agent's pane
muster nudge <alias> --no-submit  # type only; don't press Enter
```

Nudge auto-submits for Claude Code; Codex holds the text in its composer, so
you press Enter there (muster tells you). Autonomous Codex wake (via its
app-server) is possible but requires launching Codex differently — deferred.
```

- [ ] **Step 4: `just verify` + commit**
```bash
TMPDIR=/tmp GODEBUG=netdns=go just verify
git add cmd/muster/main.go README.md
git commit -m "feat(serve): wire real TmuxNotifier; document notify + nudge

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

## Milestone E Definition of Done

- `just verify` green (fmt, lint, `-race`, build) locally + CI.
- Daemon notifies recipient *sessions* (socket-aware, bounded, actor-excluded, skips agents with no session) after send/create/reply/transition — and **never** calls `send-keys` (asserted by test).
- `get_inbox` clears the notify flag (unconditional).
- `register_agent` captures a stable `session_id`.
- `muster nudge <alias>` types into the pane, auto-submits for Claude, types-only + tells the operator for Codex (or auto-submits if the build-time investigation cracked it), rejects unknown aliases, prints the resolved target, `--no-submit` supported.
- The old blind-send-keys wake and its integration test are gone.

## Self-Review notes (author)
- **Spec coverage:** session_id capture (T1/T4), Notifier + set/clear/timeout (T2), daemon notify-only + Serve signature + actor-exclusion (T3), flag-clear on get_inbox (T5), CLI operator nudge with agent-aware submit + exact-alias + resolved-target + `--no-submit` (T6), real wiring + config + docs (T7). Deferred items (Codex app-server, Stop-hook, async queue, read-cursor) remain out of scope per spec.
- **No placeholders:** every step has runnable code/commands. (The one open exploration — cracking Codex submit — is bounded inside T6 with a defined fallback, not a blocking TODO.)
- **Type consistency:** `wake.Notifier{Notify,Clear}`, `TmuxNotifier`, `daemon.Serve(sock,store,Notifier)`, `notifyForThread(threadID,actor)`, `store.GetAgent`, `Agent.SessionID`, daemon ops `get_agent`/`register_agent(session_id)`, `nudge.TmuxNudger.Nudge(socket,pane,modelType,submit)`, humancli `cmdNudge`/`nudgeRun` — used consistently across tasks.
