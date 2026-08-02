# Durable Alias Identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A resumed harness conversation (claude --resume into a brand-new tmux session) automatically reclaims its alias — inbox, threads, read-state — with the register response, hooks, and docs all teaching alias-as-durable-identity.

**Architecture:** The alias is already the durable key and `harness_session_id` already exists end-to-end (column, migration, store round-trip, CLI `--harness-session` flag, `harnessOwnedRows` lookup — all landed with paneless agents, PR #74). This feature adds: (1) a classified register response (new/refreshed/revived + unread count), (2) a lightweight stamp op so hooks can attach the harness session ID to already-registered aliases, (3) hook-driven reclaim on SessionStart `source:"resume"` with a summary printed to stdout (which Claude Code injects into the session's context), and (4) vocabulary fixes in skill/README. Spec: `docs/superpowers/specs/2026-08-01-durable-alias-identity-design.md`.

**Tech Stack:** Go, pure-Go SQLite (`modernc.org/sqlite`), newline-JSON daemon protocol (`internal/proto`), MCP go-sdk.

## Global Constraints

- **Work in a git worktree cut from `origin/dev`** (NEVER local `dev` — the primary clone lags): `git fetch origin && git worktree add ~/GitHub/worktrees/muster-durable-alias -b feat/durable-alias-identity origin/dev`. First commit: copy `docs/superpowers/specs/2026-08-01-durable-alias-identity-design.md` and this plan file from the primary clone into the worktree (they exist only in the primary clone's working tree).
- **`just verify` is the gate** (gofmt, golangci-lint, `go test -race`, build) — run before every commit claim of done.
- **cgo-free**: no new dependencies; `CGO_ENABLED=0` must keep building.
- **stdout is sacred in mcp mode** — all new diagnostics in mcpserver paths go to stderr or the tool result, never `fmt.Println`.
- **Daemon stays tmux- and harness-agnostic**: `internal/daemon`/`internal/store` treat `harness_session_id` as an opaque string; no `tmuxenv`/`harnessenv` imports there.
- **Hooks never block a session**: every new hook path swallows errors and degrades to today's behavior (`cmdHook` always returns nil).
- macOS tests: daemon-socket tests must use the existing `startTestDaemon`/`startWithNotifier` helpers (they use `mustertest.ShortHome()` for the sun_path limit) — never `t.TempDir()` for a socket dir.
- House style: doc comments are full-sentence, rationale-carrying (see any function in `hook.go`); Go doc comments in this repo deliberately render empty-string as `''` (U+2019 pair) — don't "fix" existing ones.

## Interfaces established by this plan (cross-task contract)

- Daemon `register_agent` response `Data`: `{"outcome": "new"|"refreshed"|"revived", "unread": <int>}`.
- New daemon op `stamp_harness_session`, args `{"alias": string, "harness_session_id": string}` → `ok(nil)`.
- New store method: `func (s *Store) SetHarnessSessionID(alias, id string) error`.
- humancli: `type registerAck struct { Outcome string `+"`json:\"outcome\"`"+`; Unread int `+"`json:\"unread\"`"+` }` and `func decodeRegisterAck(raw json.RawMessage) registerAck` (zero value on any decode failure).
- humancli: `reviveRow(ag agentRow, model string) registerAck` (signature gains the return; existing callers may discard it).
- humancli: `func hookSessionStartResume(c tmuxenv.Capture, h harnessenv.Capture, model string, out io.Writer) bool` — reclaims owned rows onto the current tmux tuple; true when ≥1 alias was reclaimed (caller then skips the default register).

---

### Task 1: Daemon — classified register response with unread count

**Files:**
- Modify: `internal/daemon/daemon.go` (handleRegisterAgent, ~line 205)
- Test: `internal/daemon/register_outcome_test.go` (new)

**Interfaces:**
- Consumes: existing `d.s.GetAgent` / `d.s.RegisterAgent` / `d.s.UnreadCount` / `d.logEvent`.
- Produces: `register_agent` response Data `{"outcome": ..., "unread": ...}`; journal event `Kind:"register", Agent:<alias>, Detail:"revived"` on revival only.

- [ ] **Step 1: Write the failing test**

```go
package daemon

import "testing"

// TestRegisterAgentOutcomeClassification covers the three register outcomes
// the durable-alias spec defines: a first-sight alias is "new", an upsert
// over a live row is "refreshed", and an upsert over a tombstone is
// "revived" — with the response carrying the alias's unread count so a
// resuming session learns its backlog in the same call.
func TestRegisterAgentOutcomeClassification(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})

	resp := call(t, sock, "register_agent", map[string]any{
		"alias": "backend", "socket_path": "/s", "session_id": "$1",
	})
	if !resp.OK {
		t.Fatalf("register: %+v", resp)
	}
	data, _ := resp.Data.(map[string]any)
	if data["outcome"] != "new" {
		t.Fatalf("first register outcome = %v, want new", data["outcome"])
	}

	resp = call(t, sock, "register_agent", map[string]any{
		"alias": "backend", "socket_path": "/s", "session_id": "$1",
	})
	data, _ = resp.Data.(map[string]any)
	if data["outcome"] != "refreshed" {
		t.Fatalf("re-register outcome = %v, want refreshed", data["outcome"])
	}

	// Mail lands while the agent is departed; revival must report it.
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender", "socket_path": "/s", "session_id": "$2",
	})
	call(t, sock, "send_message", map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "backend",
		"subject": "while you were away", "body": "ping",
	})
	call(t, sock, "deregister_agent", map[string]any{"alias": "backend"})

	resp = call(t, sock, "register_agent", map[string]any{
		"alias": "backend", "socket_path": "/s3", "session_id": "$9",
	})
	data, _ = resp.Data.(map[string]any)
	if data["outcome"] != "revived" {
		t.Fatalf("revival outcome = %v, want revived", data["outcome"])
	}
	if n, _ := data["unread"].(float64); n < 1 {
		t.Fatalf("revival unread = %v, want >= 1", data["unread"])
	}

	// Revival is journaled so watch/station show a returning agent.
	ev := call(t, sock, "list_events", map[string]any{"agent": "backend"})
	if !containsEventDetail(t, ev, "register", "revived") {
		t.Fatalf("no register/revived journal event after revival: %+v", ev.Data)
	}
}
```

Add the small helper in the same file (decode `ev.Data` — a `[]any` of `map[string]any` over the wire — and return whether any row has `kind=="register"` and `detail=="revived"`):

```go
func containsEventDetail(t *testing.T, resp proto.Response, kind, detail string) bool {
	t.Helper()
	rows, _ := resp.Data.([]any)
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if m["kind"] == kind && m["detail"] == detail {
			return true
		}
	}
	return false
}
```

(Import `github.com/schuettc/muster/internal/proto`. Check `list_events`' arg names against `internal/daemon/daemon.go` case `"list_events"` — the concern filter arg is `agent`; mirror whatever `internal/humancli/events.go` sends. If the events op requires more args, seed exactly what `events_test.go` does.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestRegisterAgentOutcomeClassification -race`
Expected: FAIL — `data["outcome"]` is nil (register currently returns `ok(nil)`).

- [ ] **Step 3: Implement in handleRegisterAgent**

In `internal/daemon/daemon.go`, replace the final `return ok(nil)` of `handleRegisterAgent` and classify from the already-fetched pre-mutation row:

```go
	// Outcome classification (durable-alias spec change 1): the pre-mutation
	// row read above already tells this apart for free — no prior row is a
	// first sight, a live prior row is a tuple refresh, and a tombstone is a
	// returning session (RegisterAgent's upsert just revived it). The unread
	// count rides along so a resuming session learns its backlog in the same
	// call; a count failure degrades to 0 rather than failing a register that
	// already succeeded.
	outcome := "new"
	switch {
	case hadOld && old.Departed:
		outcome = "revived"
		d.logEvent(store.Event{Kind: "register", Agent: alias, Detail: "revived"})
	case hadOld:
		outcome = "refreshed"
	}
	unread, err := d.s.UnreadCount(alias)
	if err != nil {
		unread = 0
	}
	return ok(map[string]any{"outcome": outcome, "unread": unread})
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/daemon/ -race`
Expected: PASS — including all existing register/CAS/badge tests (they ignore response Data, so nothing else should move).

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/daemon.go internal/daemon/register_outcome_test.go
git commit -m "feat(daemon): register_agent classifies new/refreshed/revived and returns unread"
```

---

### Task 2: Store + daemon — stamp_harness_session op

**Files:**
- Modify: `internal/store/agents.go`, `internal/daemon/daemon.go` (dispatch switch)
- Test: `internal/store/agents_test.go` (append), `internal/daemon/daemon_test.go` (append)

**Interfaces:**
- Produces: `Store.SetHarnessSessionID(alias, id string) error`; daemon op `stamp_harness_session` `{"alias", "harness_session_id"}` → `ok(nil)`.

- [ ] **Step 1: Write the failing store test** (append to `internal/store/agents_test.go`, mirroring its existing open-store helper — reuse whatever `TestRegisterAgent*` there uses to get a `*Store`):

```go
// TestSetHarnessSessionID covers the hook-repair path of the durable-alias
// spec: an alias registered without a harness link (e.g. via the MCP tool in
// an env with no harness UUID) gets one stamped later by the Stop hook.
func TestSetHarnessSessionID(t *testing.T) {
	s := openTestStore(t) // use this file's existing store-opening helper name
	if err := s.RegisterAgent(Agent{Alias: "backend"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHarnessSessionID("backend", "uuid-1"); err != nil {
		t.Fatal(err)
	}
	ag, ok, err := s.GetAgent("backend")
	if err != nil || !ok {
		t.Fatalf("get: %v %v", ok, err)
	}
	if ag.HarnessSessionID != "uuid-1" {
		t.Fatalf("harness_session_id = %q, want uuid-1", ag.HarnessSessionID)
	}
	// Unknown alias is a no-op, mirroring TouchAgent's contract.
	if err := s.SetHarnessSessionID("ghost", "uuid-2"); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run it** — `go test ./internal/store/ -run TestSetHarnessSessionID -race` — expect FAIL (method undefined).

- [ ] **Step 3: Implement the store method** (in `internal/store/agents.go`, next to `TouchAgent`):

```go
// SetHarnessSessionID stamps the harness session UUID onto an existing row —
// the repair half of the durable-alias spec: an alias registered without a
// harness link (an MCP register in an env carrying no harness UUID) gets one
// attached later by a hook that DOES see the UUID (every hook payload carries
// it). Identity, tuple, and read-state are untouched; unknown alias is a
// no-op, mirroring TouchAgent's contract.
func (s *Store) SetHarnessSessionID(alias, id string) error {
	_, err := s.db.Exec(`UPDATE agents SET harness_session_id=? WHERE alias=?`, id, alias)
	return err
}
```

- [ ] **Step 4: Write the failing daemon test** (append to `internal/daemon/daemon_test.go`):

```go
// TestStampHarnessSession covers the stamp op end to end: register without a
// harness link, stamp, and see the link in list_agents.
func TestStampHarnessSession(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{
		"alias": "backend", "socket_path": "/s", "session_id": "$1",
	})
	resp := call(t, sock, "stamp_harness_session", map[string]any{
		"alias": "backend", "harness_session_id": "uuid-1",
	})
	if !resp.OK {
		t.Fatalf("stamp: %+v", resp)
	}
	list := call(t, sock, "list_agents", nil)
	rows, _ := list.Data.([]any)
	m, _ := rows[0].(map[string]any)
	if m["harness_session_id"] != "uuid-1" {
		t.Fatalf("harness_session_id = %v, want uuid-1", m["harness_session_id"])
	}
}
```

- [ ] **Step 5: Run it** — expect FAIL (unknown op). Then add the dispatch case in `internal/daemon/daemon.go`, next to `case "set_label"`:

```go
	case "stamp_harness_session":
		if err := d.s.SetHarnessSessionID(str(a, "alias"), str(a, "harness_session_id")); err != nil {
			return fail(err)
		}
		return ok(nil)
```

- [ ] **Step 6: Run both packages** — `go test ./internal/store/ ./internal/daemon/ -race` — expect PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/store/agents.go internal/store/agents_test.go internal/daemon/daemon.go internal/daemon/daemon_test.go
git commit -m "feat(store,daemon): stamp_harness_session op attaches a harness link to an existing alias"
```

---

### Task 3: CLI — register prints outcome + unread

**Files:**
- Modify: `internal/humancli/identity.go` (cmdRegister), `internal/humancli/paneless.go` (reviveRow)
- Create: `internal/humancli/register_ack.go`
- Test: `internal/humancli/identity_test.go` (append)

**Interfaces:**
- Consumes: Task 1's response Data.
- Produces: `registerAck`, `decodeRegisterAck(raw json.RawMessage) registerAck`, `func (a registerAck) line(alias string) string`, `reviveRow(...) registerAck`.

- [ ] **Step 1: Write the failing test** (append to `identity_test.go`; the file's existing tests show how to run `cmdRegister` against `startTestDaemon(t)` with `t.Setenv("TMUX", ...)` + a `tmuxenv.Run` stub — mirror one):

```go
// TestRegisterPrintsRevivalAndUnread covers the resume loop's CLI surface:
// re-registering a departed alias that accrued mail must say so, so the
// operator (or a resumed agent using the CLI) learns the backlog in the
// same command.
func TestRegisterPrintsRevivalAndUnread(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("MUSTER_ALIAS", "")
	seed := func(op string, args map[string]any) {
		t.Helper()
		if _, err := callData(op, args); err != nil {
			t.Fatal(err)
		}
	}
	seed("register_agent", map[string]any{"alias": "backend", "socket_path": "/s", "session_id": "$1"})
	seed("register_agent", map[string]any{"alias": "sender", "socket_path": "/s", "session_id": "$2"})
	seed("send_message", map[string]any{"from": "sender", "to_kind": "agent", "to_target": "backend", "subject": "hi", "body": "b"})
	seed("deregister_agent", map[string]any{"alias": "backend"})

	var buf bytes.Buffer
	if err := cmdRegister([]string{"backend"}, &buf); err != nil {
		t.Fatalf("register: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "revived") || !strings.Contains(out, "1 unread") {
		t.Fatalf("register output missing revival/unread notice:\n%s", out)
	}
}
```

(Note: with `TMUX` empty and an explicit positional alias, cmdRegister takes the tmux-anchored code path with empty capture → the `paneless` half-capture guard stores the clean paneless shape; the daemon response decode is what's under test. If the empty-env path errors in practice, set `TMUX`/`TMUX_PANE` and stub `tmuxenv.Run` exactly as `TestHookStopUnreadEmitsBlockDecision` does instead.)

- [ ] **Step 2: Run it** — `go test ./internal/humancli/ -run TestRegisterPrintsRevivalAndUnread -race` — expect FAIL (no such output).

- [ ] **Step 3: Implement.** New file `internal/humancli/register_ack.go`:

```go
package humancli

import (
	"encoding/json"
	"fmt"
)

// registerAck decodes register_agent's response data (durable-alias spec
// change 1): how the daemon classified this registration, and the alias's
// unread-thread count at that moment.
type registerAck struct {
	Outcome string `json:"outcome"`
	Unread  int    `json:"unread"`
}

// decodeRegisterAck tolerantly decodes a register_agent response. A daemon
// predating the outcome field (or any decode failure) yields the zero value,
// whose line() renders nothing — callers degrade to today's output.
func decodeRegisterAck(raw json.RawMessage) registerAck {
	var a registerAck
	_ = json.Unmarshal(raw, &a)
	return a
}

// line renders the human-facing follow-up for a registration worth
// remarking on: a revival, or pending mail. "" for an unremarkable new or
// refreshed registration with an empty inbox — the existing "registered X"
// line already says everything.
func (a registerAck) line(alias string) string {
	if a.Outcome != "revived" && a.Unread == 0 {
		return ""
	}
	msg := fmt.Sprintf("reconnected: identity '%s' %s", alias, a.Outcome)
	if a.Unread > 0 {
		msg += fmt.Sprintf(" — %d unread thread(s); run `muster inbox '%s'`", a.Unread, alias)
	}
	return msg + "\n"
}
```

In `cmdRegister` (identity.go), capture and print on the main path — change:

```go
	if _, err := callData("register_agent", map[string]any{
```

to:

```go
	raw, err := callData("register_agent", map[string]any{
```

adjust the error check to `if err != nil { return err }` and after the existing `registered %s ...` Fprintf add:

```go
	if s := decodeRegisterAck(raw).line(alias); s != "" {
		if _, err := fmt.Fprint(out, s); err != nil {
			return err
		}
	}
```

In `paneless.go`, `reviveRow` returns the ack (existing callers compile unchanged only if they ignore returns — `hookSessionStartPaneless` and cmdRegister's paneless branch call it as a statement today; a statement call of a value-returning function is legal Go, so nothing else changes):

```go
func reviveRow(ag agentRow, model string) registerAck {
	if model == "" {
		model = ag.ModelType
	}
	raw, _ := callData("register_agent", map[string]any{ /* ...unchanged args... */ })
	return decodeRegisterAck(raw)
}
```

In cmdRegister's paneless owned-rows branch, print the ack line after the existing `registered %s (existing identity...)` Fprintf:

```go
			if s := reviveRow(owned[0], *model).line(alias); s != "" {
				_, _ = fmt.Fprint(out, s)
			}
```

(replacing the bare `reviveRow(owned[0], *model)` statement).

- [ ] **Step 4: Run the package** — `go test ./internal/humancli/ -race` — expect PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/humancli/register_ack.go internal/humancli/identity.go internal/humancli/paneless.go internal/humancli/identity_test.go
git commit -m "feat(cli): register reports revival and unread backlog"
```

---

### Task 4: MCP — register stamps the harness link and reports the outcome

**Files:**
- Modify: `internal/mcpserver/tools_registry.go`
- Test: `internal/mcpserver/tools_registry_test.go` (append)

**Interfaces:**
- Consumes: Task 1's response Data; existing `harnessenv.FromEnv()`.
- Produces: `register_agent` MCP Detail carrying outcome + unread; `harness_session_id` in the daemon args.

- [ ] **Step 1: Write the failing test** (append; mirror `TestRegisterAgentCapturesTmuxEnv`'s `callDaemon` stub):

```go
// TestRegisterAgentStampsHarnessLinkAndReportsRevival covers the durable-alias
// spec's MCP surface: the handler forwards the ambient harness session UUID
// (so hook reclaim can find this row after a resume), and folds the daemon's
// outcome/unread into the Detail so a re-registering agent learns its backlog.
func TestRegisterAgentStampsHarnessLinkAndReportsRevival(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%6")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "uuid-7")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		switch args[len(args)-1] {
		case "#{session_id}":
			return "$5", nil
		case "#{session_name}":
			return "muster-2", nil
		default:
			return "", nil
		}
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var got map[string]any
	prevDaemon := callDaemon
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		if op == "register_agent" {
			got = args
			return []byte(`{"outcome":"revived","unread":3}`), nil
		}
		return []byte(`[]`), nil // paneRegistration's list_agents probe
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, out, err := registerAgentHandler(context.TODO(), nil, RegisterAgentIn{Alias: "backend", Role: "peer", ModelType: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if got["harness_session_id"] != "uuid-7" {
		t.Fatalf("harness_session_id = %v, want uuid-7", got["harness_session_id"])
	}
	if !strings.Contains(out.Detail, "revived") || !strings.Contains(out.Detail, "3 unread") {
		t.Fatalf("Detail = %q, want revival + unread notice", out.Detail)
	}
}
```

(Verify how `paneRegistration` fetches the roster — read `internal/mcpserver/validate.go` on the worktree; adjust the stub's non-register return to whatever shape it decodes.)

- [ ] **Step 2: Run it** — `go test ./internal/mcpserver/ -run TestRegisterAgentStampsHarness -race` — expect FAIL.

- [ ] **Step 3: Implement.** In `registerAgentHandler`: hoist `h := harnessenv.FromEnv()` above the paneless branch (the branch currently declares its own `h` — reuse the hoisted one), add `"harness_session_id": h.SessionID` to the `callDaemon` args map, capture the response, and build the Detail:

```go
	raw, err := callDaemon("register_agent", map[string]any{ /* existing args + harness_session_id */ })
	if err != nil {
		return nil, OKOut{}, err
	}
	var ack struct {
		Outcome string `json:"outcome"`
		Unread  int    `json:"unread"`
	}
	_ = json.Unmarshal(raw, &ack)
	detail := "registered " + in.Alias
	if ack.Outcome == "revived" {
		detail = fmt.Sprintf("reconnected as '%s' — revived a previous registration", in.Alias)
	}
	if ack.Unread > 0 {
		detail += fmt.Sprintf("; %d unread thread(s): call get_inbox with alias '%s'", ack.Unread, in.Alias)
	}
	return nil, OKOut{OK: true, Detail: detail}, nil
```

- [ ] **Step 4: Run the package** — `go test ./internal/mcpserver/ -race` — expect PASS (existing capture test still passes: it asserts specific keys, not the absence of others; if it asserts the full map, add the new key to its expectation).

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/tools_registry.go internal/mcpserver/tools_registry_test.go
git commit -m "feat(mcp): register_agent stamps the harness link and reports revival/unread"
```

---

### Task 5: tmuxenv — ancestry-based pane capture + `muster whereami`

**Context (verified on thread 149 with the dotfiles session):** harnesses spawn
hooks with a STRIPPED environment — `$TMUX`/`$TMUX_PANE` unset — so
`tmuxenv.CaptureEnv()` comes back empty inside a hook even when the session
lives in a pane. The proven fix (shipped in dotfiles
`config/claude/claude-notify.sh`) is to walk the hook's process ancestry
(hook → claude → pane shell) until a PID matches a tmux pane's
`#{pane_pid}`. Muster builds its own copy in tmuxenv; dotfiles keeps theirs
(independent installability — settled on thread 149, entry 878). The two are
aligned by a four-point CONTRACT, quoted in the spec: walk outward; match
across the `proj-*` sockets (the notify script's default-socket-only query
is NOT enough — this machine runs one tmux server per project); first match
wins; fail-safe empty, never a cwd guess or broadcast. Read
`~/dotfiles/config/claude/claude-notify.sh` before implementing and lift its
matching behavior — it is the version with real mileage. `muster whereami`
is for muster's own hooks and operator convenience only; do not pitch it as
a dotfiles dependency anywhere in help text or docs.

**Files:**
- Create: `internal/tmuxenv/ancestry.go`
- Test: `internal/tmuxenv/ancestry_test.go`
- Modify: `internal/humancli/registry.go` (+ whichever file pattern the registry uses for a new command — mirror how `cmdNudge` or `cmdLabel` is wired), new `internal/humancli/whereami.go`
- Test: `internal/humancli/whereami_test.go`

**Interfaces:**
- Produces: `tmuxenv.CaptureFromAncestry() Capture` — same `Capture` struct `CaptureEnv` returns, zero value when no pane matches.
- Produces (injectable for tests, mirroring the existing `tmuxenv.Run` var pattern): `tmuxenv.AncestorPIDs func() []int` and `tmuxenv.SocketDir func() string`.
- Produces: CLI verb `muster whereami` — prints `socket=<path> session_id=<id> session_name=<name> pane=<id> created=<ts>` (one line), `--json` for a JSON object, empty stdout + exit 1 when no pane matches.

- [ ] **Step 1: Write the failing tmuxenv test:**

```go
package tmuxenv

import "testing"

// TestCaptureFromAncestryMatchesPanePID: the walk must find the pane whose
// #{pane_pid} is one of this process's ancestors, capture the full tuple
// from THAT socket, and come back empty (fail-safe) when no pane matches —
// never a guess.
func TestCaptureFromAncestryMatchesPanePID(t *testing.T) {
	prevRun, prevAnc, prevDir := Run, AncestorPIDs, SocketDir
	t.Cleanup(func() { Run, AncestorPIDs, SocketDir = prevRun, prevAnc, prevDir })

	AncestorPIDs = func() []int { return []int{999, 4242, 1} }

	Run = func(args ...string) (string, error) {
		// list-panes across sockets: only the proj-muster socket has our pane.
		switch {
		case contains(args, "list-panes") && containsSuffix(args, "proj-muster"):
			return "4242\t%7\t$3\tmuster-2\t555", nil
		case contains(args, "list-panes"):
			return "100\t%1\t$1\tother\t111", nil
		default:
			return "", nil
		}
	}
	// Socket enumeration: SocketDir points at a temp dir holding two fake
	// socket files so CaptureFromAncestry's glob finds both "servers".
	dir := t.TempDir()
	for _, name := range []string{"proj-other", "proj-muster"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	SocketDir = func() string { return dir }

	c := CaptureFromAncestry()
	if c.SocketPath == "" || c.PaneID != "%7" || c.SessionID != "$3" || c.SessionCreated != 555 {
		t.Fatalf("capture = %+v, want pane %%7 on $3 created 555", c)
	}

	AncestorPIDs = func() []int { return []int{999, 1} } // no pane pid in chain
	if c := CaptureFromAncestry(); c.SocketPath != "" || c.PaneID != "" {
		t.Fatalf("no-match capture = %+v, want zero value (fail-safe)", c)
	}
}
```

(Write `contains(args []string, want string) bool` and
`containsSuffix(args []string, suffix string) bool` as small helpers —
`containsSuffix` matches the `-S <path>` socket argument by its basename.
Imports: `os`, `path/filepath`, `testing`. The test's CONTRACT is fixed:
ancestor-pid match selects the socket and pane; no match yields the zero
Capture. If `Run`'s real call shape targets sockets differently than a
leading `-S` — check `tmuxenv.go`'s `query` helper — adjust the stub's
match to the real arg shape, keeping both assertions.)

- [ ] **Step 2: Run it** — `go test ./internal/tmuxenv/ -run TestCaptureFromAncestry -race` — expect FAIL (function undefined).

- [ ] **Step 3: Implement `internal/tmuxenv/ancestry.go`:**

```go
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
```

with `capturePane` assembling a `Capture` the way `CaptureEnv` does for its
fields — socket, pane, session id/name/created directly from the match, and
Project/Label via the SAME queries CaptureEnv uses given a socket + pane
(read `tmuxenv.go`'s `CaptureEnv`/`query` and reuse those helpers verbatim;
if `Run` doesn't already accept a leading `-S`, mirror however `query`
targets a socket).

Adjust the Step 1 test mechanics to these seams (fake socket files in
`SocketDir()`'s dir so the glob finds them).

- [ ] **Step 4: Run it** — expect PASS. Also `go test ./internal/tmuxenv/ -race` for the package.

- [ ] **Step 5: Write the failing CLI test** (`internal/humancli/whereami_test.go`):

```go
package humancli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// TestWhereamiPrintsResolvedTuple: the verb is the walk's CLI face, for
// muster's own hooks and operators (dotfiles keeps its own walk — the two
// share a behavioral contract, not a binary; thread 149).
func TestWhereamiPrintsResolvedTuple(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	prev := tmuxenv.AncestorPIDs
	// ... stub AncestorPIDs/SocketDir/Run exactly as the tmuxenv test does,
	// yielding pane %7 / $3 / muster-2 / created 555 on socket S ...
	t.Cleanup(func() { tmuxenv.AncestorPIDs = prev })

	var buf bytes.Buffer
	if err := Dispatch([]string{"whereami"}, &buf); err != nil {
		t.Fatalf("whereami: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"session_id=$3", "pane=%7", "session_name=muster-2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("whereami output missing %q:\n%s", want, out)
		}
	}
}

// TestWhereamiFailsWhenUnresolvable: no env, no ancestry match → empty
// stdout and a non-nil error (the CLI maps it to a nonzero exit).
func TestWhereamiFailsWhenUnresolvable(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	// stub AncestorPIDs to pids matching no pane; Run to return no panes
	var buf bytes.Buffer
	if err := Dispatch([]string{"whereami"}, &buf); err == nil {
		t.Fatalf("expected an error when no pane resolves, got output %q", buf.String())
	}
}
```

- [ ] **Step 6: Implement `cmdWhereami`** in `internal/humancli/whereami.go`: env capture first (`tmuxenv.CaptureEnv()`), ancestry walk as fallback, `--json` flag via the registry's flag pattern. Register it in `registry.go` mirroring an existing simple command (grouped under the identity/hooks group; help text: "print the tmux identity of the pane this process runs under — resolves via $TMUX when present, else by walking process ancestry (works inside env-stripped harness hooks)").

- [ ] **Step 7: Run the package** — `go test ./internal/humancli/ ./internal/tmuxenv/ -race` — expect PASS. Run `just verify` (registry changes touch help/man goldens if any — fix them per their test output).

- [ ] **Step 8: Commit**

```bash
git add internal/tmuxenv/ancestry.go internal/tmuxenv/ancestry_test.go internal/humancli/whereami.go internal/humancli/whereami_test.go internal/humancli/registry.go
git commit -m "feat(tmuxenv): ancestry-based pane capture + muster whereami"
```

---

### Task 6: Hooks — SessionStart passes the payload's harness ID; Stop repairs missing links

**Files:**
- Modify: `internal/humancli/hook.go` (cmdHook SessionStart tmux branch; hookStop tmux path)
- Test: `internal/humancli/hook_test.go` (append)

**Interfaces:**
- Consumes: `harnessenv.FromHookPayload`, CLI `--harness-session` flag (exists), Task 2's `stamp_harness_session` op.
- Produces: tmux-anchored SessionStart registrations carry `harness_session_id`; Stop stamps owned, link-less aliases (only when the mail gate already opened — the cheap `@muster_inbox` gate stays the only thing that decides whether Stop dials the daemon at all).

- [ ] **Step 1: Write the failing Stop-repair test** (append to `hook_test.go`; mirror `TestHookStopUnreadEmitsBlockDecision`'s daemon + `tmuxenv.Run` + env setup — that test registers aliases on tuple `("/tmp/sock", "$1")`, stubs `@muster_inbox` > 0, and runs `cmdHook([]string{"Stop"}, ...)`):

```go
// TestHookStopStampsHarnessLink covers the repair half of the durable-alias
// spec: a custom alias registered via the MCP tool (no harness link) gets the
// payload's session_id stamped when the Stop hook fires for real mail — so a
// later resume can find the row. The stamp piggybacks on the mail gate: no
// mail, no daemon dials, no stamp.
func TestHookStopStampsHarnessLink(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	if _, err := callData("register_agent", map[string]any{
		"alias": "backend", "socket_path": "/tmp/sock", "session_id": "$1", "pane_id": "%1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": "sender", "socket_path": "/tmp/sock", "session_id": "$2",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "backend", "subject": "s", "body": "b",
	}); err != nil {
		t.Fatal(err)
	}
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"@muster_inbox":    "1",
		"#{session_id}":    "$1",
		"#{session_name}":  "backend",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{"session_id":"uuid-9"}`), &buf); err != nil {
		t.Fatal(err)
	}
	ag, ok := hookGetAgent("backend")
	if !ok || ag.HarnessSessionID != "uuid-9" {
		t.Fatalf("harness link after Stop = %q (found=%v), want uuid-9", ag.HarnessSessionID, ok)
	}
}
```

(The `tmuxenv.Run` stub must also satisfy `tmuxenv.SocketFromEnv` — that reads `$TMUX`, not Run — and the label option lookup; `hookRun` returns "" for unknown keys, which is fine. If `SocketFromEnv` yields `/tmp/sock` from the TMUX env var, the registered tuple matches.)

- [ ] **Step 2: Run it** — expect FAIL (link stays empty).

- [ ] **Step 3: Implement both halves in `hook.go`.**

First, the capture helper both hook tasks build on — hooks run env-stripped
(verified, thread 149), so env capture alone lands every pane-launched
session in the paneless branch; the ancestry walk (Task 5) is the fallback
that recovers the pane:

```go
// hookCapture resolves the tmux identity a hook acts on: the environment
// when the harness passed it through, else the process-ancestry walk —
// hooks are spawned env-stripped, but a synchronous hook is still a
// descendant of its pane's shell (see tmuxenv.CaptureFromAncestry). The
// zero Capture (both paths empty) means genuinely paneless.
func hookCapture() tmuxenv.Capture {
	if c := tmuxenv.CaptureEnv(); c.SocketPath != "" && c.PaneID != "" {
		return c
	}
	return tmuxenv.CaptureFromAncestry()
}
```

SessionStart branch of `cmdHook` — use it, and thread the payload's harness
ID into the register (the `--harness-session` flag already exists on
cmdRegister; an empty value falls back to `FromEnv` inside it, so pass
unconditionally). NOTE: `cmdRegister` itself reads `tmuxenv.CaptureEnv()`
internally, which is empty in a stripped hook — so when the ancestry walk
(not the env) produced the capture, register through a direct
`register_agent` call with the walked tuple (the `reclaimRow`-style args
shape from Task 7) rather than through cmdRegister. Structure the branch so
both paths stamp the harness ID:

```go
	case "SessionStart":
		c := hookCapture()
		h := harnessenv.FromHookPayload(payload)
		if c.SocketPath != "" && c.PaneID != "" {
			if hookMayClaimIdentity(c) {
				hookRegisterPane(c, h, model)
			}
		} else {
			hookSessionStartPaneless(h, model)
		}
```

with:

```go
// hookRegisterPane registers the session-name alias for a pane-anchored
// SessionStart. It cannot delegate to cmdRegister: that reads the tmux
// identity from the ENVIRONMENT, which a stripped hook doesn't have — the
// capture c (env or ancestry walk) is the truth here.
func hookRegisterPane(c tmuxenv.Capture, h harnessenv.Capture, model string) {
	alias := hookAlias(c)
	if alias == "" {
		return
	}
	_, _ = callData("register_agent", map[string]any{
		"alias": alias, "role": "", "model_type": model,
		"session_name": c.SessionName, "session_id": c.SessionID,
		"session_created":    c.SessionCreated,
		"harness_session_id": h.SessionID,
		"socket_path":        c.SocketPath, "pane_id": c.PaneID,
		"project": c.Project, "label": c.Label, "label_manual": c.LabelManual,
	})
}
```

(`hookAlias` currently takes the env-derived Capture — it already accepts a
`tmuxenv.Capture` parameter, so it composes with the walked capture as-is.)

Stop repair — in `hookStop`, after the `aliases := sessionAliasesForHook(...)` line and the ownership gate, before building the reason:

```go
	// Repair missing harness links (durable-alias spec: the stamp's Stop
	// half). An alias registered via the MCP tool in an env carrying no
	// harness UUID has no link for a future resume to find; every Stop
	// payload carries the UUID, so stamp it here. This runs only when the
	// mail gate above already opened — the cheap @muster_inbox check stays
	// the sole decider of whether Stop dials the daemon at all, so a
	// mail-less session costs nothing (documented residual: a link-less
	// alias that never receives mail is never auto-stamped and rides the
	// re-register-by-transcript contract instead).
	stampHarnessLinks(aliases, harnessenv.FromHookPayload(payload), socketPath, sessionID)
```

with, as a new function in `hook.go`:

```go
// stampHarnessLinks attaches h.SessionID to every alias of MY tuple that
// lacks a harness link. Best-effort per alias: a failed get or stamp is
// skipped — hooks never block a session.
func stampHarnessLinks(aliases []string, h harnessenv.Capture, socketPath, sessionID string) {
	if h.SessionID == "" {
		return
	}
	for _, alias := range aliases {
		ag, ok := hookGetAgent(alias)
		if !ok || ag.Departed || ag.HarnessSessionID != "" ||
			ag.SocketPath != socketPath || ag.SessionID != sessionID {
			continue
		}
		_, _ = callData("stamp_harness_session", map[string]any{
			"alias": alias, "harness_session_id": h.SessionID,
		})
	}
}
```

- [ ] **Step 4: Run the package** — `go test ./internal/humancli/ -race` — expect PASS (existing SessionStart tests must still pass: the extra flag is inert when the payload has no session_id and the env fallback is empty).

- [ ] **Step 5: Commit**

```bash
git add internal/humancli/hook.go internal/humancli/hook_test.go
git commit -m "feat(hook): SessionStart carries the harness link; Stop repairs link-less aliases"
```

---

### Task 7: Hooks — reclaim on SessionStart source:resume, summary into context

**Files:**
- Modify: `internal/humancli/hook.go` (cmdHook SessionStart branch), `internal/humancli/paneless.go` (new reclaim helper next to reviveRow)
- Test: `internal/humancli/hook_test.go` (append)

**Interfaces:**
- Consumes: `harnessOwnedRows`, `tmuxenv.IsSessionAlive`, Task 3's `decodeRegisterAck`/`line`.
- Produces: `hookSessionStartResume(c tmuxenv.Capture, h harnessenv.Capture, model string, out io.Writer) bool`.

- [ ] **Step 1: Write the failing test:**

```go
// TestHookSessionStartResumeReclaimsAlias is the durable-alias spec's core
// scenario end to end: a conversation's alias was registered in a now-dead
// tmux session (tombstoned), mail arrived, and the conversation is resumed
// in a brand-new tmux session. The SessionStart hook with source:"resume"
// must re-register the alias onto the NEW tuple and print a summary line
// (which Claude Code injects into the session's context) naming the alias
// and the backlog — and must NOT additionally register a fresh
// session-name alias.
func TestHookSessionStartResumeReclaimsAlias(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("MUSTER_ALIAS", "")
	seed := func(op string, args map[string]any) {
		t.Helper()
		if _, err := callData(op, args); err != nil {
			t.Fatal(err)
		}
	}
	seed("register_agent", map[string]any{
		"alias": "backend-2", "socket_path": "/tmp/sock", "session_id": "$OLD",
		"session_created": 111, "harness_session_id": "uuid-42", "label": "lake", "label_manual": true,
	})
	seed("register_agent", map[string]any{"alias": "sender", "socket_path": "/tmp/sock", "session_id": "$2"})
	seed("send_message", map[string]any{"from": "sender", "to_kind": "agent", "to_target": "backend-2", "subject": "s", "body": "b"})
	seed("deregister_agent", map[string]any{"alias": "backend-2"})

	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}":      "$NEW",
		"#{session_name}":    "muster-3",
		"#{session_created}": "222",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(`{"source":"resume","session_id":"uuid-42"}`), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "backend-2") || !strings.Contains(out, "1 unread") {
		t.Fatalf("resume summary missing alias/backlog:\n%s", out)
	}
	ag, ok := hookGetAgent("backend-2")
	if !ok || ag.Departed || ag.SessionID != "$NEW" || ag.Label != "lake" {
		t.Fatalf("reclaimed row = %+v (found=%v), want live on $NEW with label kept", ag, ok)
	}
	if _, exists := hookGetAgent("muster-3"); exists {
		t.Fatalf("resume must not also register a fresh session-name alias")
	}
}
```

(Check `tmuxenv.CaptureEnv`'s Run queries on the worktree and extend the `hookRun` map so socket/pane/project/label capture succeed — `tmuxenv_test.go` shows the exact format strings, including the `\x1f`-joined label/option query. Every format CaptureEnv asks for must return something coherent or empty.)

- [ ] **Step 2: Run it** — expect FAIL (no summary; a `muster-3` row registered instead).

- [ ] **Step 3: Implement.** In `cmdHook`'s SessionStart branch (as restructured by Task 6 — `hookCapture()` + `hookRegisterPane`), decode the source and try resume first:

```go
	case "SessionStart":
		c := hookCapture()
		h := harnessenv.FromHookPayload(payload)
		var start struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(payload, &start)
		if c.SocketPath != "" && c.PaneID != "" {
			if start.Source == "resume" && hookSessionStartResume(c, h, model, out) {
				return nil
			}
			if hookMayClaimIdentity(c) {
				hookRegisterPane(c, h, model)
			}
		} else {
			hookSessionStartPaneless(h, model)
		}
```

New function in `hook.go`:

```go
// hookSessionStartResume reclaims a resumed conversation's aliases onto the
// tmux session it woke up in (durable-alias spec change 4). The harness
// session UUID is the lookup key — resume keeps it (only fork mints a new
// one) — and harnessOwnedRows returns every row this conversation ever
// registered. Each row is re-registered onto the CURRENT tuple (the revive
// path: read-state survives, the daemon reports outcome+unread), EXCEPT a
// row still live in a different, provably-alive tmux session — that is a
// real collision (the old side's SessionEnd reason:"resume" normally
// tombstones first), reported rather than clobbered. Returns true when at
// least one alias was reclaimed: the caller then skips the default
// session-name register — the conversation's identity IS the reclaimed one,
// and minting a second alias would split it. Output goes to stdout, which
// the harness injects into the session's context: the agent wakes up
// knowing who it is and what's waiting.
func hookSessionStartResume(c tmuxenv.Capture, h harnessenv.Capture, model string, out io.Writer) bool {
	if h.SessionID == "" {
		return false
	}
	reclaimed := 0
	for _, ag := range harnessOwnedRows(h.SessionID) {
		sameTuple := ag.SocketPath == c.SocketPath && ag.SessionID == c.SessionID
		if !ag.Departed && !sameTuple && ag.SocketPath != "" &&
			tmuxenv.IsSessionAlive(ag.SocketPath, ag.SessionID, ag.SessionCreated) {
			fmt.Fprintf(out, "muster: alias '%s' is still live in another tmux session — not reclaimed\n", ag.Alias)
			continue
		}
		ack := reclaimRow(ag, c, h.SessionID, model)
		fmt.Fprintf(out, "muster: reconnected as '%s' (%s) — %d unread thread(s); call get_inbox with alias '%s'\n",
			ag.Alias, ack.Outcome, ack.Unread, ag.Alias)
		reclaimed++
	}
	return reclaimed > 0
}
```

New function in `paneless.go`, next to `reviveRow` (same shape, but the tuple is the CURRENT capture — a reclaim moves the row; label/role travel with the conversation, per the spec: identity belongs to the conversation, and a manually-pinned label is part of it):

```go
// reclaimRow re-registers a conversation's row onto the tmux session it
// resumed in: role, model, label, and harness link are echoed from the row
// (they belong to the conversation), while the tuple is the CURRENT
// capture's (the conversation moved). Contrast reviveRow, which echoes the
// stored tuple back for an in-place revival.
func reclaimRow(ag agentRow, c tmuxenv.Capture, harnessID, model string) registerAck {
	if model == "" {
		model = ag.ModelType
	}
	raw, _ := callData("register_agent", map[string]any{
		"alias": ag.Alias, "role": ag.Role, "model_type": model,
		"session_name": c.SessionName, "session_id": c.SessionID,
		"session_created":    c.SessionCreated,
		"harness_session_id": harnessID,
		"socket_path":        c.SocketPath, "pane_id": c.PaneID,
		"project": c.Project, "label": ag.Label, "label_manual": ag.LabelManual,
	})
	return decodeRegisterAck(raw)
}
```

- [ ] **Step 4: Also cover the collision-skip branch** — append a second test seeding the row NON-departed on a different tuple with the `IsSessionAlive` probe answering alive (stub `tmuxenv.Run` so the `#{session_created}` probe for the OLD socket returns the row's stored `session_created` — read `tmuxenv.IsSessionAlive` on the worktree for the exact args it passes; `hook_test.go`'s existing liveness-stubbing tests show the pattern):

```go
// TestHookSessionStartResumeSkipsLiveCollision: a row still provably live in
// another tmux session is reported, never clobbered — and with nothing
// reclaimed the hook falls through to the normal session-name register.
func TestHookSessionStartResumeSkipsLiveCollision(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("MUSTER_ALIAS", "")
	if _, err := callData("register_agent", map[string]any{
		"alias": "backend-2", "socket_path": "/other-sock", "session_id": "$OLD",
		"session_created": 111, "harness_session_id": "uuid-42",
	}); err != nil {
		t.Fatal(err)
	}
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		// The liveness probe against /other-sock/$OLD must confirm alive.
		for _, a := range args {
			if a == "/other-sock" {
				return "111", nil
			}
		}
		return hookRun(map[string]string{
			"#{session_id}": "$NEW", "#{session_name}": "muster-3", "#{session_created}": "222",
		})(args...)
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(`{"source":"resume","session_id":"uuid-42"}`), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "not reclaimed") {
		t.Fatalf("expected a collision notice, got:\n%s", buf.String())
	}
	ag, _ := hookGetAgent("backend-2")
	if ag.SessionID != "$OLD" {
		t.Fatalf("collision row moved to %q — must stay on $OLD", ag.SessionID)
	}
}
```

- [ ] **Step 5: Run the package** — `go test ./internal/humancli/ -race` — expect PASS, including all pre-existing SessionStart/Stop/paneless tests.

- [ ] **Step 6: Commit**

```bash
git add internal/humancli/hook.go internal/humancli/paneless.go internal/humancli/hook_test.go
git commit -m "feat(hook): SessionStart source:resume reclaims the conversation's aliases with a context summary"
```

---

### Task 8: Docs — skill and README teach alias-as-durable-identity

**Files:**
- Modify: `.claude/skills/muster-coordination/SKILL.md`, `README.md`

**Interfaces:** none (prose only). Full sentences; explain, don't sell; no assumed context.

- [ ] **Step 1: SKILL.md — three edits.**

Replace the "Register once, at the start" section (lines 12–17) with:

```markdown
## Register at the start — and on resume

Call `register_agent(alias, role, model_type)` once when your session begins.
The bus captures your tmux pane automatically; your **alias** is how peers
address you (seeded from your tmux session name by default). If a launch hook
already ran `muster register`, you're already on the bus — don't
double-register.

Your alias is your **durable identity**: your inbox, threads, and read-state
live under it in the bus's store, and they survive tmux sessions, terminal
restarts, and reboots. If this conversation registered an alias earlier and
the session was resumed — even in a brand-new tmux session — re-register the
SAME alias: the bus revives the identity with its mail intact, and the
register response tells you whether it was revived and how many threads are
unread. (Under Claude Code the SessionStart hook does this reclaim for you
and tells you your alias and backlog; re-registering by name is the fallback
every harness has.)
```

In the Addressing section, replace the alias bullet (line 40):

```markdown
- **alias** — a peer's durable bus identity, globally unique: `send to
  "backend-2"`. It is usually seeded from the tmux session name at first
  registration, but it belongs to the conversation, not the terminal — it
  keeps its inbox across tmux sessions.
```

- [ ] **Step 2: README — vocabulary sweep.** Run `grep -n "session name" README.md` in the worktree. For each hit that *defines* the alias as the tmux session name (the register/identity sections), rephrase to the seed-then-own model, e.g. "your alias defaults to your tmux session name at first registration" → "your alias is seeded from your tmux session name at first registration; thereafter it is the session's durable identity — re-registering the same alias from anywhere (including after a resume in a new tmux session) revives it with its inbox intact." Do not touch hits that merely *display* the session name (nudge output, station columns). Also add one sentence to the README's hooks section: "On `SessionStart` with `source:"resume"`, the hook reclaims every alias the resumed conversation owns onto the new tmux session and reports the unread backlog into the session's context."

- [ ] **Step 3: Verify prose renders** — `just verify` still passes (docs don't affect it, but the commit gate is uniform).

- [ ] **Step 4: Commit**

```bash
git add .claude/skills/muster-coordination/SKILL.md README.md
git commit -m "docs: alias is the durable identity; register on resume"
```

---

### Task 9: Gate, PR, and live verification checklist

- [ ] **Step 1:** `just verify` in the worktree — all green (trust CI over local if a local-only dependency error appears; read the CI log on failure).
- [ ] **Step 2:** Push and open a PR to `dev` titled `feat: durable alias identity — register-on-resume reclaim`. Body: link the spec file, summarize the four changes, and note the residuals (Stop-stamp piggybacks on the mail gate; Codex rides the contract; Cursor payload fields to be verified empirically).
- [ ] **Step 3 (operator-gated, post-merge):** live verification with the real binary — register an alias in a tmux session, send it mail from another session, kill the tmux session, `claude --resume` the conversation in a NEW tmux session, and confirm: the old row was tombstoned (SessionEnd), the SessionStart hook printed the "reconnected as … unread" summary into context, and `muster agents` shows the alias live on the new tuple. Also capture what `cursor-agent` hook payloads actually contain (the spec's open Cursor question) and post the findings — plus the final `whereami` verb name/flags — as an fyi reply on bus thread 149, where the dotfiles session is waiting to fold them into its hooks.json writers.
- [ ] **Step 4:** VERSION bump rides a separate `chore/bump-*` PR on dev when the operator decides to release (per house release model).

---

## Self-review notes (spec ↔ plan)

- Spec change 1 (outcome + unread + journal event) → Task 1; CLI/MCP surfaces → Tasks 3–4.
- Spec change 2 (skill rewrite) → Task 8.
- Spec change 3 (CLI default kept, vocabulary fixed) → Task 8 (no code change, by design).
- Spec change 4 (stamp / release / reclaim) → ancestry capture + whereami: Task 5; stamp: Tasks 2, 4, 6; release: already shipped (v0.7.4 hookSessionEnd — no task); reclaim: Task 7.
- Spec's ancestry-walk contract (4 invariants, shared with dotfiles by contract not by binary — thread 149) → Task 5; the fail-safe invariant is the tmuxenv test's second assertion.
- Spec's "revival journal event" → Task 1 Step 3.
- Spec's collision-skip rule → Task 7 Steps 3–4.
- Hooks are env-stripped (verified, thread 149): both hook tasks route capture through `hookCapture()` (env, else ancestry walk), and Task 6's `hookRegisterPane` replaces the cmdRegister delegation, which would silently re-read the empty env. Hook tests that stub `$TMUX`/`tmuxenv.Run` exercise the env path; the walk path is covered in Task 5's tmuxenv tests.
- Cursor payload verification + posting findings to bus thread 149 → Task 9 Step 3.
- Paneless resume already revives via `hookSessionStartPaneless` (shipped in #74) — out of scope here except that `reviveRow` now returns an ack (Task 3), which paneless callers may ignore.
