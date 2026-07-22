# Project-Scoped Broadcast Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a broadcast be scoped to one project — `to_kind='broadcast'` with `to_target='<project>'` reaches only that project's agents; empty target stays global — with daemon-side validation rejecting unknown projects.

**Architecture:** Reuse the existing broadcast kind; the scope rides in `to_target`, which is empty for every historical row. The store's one canonical concern predicate gains the scope check; the daemon validates the project at send time (the black-hole backstop, same principle as alias resolution) and filters the wake fan-out; CLI grows a `--project` flag; MCP changes are description-only.

**Tech Stack:** Go, pure-Go SQLite (`modernc.org/sqlite`), no cgo. Spec: `docs/superpowers/specs/2026-07-22-project-broadcast-design.md`.

## Global Constraints

- Work in the worktree `/Users/courtschuett/GitHub/worktrees/muster-project-broadcast` on branch `feat/project-broadcast`. Never touch the primary clone.
- The full gate is `just verify` (gofmt, golangci-lint, `go test -race`, build). Run it before the final commit; per-task, run the named package tests.
- `CGO_ENABLED=0` must keep building. Add no dependencies.
- stdout is sacred in mcp mode; no stray prints (all daemon/MCP diagnostics go to stderr — this plan adds no prints at all).
- Exact-match project validation only — no fuzzy/prefix matching (spec "Out of scope").
- Error format, verbatim: `no registered agents in project "<target>" (known projects: <comma-space separated, sorted, deduped, non-departed, non-empty>)`.
- Journal target format: `broadcast` (global) / `broadcast:<project>` (scoped).

---

### Task 1: Store — scoped broadcast in the canonical concern predicate

**Files:**
- Modify: `internal/store/threads.go:162-178` (`threadConcerns`, `threadConcernsJoin`)
- Modify: `internal/store/threads.go:242` (Inbox bind args)
- Modify: `internal/store/agents.go:194` (UnreadCount bind args)
- Modify: `internal/store/events.go:64` (Events bind args)
- Test: `internal/store/threads_test.go`, `internal/store/agents_test.go`

**Interfaces:**
- Consumes: existing `threadConcerns` / `threadConcernsJoin` SQL fragments and their call sites.
- Produces: `threadConcerns` binding the alias **4** times (order: direct-target, role subselect, NEW project subselect, from_agent); `threadConcernsJoin` unchanged in bind count (uses `sess.alias`). Later tasks rely on: a thread `{ToKind: "broadcast", ToTarget: "projX"}` concerns exactly the agents whose registered `project` is `projX` (plus the originator), and departed rows keep matching via their preserved `project`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/threads_test.go`:

```go
func TestInboxScopedBroadcastMatchesProjectOnly(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "in-proj", Project: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "other-proj", Project: "api"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "no-proj"}); err != nil {
		t.Fatal(err)
	}
	mk := func(toTarget string) {
		if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "sender", ToKind: "broadcast", ToTarget: toTarget}, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	mk("web") // scoped to web
	mk("")    // global

	for alias, want := range map[string]int{"in-proj": 2, "other-proj": 1, "no-proj": 1} {
		in, err := s.Inbox(alias)
		if err != nil {
			t.Fatalf("inbox(%s): %v", alias, err)
		}
		if len(in) != want {
			t.Fatalf("inbox(%s) = %d threads, want %d", alias, len(in), want)
		}
	}

	// UnreadCount agrees with Inbox (same canonical predicate).
	if n, err := s.UnreadCount("other-proj"); err != nil || n != 1 {
		t.Fatalf("UnreadCount(other-proj) = %d (%v), want 1", n, err)
	}
	if n, err := s.UnreadCount("in-proj"); err != nil || n != 2 {
		t.Fatalf("UnreadCount(in-proj) = %d (%v), want 2", n, err)
	}
}

func TestScopedBroadcastConcernsDepartedAgentsProject(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "ghost", Project: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DepartAgent("ghost"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "sender", ToKind: "broadcast", ToTarget: "web"}, "hi"); err != nil {
		t.Fatal(err)
	}
	// Tombstoned rows preserve project; a departed agent that re-registers
	// into the same alias sees the scoped broadcast (read-time semantics).
	in, err := s.Inbox("ghost")
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 1 {
		t.Fatalf("departed ghost inbox = %d threads, want 1", len(in))
	}
}
```

Extend the fixture matrix in `TestThreadConcernsSessionJoinEquivalence` (`internal/store/agents_test.go:335-338`) — after the existing `mk(...)` calls add scoped-broadcast shapes, and give the agents projects so scoping discriminates. Replace the three `RegisterAgent` calls at lines 320-328 and the `mk` block so the test reads:

```go
	if err := s.RegisterAgent(Agent{Alias: "rev1", Role: "reviewer", Project: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "rev2", Role: "reviewer", Project: "api"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "other", Role: "producer"}); err != nil {
		t.Fatal(err)
	}
```

and after `mk("task", "other", "agent", "rev1")` add:

```go
	mk("message", "backend", "broadcast", "web") // scoped: concerns rev1 only
	mk("message", "backend", "broadcast", "api") // scoped: concerns rev2 only
	mk("message", "backend", "broadcast", "gone") // scoped: concerns nobody
```

The loop body needs the extra bind on the literal form — change line 360 to bind the alias **four** times:

```go
		want := idsMatching(`SELECT id FROM threads WHERE `+threadConcerns+` ORDER BY id`, alias, alias, alias, alias)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/courtschuett/GitHub/worktrees/muster-project-broadcast && go test ./internal/store/ -run 'TestInboxScopedBroadcast|TestScopedBroadcastConcernsDeparted|TestThreadConcernsSessionJoinEquivalence' -v`
Expected: FAIL — `TestInboxScopedBroadcastMatchesProjectOnly` fails because `other-proj`/`no-proj` see the scoped broadcast (predicate matches every broadcast); the equivalence test fails with a bind-count error ("not enough args") on the 4-bind literal form.

- [ ] **Step 3: Change the predicate and the bind lists**

In `internal/store/threads.go` replace the two fragments (keep the doc comments above them, appending one sentence to `threadConcerns`'s comment: "A scoped broadcast (to_target != '') concerns only agents whose registered project matches it exactly; binds the alias four times."):

```go
const threadConcerns = `((threads.to_kind='agent'  AND threads.to_target=?)
   OR (threads.to_kind='role'      AND threads.to_target != '' AND threads.to_target=(SELECT role FROM agents WHERE alias=?))
   OR (threads.to_kind='broadcast' AND (threads.to_target='' OR threads.to_target=(SELECT project FROM agents WHERE alias=?)))
   OR (threads.from_agent=?))`
```

```go
const threadConcernsJoin = `((threads.to_kind='agent' AND threads.to_target=sess.alias)
   OR (threads.to_kind='role'     AND threads.to_target != '' AND threads.to_target=(SELECT role FROM agents WHERE alias=sess.alias))
   OR (threads.to_kind='broadcast' AND (threads.to_target='' OR threads.to_target=(SELECT project FROM agents WHERE alias=sess.alias)))
   OR (threads.from_agent=sess.alias))`
```

Update the three literal-form call sites to bind one more alias:
- `internal/store/threads.go:242` (Inbox): `alias, alias, alias, alias, alias` → `alias, alias, alias, alias, alias, alias` (predicate now takes 4, unread CTE still takes 2).
- `internal/store/agents.go:194` (UnreadCount): five `alias` args → six.
- `internal/store/events.go:64` (Events): `q.Agent` six times → seven (three pre-predicate + four predicate).

`SessionUnread` (`agents.go:236-251`) uses the join form only — no bind change.

- [ ] **Step 4: Run the store package tests**

Run: `go test -race ./internal/store/`
Expected: PASS (all — including the pre-existing broadcast/inbox tests, which assert global behavior is unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat(store): scoped broadcast in the canonical concern predicate"
```

---

### Task 2: Daemon — validate the project at send time

**Files:**
- Modify: `internal/daemon/daemon.go:470-511` (send_message / task_create dispatch cases) plus a new helper near `resolveAgentTarget`'s pattern
- Test: `internal/daemon/daemon_test.go` (or a new `internal/daemon/broadcast_test.go`)

**Interfaces:**
- Consumes: `d.s.ListAgents() ([]store.Agent, error)`; `store.Agent{Alias, Project, Departed}`; test harness `startWithNotifierAndStore(t, n)` / `call(t, sock, op, args)` from `wake_wiring_test.go`.
- Produces: `(*Daemon) validateBroadcastTarget(project string) error` — nil for `""` or a known project; otherwise the exact error from Global Constraints. Both send_message and task_create call it when `to_kind=="broadcast"`.

- [ ] **Step 1: Write the failing tests**

Create `internal/daemon/broadcast_test.go`:

```go
package daemon

import (
	"strings"
	"testing"
)

// Scoped broadcast to a project nobody (non-departed) is registered under
// must be rejected at the daemon — the black-hole backstop, same principle
// as resolveAgentTarget for mistyped aliases.
func TestScopedBroadcastUnknownProjectRejected(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1"})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "web1", "to_kind": "broadcast", "to_target": "wbe", "subject": "typo", "body": "x",
	})
	if resp.OK {
		t.Fatalf("expected rejection for unknown project, got OK")
	}
	if !strings.Contains(resp.Error, `no registered agents in project "wbe"`) || !strings.Contains(resp.Error, "known projects: web") {
		t.Fatalf("error should name the project and list known ones, got: %q", resp.Error)
	}
}

func TestScopedBroadcastKnownProjectAccepted(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "web2", "project": "web", "socket_path": "/s", "session_id": "$2"})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "web1", "to_kind": "broadcast", "to_target": "web", "subject": "ok", "body": "x",
	})
	if !resp.OK {
		t.Fatalf("scoped broadcast to a live project should succeed: %s", resp.Error)
	}
}

func TestScopedBroadcastDepartedOnlyProjectRejected(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "solo", "project": "api", "socket_path": "/s", "session_id": "$2"})
	call(t, sock, "deregister_agent", map[string]any{"alias": "solo"})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "web1", "to_kind": "broadcast", "to_target": "api", "subject": "tombstones", "body": "x",
	})
	if resp.OK {
		t.Fatalf("broadcast to a departed-only project should be rejected")
	}
}

func TestGlobalBroadcastNeverValidated(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1"})
	resp := call(t, sock, "send_message", map[string]any{
		"from": "web1", "to_kind": "broadcast", "to_target": "", "subject": "all", "body": "x",
	})
	if !resp.OK {
		t.Fatalf("global broadcast must not be validated: %s", resp.Error)
	}
}

func TestScopedBroadcastTaskCreateValidatedToo(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1"})
	resp := call(t, sock, "task_create", map[string]any{
		"from": "web1", "to_kind": "broadcast", "to_target": "nope", "subject": "t", "body": "x",
	})
	if resp.OK {
		t.Fatalf("task_create must validate scoped broadcast targets too")
	}
}
```

The deregister op name is `deregister_agent` (`internal/daemon/daemon.go:655`); it tombstones the row (`departed=1`, project preserved), which is exactly what this test needs.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/ -run 'TestScopedBroadcast|TestGlobalBroadcast' -v`
Expected: FAIL — unknown/departed/task cases get OK responses (no validation exists yet).

- [ ] **Step 3: Implement the validator and wire it in**

In `internal/daemon/daemon.go`, next to `senderProject` (~line 437), add:

```go
// validateBroadcastTarget is the send-time backstop for scoped broadcasts
// (to_kind=broadcast, to_target=<project>): reject unless some non-departed
// agent is registered under exactly that project, so a typo'd project never
// creates a thread that concerns nobody — the same black-hole principle as
// resolveAgentTarget for mistyped aliases. A global broadcast (empty
// target) is never validated. Exact match only; project strings come from
// tmux capture and are stable, so no fuzzy or prefix matching.
func (d *Daemon) validateBroadcastTarget(project string) error {
	if project == "" {
		return nil
	}
	agents, err := d.s.ListAgents()
	if err != nil {
		return err
	}
	known := make(map[string]bool)
	for _, ag := range agents {
		if !ag.Departed && ag.Project != "" {
			known[ag.Project] = true
		}
	}
	if known[project] {
		return nil
	}
	names := make([]string, 0, len(known))
	for p := range known {
		names = append(names, p)
	}
	sort.Strings(names)
	return fmt.Errorf("no registered agents in project %q (known projects: %s)", project, strings.Join(names, ", "))
}
```

Add `"sort"` and `"strings"` to the imports if not present (`fmt` already is). Then in `dispatch`, in **both** the `case "send_message":` and `case "task_create":` blocks, immediately after the existing `if toKind == "agent" { ... }` resolution block, add:

```go
		if toKind == "broadcast" {
			if err := d.validateBroadcastTarget(toTarget); err != nil {
				return fail(err)
			}
		}
```

- [ ] **Step 4: Run the daemon package tests**

Run: `go test -race ./internal/daemon/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/
git commit -m "feat(daemon): validate scoped broadcast projects at send time"
```

---

### Task 3: Daemon — scoped fan-out and journal target

**Files:**
- Modify: `internal/daemon/daemon.go:362-366` (`notifyForThread` broadcast case)
- Modify: `internal/daemon/daemon.go:447-457` (`targetOf`)
- Test: `internal/daemon/broadcast_test.go` (extend)

**Interfaces:**
- Consumes: Task 2's registered-agents harness; `fakeNotifier.snap(&n.notified)`.
- Produces: scoped broadcasts notify only same-project sessions; journal/event rows carry target `broadcast:<project>` (Task 4's renderer handles this string).

- [ ] **Step 1: Write the failing tests**

Append to `internal/daemon/broadcast_test.go`:

```go
func TestScopedBroadcastNotifiesOnlyProjectSessions(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "web2", "project": "web", "socket_path": "/s", "session_id": "$2"})
	call(t, sock, "register_agent", map[string]any{"alias": "api1", "project": "api", "socket_path": "/s", "session_id": "$3"})

	call(t, sock, "send_message", map[string]any{
		"from": "web1", "to_kind": "broadcast", "to_target": "web", "subject": "s", "body": "x",
	})
	got := n.snap(&n.notified)
	if len(got) != 1 || got[0] != "$2" {
		t.Fatalf("scoped broadcast should notify only web2's session $2 (sender excluded, api1 out of scope), got %v", got)
	}
}

func TestTargetOfScopedBroadcast(t *testing.T) {
	if got := targetOf("broadcast", ""); got != "broadcast" {
		t.Fatalf("global: got %q", got)
	}
	if got := targetOf("broadcast", "web"); got != "broadcast:web" {
		t.Fatalf("scoped: got %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/ -run 'TestScopedBroadcastNotifies|TestTargetOfScoped' -v`
Expected: FAIL — api1's session `$3` is also notified; `targetOf` returns `broadcast` for the scoped case.

- [ ] **Step 3: Implement**

`notifyForThread` broadcast case (`daemon.go:362-366`) becomes:

```go
	case "broadcast":
		for _, a := range agents {
			if th.ToTarget == "" || a.Project == th.ToTarget {
				recipients[a.Alias] = struct{}{}
			}
		}
```

`targetOf` (`daemon.go:452-457`) becomes (extend its comment with: "a scoped broadcast journals 'broadcast:<project>'"):

```go
func targetOf(toKind, toTarget string) string {
	if toKind == "broadcast" {
		if toTarget == "" {
			return "broadcast"
		}
		return "broadcast:" + toTarget
	}
	return toKind + ":" + toTarget
}
```

- [ ] **Step 4: Run the daemon package tests**

Run: `go test -race ./internal/daemon/`
Expected: PASS (existing global-broadcast fan-out tests unchanged: empty target still matches every agent).

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/
git commit -m "feat(daemon): scoped broadcast fan-out and broadcast:<project> journal target"
```

---

### Task 4: Display — renderer and station know `broadcast:<project>`

**Files:**
- Modify: `internal/render/renderer.go:124-132` (`dispTarget`)
- Modify: `internal/station/model.go:1519-1530` (`dispToTarget`)
- Test: Create `internal/render/renderer_test.go` (the render package has no test files today — it is covered indirectly via humancli's events tests); add the station case to `internal/station/model_test.go`.

**Interfaces:**
- Consumes: Task 3's `broadcast:<project>` journal-target string; thread rows with `ToKind=="broadcast"`, `ToTarget!=""`.
- Produces: both surfaces display `broadcast:<project>` as-is (no alias/label resolution attempted on the project string).

- [ ] **Step 1: Write the failing tests**

Renderer — the danger is the fallthrough: `broadcast:web` misses every special case in `dispTarget` and lands in `r.disp(target)`, the bare-alias path, which label-resolves the whole string. Pin "a broadcast target is never label-resolved" by planting a label under the literal target string — current code returns the label, fixed code returns the target as-is. `NewRenderer`'s signature is `NewRenderer(rows []EventRow, labels map[string]string, aliases, fullTime bool, width int) *Renderer` (`renderer.go:43`).

Create `internal/render/renderer_test.go`:

```go
package render

import "testing"

// dispTarget must show broadcast targets as-is — never label-resolve them
// through the bare-alias fallthrough. The label planted under the literal
// target string proves the fallthrough is not taken.
func TestDispTargetScopedBroadcastShownAsIs(t *testing.T) {
	r := NewRenderer(nil, map[string]string{"broadcast:web": "WRONG"}, false, false, 120)
	if got := r.dispTarget("broadcast:web"); got != "broadcast:web" {
		t.Fatalf("scoped broadcast target rendered %q, want broadcast:web", got)
	}
	if got := r.dispTarget("broadcast"); got != "broadcast" {
		t.Fatalf("global broadcast target rendered %q, want broadcast", got)
	}
}
```

Station — append to `internal/station/model_test.go`:

```go
func TestDispToTargetScopedBroadcast(t *testing.T) {
	var m Model
	if got := m.dispToTarget(listThreadRow{ToKind: "broadcast", ToTarget: "web"}); got != "broadcast:web" {
		t.Fatalf("got %q, want broadcast:web", got)
	}
	if got := m.dispToTarget(listThreadRow{ToKind: "broadcast"}); got != "broadcast" {
		t.Fatalf("got %q, want broadcast", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/render/ ./internal/station/ -run 'ScopedBroadcast' -v`
Expected: renderer FAIL (fallthrough label-resolves to "WRONG"); station FAIL (returns bare `broadcast`, dropping the project).

- [ ] **Step 3: Implement**

`renderer.go:128` — add the prefix to the show-as-is branch:

```go
	if strings.HasPrefix(target, "role:") || target == "broadcast" || strings.HasPrefix(target, "broadcast:") || target == "" {
		return target
	}
```

`model.go:1525-1526`:

```go
	case "broadcast":
		if row.ToTarget != "" {
			return "broadcast:" + row.ToTarget
		}
		return "broadcast"
```

- [ ] **Step 4: Run both package tests**

Run: `go test -race ./internal/render/ ./internal/station/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/render/ internal/station/
git commit -m "feat(display): render scoped broadcast targets as broadcast:<project>"
```

---

### Task 5: MCP — advertise both broadcast forms (description-only)

**Files:**
- Modify: `internal/mcpserver/tools_messages.go:13-14,121` (SendMessageIn schema hints, send_message description)
- Modify: `internal/mcpserver/tools_tasks.go:13-14` (TaskCreateIn schema hints)

**Interfaces:**
- Consumes: nothing new — the handlers already pass `to_target` through verbatim; Task 2's daemon validation covers this surface automatically.
- Produces: tool schemas/descriptions that teach agents both forms. No handler code changes; no new tests (no behavior to test — the daemon tests own validation).

- [ ] **Step 1: Update the strings**

`tools_messages.go:13-14`:

```go
	ToKind   string `json:"to_kind" jsonschema:"agent, role, or broadcast"`
	ToTarget string `json:"to_target,omitempty" jsonschema:"the recipient alias or role; for broadcast: empty reaches every agent on the bus, or a project name reaches only that project's agents (unknown projects are rejected)"`
```

`tools_messages.go:121` description becomes:

```go
mcp.AddTool(srv, &mcp.Tool{Name: "send_message", Description: "Send a message to another agent (to_kind=agent), a role (to_kind=role), or many agents at once (to_kind=broadcast). A broadcast with empty to_target reaches every agent on the bus; set to_target to a project name to reach only that project's agents. Set intent to fyi/reply-requested/action-requested so the recipient's inbox and drain reflect what you actually need back."}, sendMessageHandler)
```

`tools_tasks.go:14`:

```go
	ToTarget string `json:"to_target" jsonschema:"the assignee alias or role; for broadcast: empty for every agent, or a project name for that project's agents only"`
```

- [ ] **Step 2: Build and run the package tests**

Run: `go build ./... && go test -race ./internal/mcpserver/`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/mcpserver/
git commit -m "docs(mcp): advertise global and project-scoped broadcast on tool schemas"
```

---

### Task 6: CLI — `--project` flag on `muster send --broadcast`

**Files:**
- Modify: `internal/humancli/humancli.go:186-277` (sendFlagVals, newSendFlagsWithVals, cmdSend)
- Modify: `internal/humancli/registry.go:94-100` (send synopsis + help prose)
- Test: new `internal/humancli/send_broadcast_test.go`. The harness is real, not stubbed: `startTestDaemon(t)` (`humancli_test.go:22`) starts a daemon on an isolated `MUSTER_HOME` and returns its `*store.Store`; `callData(op, args)` talks to it; `cmdSend(args, &buf)` does a full round trip (see `thread_test.go:18` for the pattern). Because the daemon is real, Task 2's validation applies — the scoped test must register an agent under the target project first.

**Interfaces:**
- Consumes: daemon `send_message` op accepting `to_kind=broadcast, to_target=<project>` (Tasks 1-3).
- Produces: `muster send --broadcast --project <p> "body"` sends `{to_kind: "broadcast", to_target: "<p>"}`; `--project` without `--broadcast` errors; plain `--broadcast` keeps joining ALL positionals into the body.

- [ ] **Step 1: Write the failing tests**

Create `internal/humancli/send_broadcast_test.go`:

```go
package humancli

import (
	"bytes"
	"strings"
	"testing"
)

func TestSendBroadcastProjectFlag(t *testing.T) {
	s := startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "w1", "project": "web", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := cmdSend([]string{"--broadcast", "--project", "web", "deploy", "landed", "--from", "tester"}, &buf); err != nil {
		t.Fatalf("scoped broadcast send: %v", err)
	}
	ths, err := s.Threads(10)
	if err != nil || len(ths) != 1 {
		t.Fatalf("threads: %v (%d)", err, len(ths))
	}
	if ths[0].ToKind != "broadcast" || ths[0].ToTarget != "web" {
		t.Fatalf("stored thread addressed %s:%q, want broadcast:web", ths[0].ToKind, ths[0].ToTarget)
	}
	_, entries, err := s.GetThread(ths[0].ID)
	if err != nil || len(entries) != 1 || entries[0].Body != "deploy landed" {
		t.Fatalf("unquoted body must join: %v / %+v", err, entries)
	}
}

func TestSendProjectWithoutBroadcastErrors(t *testing.T) {
	startTestDaemon(t)
	var buf bytes.Buffer
	err := cmdSend([]string{"--project", "web", "hello"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "--project requires --broadcast") {
		t.Fatalf("want '--project requires --broadcast' error, got %v", err)
	}
}

func TestSendBroadcastUnquotedBodyStaysGlobal(t *testing.T) {
	s := startTestDaemon(t)
	// "muster" is a real project on the roster — the exact collision the
	// rejected positional form would have silently mis-scoped.
	if _, err := callData("register_agent", map[string]any{"alias": "m1", "project": "muster", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := cmdSend([]string{"--broadcast", "muster", "is", "broken", "--from", "tester"}, &buf); err != nil {
		t.Fatalf("global broadcast send: %v", err)
	}
	ths, err := s.Threads(10)
	if err != nil || len(ths) != 1 {
		t.Fatalf("threads: %v (%d)", err, len(ths))
	}
	if ths[0].ToTarget != "" {
		t.Fatalf("unquoted broadcast body must stay global, got to_target=%q", ths[0].ToTarget)
	}
	_, entries, err := s.GetThread(ths[0].ID)
	if err != nil || len(entries) != 1 || entries[0].Body != "muster is broken" {
		t.Fatalf("body must join all positionals: %v / %+v", err, entries)
	}
}

func TestSendBroadcastUnknownProjectSurfacesDaemonError(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "w1", "project": "web", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err := cmdSend([]string{"--broadcast", "--project", "wbe", "typo", "--from", "tester"}, &buf)
	if err == nil || !strings.Contains(err.Error(), `no registered agents in project "wbe"`) {
		t.Fatalf("daemon validation error must surface through the CLI, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/humancli/ -run 'TestSendBroadcastProject|TestSendProjectWithout|TestSendBroadcastUnquoted' -v`
Expected: FAIL — `--project` is an unknown flag.

- [ ] **Step 3: Implement**

`sendFlagVals` gains the field:

```go
type sendFlagVals struct {
	from, subject, ref, intent, project *string
	role, broadcast                     *bool
}
```

`newSendFlagsWithVals` declares it:

```go
	v.project = fs.String("project", "", "with --broadcast: send only to this project's agents")
```

In `cmdSend`, after `validateIntent` (line 228-230) add:

```go
	if *v.project != "" && !*v.broadcast {
		return fmt.Errorf("--project requires --broadcast")
	}
```

and in the broadcast branch (lines 239-243), set the target:

```go
	if *v.broadcast {
		if len(rest) < 1 {
			return fmt.Errorf("usage: muster send --broadcast [--project <p>] <body> [--intent fyi|reply-requested|action-requested]")
		}
		toTarget = *v.project
		body = strings.Join(rest, " ")
	} else {
```

(`toTarget` is already sent on the wire at line 259 — no other change.)

`registry.go:94-100`: synopsis becomes

```
send <target> "body" [--from <alias>] [--subject <s>] [--ref <r>] [--role] [--broadcast [--project <p>]] [--intent fyi|reply-requested|action-requested]
```

and in the help prose, after the sentence about `--broadcast`, add: "With --project, the broadcast reaches only agents registered under that exact project (the daemon rejects unknown projects and lists the known ones)."

- [ ] **Step 4: Run the package tests**

Run: `go test -race ./internal/humancli/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/humancli/
git commit -m "feat(cli): muster send --broadcast --project <p> for scoped broadcasts"
```

---

### Task 7: Gate and end-to-end sanity

**Files:**
- No source changes expected; fixes only if the gate finds drift.

**Interfaces:**
- Consumes: everything above.
- Produces: a green `just verify` on the branch.

- [ ] **Step 1: Run the full gate**

Run: `cd /Users/courtschuett/GitHub/worktrees/muster-project-broadcast && just verify`
Expected: gofmt clean, golangci-lint clean, `go test -race ./...` PASS, build OK.

- [ ] **Step 2: End-to-end smoke on an isolated bus**

```bash
export MUSTER_HOME=/tmp/muster-pb-smoke
rm -rf $MUSTER_HOME && mkdir -p $MUSTER_HOME
go build -o /tmp/muster-pb-smoke/muster ./cmd/muster
M=/tmp/muster-pb-smoke/muster
MUSTER_ALIAS=w1 $M register w1
MUSTER_ALIAS=w2 $M register w2
$M send --broadcast --project nope "typo test" --from w1 || echo "REJECTED (expected)"
# registered project of w1/w2 is captured from tmux ("muster" when run from this repo's session)
$M send --broadcast --project muster "scoped hello" --from w1
$M inbox w2      # expect: TO broadcast:muster (or to_target column showing the project), 1 unread
$M send --broadcast "global hello" --from w1
$M inbox w2      # expect: 2 threads
$M events | tail -5   # expect a send row with target broadcast:muster
pkill -f "muster-pb-smoke/muster serve"; rm -rf /tmp/muster-pb-smoke
```

Expected: the `nope` send prints the known-projects error; the scoped and global sends both land in w2's inbox; the journal shows `broadcast:muster`.

- [ ] **Step 3: Commit any gate fixes, then final commit if dirty**

```bash
git status --short   # commit fixes with a descriptive message if the gate changed anything
```
