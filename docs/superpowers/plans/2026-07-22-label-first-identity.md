# Label-First Identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the tmux task label the one human-facing session name — set once via `prefix T` → `muster label`, propagated to tmux, the bus, AND the running Claude Code session — while closing the two alias-minting holes (models re-registering under invented aliases; unregistered from-addresses) that litter the roster.

**Architecture:** `muster label` already writes the tmux option + manual flag and pushes the stored label to the daemon (`set_label`). This plan adds the third fan-out — typing `/rename <name>` into the session's registered live Claude pane, strictly gated on the roster naming a live claude-model agent with a live pane on this session tuple — and hardens the MCP surface: `register_agent` becomes idempotent for a pane that already has a live registration, and `send_message`/`reply`/`task_create` reject a `from` alias that isn't on the roster. The Stop hook and register responses speak label-first so models learn the label vocabulary. Finally, dotfiles rewires `prefix T` to shell out to `muster label`.

**Tech Stack:** Go (muster repo: `~/GitHub/schuettc/muster`, module `github.com/schuettc/muster`), tmux config (dotfiles repo: `~/dotfiles`).

## Global Constraints

- **Never branch-switch the primary clone** `~/GitHub/schuettc/muster`. All muster work happens in a git worktree on branch `feat/label-first-identity`, created off `dev` (branch model: feat → dev → main). Move THIS plan file onto that branch as its first commit.
- Verify commands (same as CI): `just fmt-check && just lint && TMPDIR=/tmp just test` from the worktree root. `TMPDIR=/tmp` is required on macOS — unix-socket paths from `t.TempDir()` exceed the ~104-char `sun_path` limit.
- Scripts are Go; tests use the repo's existing seams: `tmuxenv.Run` (package var), `callDaemon` (mcpserver package var), `startCLITestDaemon`/`registerViaDaemon`/`listAgentsForTest` (humancli test helpers), `startTestDaemon` (in-process daemon).
- The daemon stays tmux-agnostic (package boundary rule): all tmux queries and pane typing happen client-side (humancli/nudge/tmuxenv), never in internal/daemon.
- Package nudge remains "the ONLY place muster types into a pane" — the Claude rename injection goes through it.
- Human CLI senders stay unvalidated (operator escape hatch: `--from human`/`operator` are routine). From-validation applies to the MCP surface only.
- Dotfiles changes are committed directly on `main` in `~/dotfiles` (repo convention — no feature branches there).

## File Structure

**muster (worktree):**
- `internal/nudge/nudge.go` — add `TypeLine` (generalized typing); `Nudge` delegates to it.
- `internal/nudge/nudge_test.go` — TypeLine tests.
- `internal/humancli/label.go` — add `syncClaudeName` (the /rename injection + gate); call from `cmdLabel`.
- `internal/humancli/label_test.go` — injection gate tests.
- `internal/humancli/hook.go` — label-first Stop-hook wording (`hookReason` gains a label).
- `internal/humancli/hook_test.go` — wording tests.
- `internal/humancli/registry.go` — update `label` command help text.
- `internal/mcpserver/tools_registry.go` — idempotent `register_agent` + roster-row decode + tool description.
- `internal/mcpserver/tools_registry_test.go` — idempotency tests.
- `internal/mcpserver/tools_messages.go`, `internal/mcpserver/tools_tasks.go` — from-validation on send_message / reply / task_create.
- `internal/mcpserver/validate.go` (new) — shared `requireRegisteredFrom` helper.
- `internal/mcpserver/validate_test.go` (new) — validation tests.

**dotfiles (main):**
- `.tmux.conf` — `bind T` shells out to `muster label`; comment update.

---

### Task 1: `nudge.TypeLine` — generalized pane typing

**Files:**
- Modify: `internal/nudge/nudge.go`
- Test: `internal/nudge/nudge_test.go`

**Interfaces:**
- Produces: `func (n TmuxNudger) TypeLine(socketPath, paneID, modelType, text string, submit bool) (submitted bool, err error)` — identical semantics to today's `Nudge` but with caller-supplied text. `Nudge(socketPath, paneID, modelType, submit)` becomes a one-line delegation passing the existing `message` const. Task 2 consumes `TypeLine`.

- [ ] **Step 1: Write the failing test**

Append to `internal/nudge/nudge_test.go` (match the file's existing stub style — `Run` captures args, `Sleep` records durations):

```go
func TestTypeLineTypesCallerTextAndSubmitsForClaude(t *testing.T) {
	var calls [][]string
	n := TmuxNudger{Run: func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}}
	submitted, err := n.TypeLine("/tmp/sock", "%5", "claude", "/rename standard 2000", true)
	if err != nil || !submitted {
		t.Fatalf("TypeLine: submitted=%v err=%v", submitted, err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected type + Enter, got %v", calls)
	}
	want := []string{"-S", "/tmp/sock", "send-keys", "-t", "%5", "-l", "/rename standard 2000"}
	if !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("type call = %v, want %v", calls[0], want)
	}
	if calls[1][len(calls[1])-1] != "Enter" {
		t.Fatalf("expected trailing Enter submit, got %v", calls[1])
	}
}

func TestNudgeStillTypesTheCanonicalMessage(t *testing.T) {
	var calls [][]string
	n := TmuxNudger{Run: func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}}
	if _, err := n.Nudge("/tmp/sock", "%5", "claude", false); err != nil {
		t.Fatal(err)
	}
	if calls[0][len(calls[0])-1] != message {
		t.Fatalf("Nudge must type the canonical message, got %v", calls[0])
	}
}
```

Add `"reflect"` to the test file's imports if absent.

- [ ] **Step 2: Run tests to verify the new one fails**

Run: `TMPDIR=/tmp go test -race ./internal/nudge/ -run 'TypeLine|CanonicalMessage' -v`
Expected: FAIL — `n.TypeLine undefined`.

- [ ] **Step 3: Implement TypeLine, delegate Nudge**

In `internal/nudge/nudge.go`, replace the body of `Nudge` and add `TypeLine` (the doc comments move with the semantics):

```go
// TypeLine types text into the pane and optionally submits it — the general
// form of Nudge, for callers with their own line to deliver (e.g. `muster
// label`'s /rename sync). Submit semantics are per-model, identical to Nudge:
// claude accepts an immediate Enter; codex needs codexSubmitDelay first;
// unknown model types are typed-only (submitted=false) so the caller can tell
// the operator to press Enter.
func (n TmuxNudger) TypeLine(socketPath, paneID, modelType, text string, submit bool) (bool, error) {
	if socketPath == "" || paneID == "" {
		return false, fmt.Errorf("agent has no tmux pane (not registered from inside tmux)")
	}
	if err := n.run("-S", socketPath, "send-keys", "-t", paneID, "-l", text); err != nil {
		return false, fmt.Errorf("send-keys failed (pane may be gone): %w", err)
	}
	if !submit {
		return false, nil
	}
	switch modelType {
	case "claude":
		// Immediate Enter submits.
	case "codex":
		n.sleep(codexSubmitDelay) // let codex finish processing the paste before Enter
	default:
		return false, nil // unknown submit behavior → typed-only
	}
	if err := n.run("-S", socketPath, "send-keys", "-t", paneID, "Enter"); err != nil {
		return false, fmt.Errorf("submit failed: %w", err)
	}
	return true, nil
}

// Nudge types the check-inbox line into the pane and (optionally) submits it.
func (n TmuxNudger) Nudge(socketPath, paneID, modelType string, submit bool) (bool, error) {
	return n.TypeLine(socketPath, paneID, modelType, message, submit)
}
```

- [ ] **Step 4: Run the package tests**

Run: `TMPDIR=/tmp go test -race ./internal/nudge/ -v`
Expected: ALL PASS (existing Nudge tests must still pass — the delegation preserves behavior).

- [ ] **Step 5: Commit**

```bash
git add internal/nudge/nudge.go internal/nudge/nudge_test.go
git commit -m "feat(nudge): TypeLine generalizes pane typing for caller-supplied text"
```

---

### Task 2: `muster label` renames the live Claude session

**Files:**
- Modify: `internal/humancli/label.go`
- Modify: `internal/humancli/registry.go` (label command help text)
- Test: `internal/humancli/label_test.go`

**Interfaces:**
- Consumes: `nudge.TmuxNudger.TypeLine` (Task 1); existing `callData`, `agentRow` (has `Alias/ModelType/SocketPath/PaneID/SessionID/Departed` fields), `tmuxenv.SocketFromEnv/CurrentSessionID/IsPaneAlive`.
- Produces: `syncClaudeName(out io.Writer, name string)` — package-private; called only from `cmdLabel`'s set path (NOT the clear path).

The gate (the user's explicit requirement — inject ONLY when Claude Code is actually there): a roster row on this session tuple with `model_type == "claude"`, not departed, non-empty `pane_id`, and `tmuxenv.IsPaneAlive` true. No row → silently skip. This is deliberately the roster, not `pane_current_command` sniffing: registration by the SessionStart hook IS the definition of "Claude Code is running in this pane."

- [ ] **Step 1: Write the failing tests**

Append to `internal/humancli/label_test.go`:

```go
// TestLabelRenamesLiveClaudePane covers the third fan-out of `muster label`:
// a live claude-model registration on the ambient session tuple gets
// "/rename <name>" typed into its pane (and submitted), so the Claude Code
// session name follows the label. The gate is the roster + a live pane —
// see TestLabelSkipsRenameWithoutLiveClaude for every negative.
func TestLabelRenamesLiveClaudePane(t *testing.T) {
	startCLITestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	registerClaudeViaDaemon(t, "worker", "/tmp/sock", "$1", "%5")

	var sent [][]string
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		last := args[len(args)-1]
		if last == "#{session_id}" {
			return "$1", nil
		}
		if last == "#{pane_id}" {
			return "%5", nil // pane-alive probe answers: alive
		}
		if len(args) > 2 && args[2] == "send-keys" {
			sent = append(sent, append([]string(nil), args...))
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdLabel([]string{"standard 2000"}, &buf); err != nil {
		t.Fatalf("label: %v", err)
	}
	if len(sent) != 2 {
		t.Fatalf("expected /rename type + Enter submit, got %v", sent)
	}
	if got := sent[0][len(sent[0])-1]; got != "/rename standard 2000" {
		t.Fatalf("typed %q, want %q", got, "/rename standard 2000")
	}
	if sent[1][len(sent[1])-1] != "Enter" {
		t.Fatalf("expected Enter submit, got %v", sent[1])
	}
	if !strings.Contains(buf.String(), "renamed claude session") {
		t.Fatalf("expected rename confirmation in output, got %q", buf.String())
	}
}

// TestLabelSkipsRenameWithoutLiveClaude: no injection for (a) a codex row,
// (b) a departed claude row, (c) a claude row whose pane is dead, (d) a
// claude row on a DIFFERENT session tuple. The label/bus writes still happen.
func TestLabelSkipsRenameWithoutLiveClaude(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		sessionID string
		depart    bool
		paneAlive bool
	}{
		{"codex row", "codex", "$1", false, true},
		{"departed claude", "claude", "$1", true, true},
		{"dead pane", "claude", "$1", false, false},
		{"other session", "claude", "$9", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			startCLITestDaemon(t)
			t.Setenv("TMUX", "/tmp/sock,1,0")
			registerModelViaDaemon(t, "worker", "/tmp/sock", tc.sessionID, "%5", tc.model)
			if tc.depart {
				if _, err := callData("deregister_agent", map[string]any{"alias": "worker"}); err != nil {
					t.Fatal(err)
				}
			}
			var sent [][]string
			prev := tmuxenv.Run
			tmuxenv.Run = func(args ...string) (string, error) {
				last := args[len(args)-1]
				if last == "#{session_id}" {
					return "$1", nil
				}
				if last == "#{pane_id}" {
					if tc.paneAlive {
						return "%5", nil
					}
					return "", nil // dead pane
				}
				if len(args) > 2 && args[2] == "send-keys" {
					sent = append(sent, append([]string(nil), args...))
				}
				return "", nil
			}
			t.Cleanup(func() { tmuxenv.Run = prev })

			var buf bytes.Buffer
			if err := cmdLabel([]string{"datalake"}, &buf); err != nil {
				t.Fatalf("label: %v", err)
			}
			if len(sent) != 0 {
				t.Fatalf("expected NO injection for %s, got %v", tc.name, sent)
			}
		})
	}
}

// TestLabelClearNeverInjects: clearing a label must not type anything into
// any pane — there is no "/rename to nothing" gesture worth sending.
func TestLabelClearNeverInjects(t *testing.T) {
	startCLITestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	registerClaudeViaDaemon(t, "worker", "/tmp/sock", "$1", "%5")
	var sent [][]string
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		last := args[len(args)-1]
		if last == "#{session_id}" {
			return "$1", nil
		}
		if last == "#{pane_id}" {
			return "%5", nil
		}
		if len(args) > 2 && args[2] == "send-keys" {
			sent = append(sent, append([]string(nil), args...))
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })
	var buf bytes.Buffer
	if err := cmdLabel([]string{"--clear"}, &buf); err != nil {
		t.Fatalf("label --clear: %v", err)
	}
	if len(sent) != 0 {
		t.Fatalf("clear must not inject, got %v", sent)
	}
}
```

Add the registration helpers next to `registerViaDaemon` in `internal/humancli/identity_test.go`:

```go
// registerClaudeViaDaemon registers a live claude-model agent with a pane —
// the row shape `muster label`'s /rename gate looks for.
func registerClaudeViaDaemon(t *testing.T, alias, socketPath, sessionID, paneID string) {
	t.Helper()
	registerModelViaDaemon(t, alias, socketPath, sessionID, paneID, "claude")
}

func registerModelViaDaemon(t *testing.T, alias, socketPath, sessionID, paneID, model string) {
	t.Helper()
	if _, err := callData("register_agent", map[string]any{
		"alias": alias, "socket_path": socketPath, "session_id": sessionID,
		"pane_id": paneID, "model_type": model,
	}); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `TMPDIR=/tmp go test -race ./internal/humancli/ -run 'TestLabel' -v`
Expected: the three new tests FAIL (no injection code exists; `registerClaudeViaDaemon` undefined until the helper is added — add helpers first, then the failures are assertion failures). Existing `TestLabel*` tests must still PASS. NOTE: the existing tests `TestLabelSetsOptionsAndRefreshes` / `TestLabelClearUnsetsBothOptions` assert exact tmux call COUNTS (4). The new roster lookup adds no tmux calls in those tests (their `tmuxenv.Run` stub answers the session-id probe with `""`, so `syncClaudeName` returns before any roster/pane work) — if a count changes, the guard `socket == "" || sessionID == ""` is wrong.

- [ ] **Step 3: Implement syncClaudeName**

In `internal/humancli/label.go`, add `"encoding/json"` and `"github.com/schuettc/muster/internal/nudge"` to imports, insert after `syncLabelToBus(out, name, true)` in `cmdLabel`'s set path (NOT the clear path):

```go
	syncClaudeName(out, name)
```

and add:

```go
// syncClaudeName types "/rename <name>" into this session's registered live
// Claude pane so the Claude Code session name follows the label — making
// prefix T (which shells out to `muster label`) the ONE naming gesture for
// tmux, the bus, and Claude. Strictly gated on the roster: a non-departed
// claude-model row on this exact session tuple whose pane is still alive. A
// session with no live Claude (plain shell, codex, dead pane) gets no
// injection — the roster is the definition of "Claude Code runs here", not
// pane_current_command sniffing. Best-effort like syncLabelToBus: a skipped
// or failed injection never fails the label write. Clearing never injects
// (there is no "/rename to nothing" gesture worth typing at a session).
func syncClaudeName(out io.Writer, name string) {
	socket := tmuxenv.SocketFromEnv()
	sessionID := tmuxenv.CurrentSessionID()
	if socket == "" || sessionID == "" {
		return
	}
	raw, err := callData("list_agents", nil)
	if err != nil {
		return // no daemon → no roster to gate on; the tmux label already landed
	}
	var rows []agentRow
	if json.Unmarshal(raw, &rows) != nil {
		return
	}
	// Route typing through the tmuxenv.Run seam (NOT a zero-value TmuxNudger,
	// whose nil Run spawns real tmux): humancli's tests stub tmuxenv.Run, and
	// one process-spawning seam per package keeps them able to observe this.
	typer := nudge.TmuxNudger{Run: func(args ...string) error {
		_, err := tmuxenv.Run(args...)
		return err
	}}
	for _, ag := range rows {
		if ag.Departed || ag.ModelType != "claude" || ag.SocketPath != socket ||
			ag.SessionID != sessionID || ag.PaneID == "" {
			continue
		}
		if !tmuxenv.IsPaneAlive(socket, ag.PaneID) {
			continue
		}
		if _, err := typer.TypeLine(socket, ag.PaneID, "claude", "/rename "+name, true); err != nil {
			_, _ = fmt.Fprintf(out, "warning: claude session rename failed (%v); run /rename %s in claude yourself\n", err, name)
			return
		}
		_, _ = fmt.Fprintf(out, "renamed claude session to match (pane %s)\n", ag.PaneID)
		return // one live claude per session; first match wins
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `TMPDIR=/tmp go test -race ./internal/humancli/ -run 'TestLabel' -v`
Expected: ALL PASS.

- [ ] **Step 5: Update the label command's help text**

In `internal/humancli/registry.go`, find the `Name: "label"` entry (~line 236) and extend its help/description string to state the third fan-out. Keep the existing sentence(s) and append: `"When a live Claude Code agent is registered in this session, also types /rename <name> into its pane so the Claude session name follows."`

- [ ] **Step 6: Run the full package + commit**

Run: `TMPDIR=/tmp go test -race ./internal/humancli/ && just fmt-check && just lint`
Expected: PASS.

```bash
git add internal/humancli/label.go internal/humancli/label_test.go internal/humancli/identity_test.go internal/humancli/registry.go
git commit -m "feat(label): muster label renames the live Claude session via /rename injection"
```

---

### Task 3: idempotent `register_agent` (MCP surface)

**Files:**
- Modify: `internal/mcpserver/tools_registry.go`
- Modify: `internal/mcpserver/call.go` (extend the roster decode)
- Test: `internal/mcpserver/tools_registry_test.go`

**Interfaces:**
- Consumes: `callDaemon` (package var, stubbable), `tmuxenv.CaptureEnv`.
- Produces: `rosterRow` struct (package-private, in call.go) — the full-fidelity decode of `list_agents` rows: `Alias, ModelType, SocketPath, PaneID, SessionID, Label string; Departed bool` with snake_case json tags. `paneRegistration(socketPath, sessionID, paneID string) (rosterRow, bool)` — the pane's own live registration, if any. Task 4 consumes both.

Behavior: when the calling pane's (socket_path, session_id, pane_id) already has a live (non-departed) registration under a DIFFERENT alias, `register_agent` does NOT register — it returns success with `Detail: "already registered as '<alias>' (label '<label>') — use that alias; not adding a second"`. Same-alias calls still upsert (harmless refresh). Outside tmux (empty socket/pane) behavior is unchanged. Success-not-error is deliberate: the caller is a model; the goal is to END its registration attempt with its true identity in hand, not to invite retries.

- [ ] **Step 1: Write the failing tests**

Create/extend `internal/mcpserver/tools_registry_test.go` (follow the package's existing callDaemon-stub test style):

```go
func TestRegisterAgentIdempotentForRegisteredPane(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) { return "$1", nil } // session-id probe
	t.Cleanup(func() { tmuxenv.Run = prevRun })

	var registered bool
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		switch op {
		case "list_agents":
			return json.RawMessage(`[{"alias":"timewalk-2","model_type":"claude","socket_path":"/tmp/sock","pane_id":"%14","session_id":"$1","label":"standard 2000","departed":false}]`), nil
		case "register_agent":
			registered = true
			return json.RawMessage(`null`), nil
		}
		t.Fatalf("unexpected op %s", op)
		return nil, nil
	}

	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "timewalk-2002", ModelType: "claude"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if registered {
		t.Fatal("must NOT mint a second alias for an already-registered pane")
	}
	if !out.OK || !strings.Contains(out.Detail, "already registered as 'timewalk-2'") || !strings.Contains(out.Detail, "standard 2000") {
		t.Fatalf("expected identity-bearing detail, got %+v", out)
	}
}

func TestRegisterAgentSameAliasStillUpserts(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) { return "$1", nil }
	t.Cleanup(func() { tmuxenv.Run = prevRun })

	var registered bool
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		switch op {
		case "list_agents":
			return json.RawMessage(`[{"alias":"timewalk-2","model_type":"claude","socket_path":"/tmp/sock","pane_id":"%14","session_id":"$1","departed":false}]`), nil
		case "register_agent":
			registered = true
			return json.RawMessage(`null`), nil
		}
		return nil, nil
	}
	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "timewalk-2", ModelType: "claude"})
	if err != nil || !out.OK {
		t.Fatalf("same-alias re-register must succeed: %+v %v", out, err)
	}
	if !registered {
		t.Fatal("same-alias call must still upsert (refresh)")
	}
}

func TestRegisterAgentFreshPaneRegisters(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) { return "$1", nil }
	t.Cleanup(func() { tmuxenv.Run = prevRun })

	var registered bool
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		switch op {
		case "list_agents":
			return json.RawMessage(`[]`), nil
		case "register_agent":
			registered = true
			return json.RawMessage(`null`), nil
		}
		return nil, nil
	}
	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "fresh", ModelType: "claude"})
	if err != nil || !out.OK || !registered {
		t.Fatalf("fresh pane must register: registered=%v out=%+v err=%v", registered, out, err)
	}
}
```

Imports needed: `context`, `encoding/json`, `strings`, `testing`, plus `github.com/schuettc/muster/internal/tmuxenv`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `TMPDIR=/tmp go test -race ./internal/mcpserver/ -run 'TestRegisterAgent' -v`
Expected: FAIL — first test sees `registered == true` (today's handler always registers).

- [ ] **Step 3: Implement rosterRow + paneRegistration + the guard**

In `internal/mcpserver/call.go`, add:

```go
// rosterRow is the full-fidelity decode of a list_agents row — the fields
// AgentView (the tool-facing shape) deliberately omits but identity guards
// need. Tags match the daemon's snake_case store JSON.
type rosterRow struct {
	Alias      string `json:"alias"`
	ModelType  string `json:"model_type"`
	SocketPath string `json:"socket_path"`
	PaneID     string `json:"pane_id"`
	SessionID  string `json:"session_id"`
	Label      string `json:"label"`
	Departed   bool   `json:"departed"`
}

// paneRegistration returns the calling pane's own live registration: the
// non-departed roster row matching this exact (socket_path, session_id,
// pane_id). ok=false outside tmux, on any daemon/decode failure (guards
// degrade open — today's behavior), or when no row matches.
func paneRegistration(socketPath, sessionID, paneID string) (rosterRow, bool) {
	if socketPath == "" || sessionID == "" || paneID == "" {
		return rosterRow{}, false
	}
	raw, err := callDaemon("list_agents", nil)
	if err != nil {
		return rosterRow{}, false
	}
	var rows []rosterRow
	if json.Unmarshal(raw, &rows) != nil {
		return rosterRow{}, false
	}
	for _, r := range rows {
		if !r.Departed && r.SocketPath == socketPath && r.SessionID == sessionID && r.PaneID == paneID {
			return r, true
		}
	}
	return rosterRow{}, false
}
```

In `internal/mcpserver/tools_registry.go`, at the top of `registerAgentHandler` (before the daemon call), insert:

```go
	c := tmuxenv.CaptureEnv()
	if row, ok := paneRegistration(c.SocketPath, c.SessionID, c.PaneID); ok && row.Alias != in.Alias {
		detail := fmt.Sprintf("already registered as '%s'", row.Alias)
		if row.Label != "" {
			detail = fmt.Sprintf("already registered as '%s' (label '%s')", row.Alias, row.Label)
		}
		return nil, OKOut{OK: true, Detail: detail + " — use that alias; not adding a second"}, nil
	}
```

(The existing `c := tmuxenv.CaptureEnv()` line later in the handler becomes redundant — reuse this one.) Add `"fmt"` to imports.

- [ ] **Step 4: Run the tests**

Run: `TMPDIR=/tmp go test -race ./internal/mcpserver/ -v`
Expected: ALL PASS.

- [ ] **Step 5: Update the tool description**

In `registerRegistryTools`, replace `register_agent`'s Description with:

```go
Description: "Claim an agent identity on the muster bus. NOTE: sessions inside tmux are auto-registered at session start under their tmux session name — you almost never need this tool; the Stop hook and your inbox already address you. Calling it from an already-registered pane returns your existing identity instead of adding a second alias.",
```

- [ ] **Step 6: Verify + commit**

Run: `just fmt-check && just lint && TMPDIR=/tmp just test`
Expected: PASS.

```bash
git add internal/mcpserver/call.go internal/mcpserver/tools_registry.go internal/mcpserver/tools_registry_test.go
git commit -m "feat(mcp): register_agent is idempotent for an already-registered pane"
```

---

### Task 4: reject unregistered `from` on the MCP send surface

**Files:**
- Create: `internal/mcpserver/validate.go`
- Create: `internal/mcpserver/validate_test.go`
- Modify: `internal/mcpserver/tools_messages.go` (sendMessageHandler, replyHandler)
- Modify: `internal/mcpserver/tools_tasks.go` (taskCreateHandler)

**Interfaces:**
- Consumes: `callDaemon`, `rosterRow`, `paneRegistration` (Task 3), `tmuxenv.CaptureEnv`.
- Produces: `requireRegisteredFrom(from string) error` — nil when `from` is an exact roster alias (departed rows INCLUDED: draining a tombstoned alias's mail must still be able to reply as it). On failure, the error names the caller's real identity when resolvable: `from alias "timewalk-1998" is not registered; this session is registered as 'timewalk-2' (label 'standard 2000') — send as that alias`.

Scope: `send_message`, `reply`, `task_create` — the thread-minting ops (this is the hole `timewalk-1998` walked through). `task_claim`/`task_transition` operate on existing threads and stay as-is (YAGNI). The human CLI is untouched — `--from human`/`operator` remain the operator's escape hatch; this validation lives only in the MCP handlers models call.

- [ ] **Step 1: Write the failing tests**

Create `internal/mcpserver/validate_test.go`:

```go
package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

func stubRoster(t *testing.T, rosterJSON string) *[]string {
	t.Helper()
	prev := callDaemon
	t.Cleanup(func() { callDaemon = prev })
	ops := &[]string{}
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		*ops = append(*ops, op)
		if op == "list_agents" {
			return json.RawMessage(rosterJSON), nil
		}
		return json.RawMessage(`{"thread_id":1,"entry_id":1}`), nil
	}
	return ops
}

func TestSendMessageRejectsUnregisteredFrom(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) { return "$1", nil }
	t.Cleanup(func() { tmuxenv.Run = prevRun })
	ops := stubRoster(t, `[{"alias":"timewalk-2","socket_path":"/tmp/sock","pane_id":"%14","session_id":"$1","label":"standard 2000","departed":false}]`)

	_, _, err := sendMessageHandler(context.Background(), nil, SendMessageIn{
		From: "timewalk-1998", ToKind: "agent", ToTarget: "timewalk-2", Body: "hi",
	})
	if err == nil {
		t.Fatal("unregistered from must be rejected")
	}
	if !strings.Contains(err.Error(), "timewalk-1998") || !strings.Contains(err.Error(), "'timewalk-2'") {
		t.Fatalf("error must name the bad alias AND the real identity, got %v", err)
	}
	for _, op := range *ops {
		if op == "send_message" {
			t.Fatal("send_message must not reach the daemon for an unregistered from")
		}
	}
}

func TestSendMessageAllowsRegisteredAndDepartedFrom(t *testing.T) {
	roster := `[{"alias":"timewalk-2","departed":false},{"alias":"lake-broker","departed":true}]`
	for _, from := range []string{"timewalk-2", "lake-broker"} {
		stubRoster(t, roster)
		_, _, err := sendMessageHandler(context.Background(), nil, SendMessageIn{
			From: from, ToKind: "agent", ToTarget: "timewalk-2", Body: "hi",
		})
		if err != nil {
			t.Fatalf("from=%q must be allowed (departed rows drain mail): %v", from, err)
		}
	}
}

func TestReplyAndTaskCreateRejectUnregisteredFrom(t *testing.T) {
	stubRoster(t, `[]`)
	if _, _, err := replyHandler(context.Background(), nil, ReplyIn{ThreadID: 1, From: "ghost", Body: "x"}); err == nil {
		t.Fatal("reply must reject unregistered from")
	}
	stubRoster(t, `[]`)
	if _, _, err := taskCreateHandler(context.Background(), nil, TaskCreateIn{From: "ghost", ToKind: "agent", ToTarget: "x", Body: "x"}); err == nil {
		t.Fatal("task_create must reject unregistered from")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `TMPDIR=/tmp go test -race ./internal/mcpserver/ -run 'From' -v`
Expected: FAIL — no validation exists, handlers reach the daemon.

- [ ] **Step 3: Implement requireRegisteredFrom**

Create `internal/mcpserver/validate.go`:

```go
package mcpserver

import (
	"encoding/json"
	"fmt"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// requireRegisteredFrom gates the MCP thread-minting ops (send_message,
// reply, task_create): from must be an EXACT roster alias. Departed rows
// count — draining a tombstoned alias's leftover mail must still be able to
// reply as it. This closes the address-minting hole where a model invents a
// from alias (e.g. a persona like "timewalk-1998") and the daemon accepts it
// verbatim, materializing an identity nobody registered. The human CLI is
// deliberately NOT behind this gate — `muster send --from operator` is the
// operator's escape hatch and doesn't route through these handlers.
//
// On rejection the error carries the caller's REAL identity when the pane's
// registration resolves, so a confused model self-corrects in one step
// instead of retrying blind. A roster fetch/decode failure degrades open
// (returns nil): a dead daemon already fails the op itself with a clearer
// transport error.
func requireRegisteredFrom(from string) error {
	raw, err := callDaemon("list_agents", nil)
	if err != nil {
		return nil // degrade open; the real op will surface the transport error
	}
	var rows []rosterRow
	if json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	for _, r := range rows {
		if r.Alias == from {
			return nil
		}
	}
	c := tmuxenv.CaptureEnv()
	if row, ok := paneRegistration(c.SocketPath, c.SessionID, c.PaneID); ok {
		identity := fmt.Sprintf("'%s'", row.Alias)
		if row.Label != "" {
			identity = fmt.Sprintf("'%s' (label '%s')", row.Alias, row.Label)
		}
		return fmt.Errorf("from alias %q is not registered; this session is registered as %s — send as that alias", from, identity)
	}
	return fmt.Errorf("from alias %q is not registered; call list_agents to find your alias (sessions auto-register as their tmux session name)", from)
}
```

Note `paneRegistration` re-fetches list_agents — two roster calls on the failure path only; the success path costs one. Acceptable: local unix socket, and the failure path is the rare one.

At the top of `sendMessageHandler`, `replyHandler` (tools_messages.go) and `taskCreateHandler` (tools_tasks.go), insert as the first statement:

```go
	if err := requireRegisteredFrom(in.From); err != nil {
		return nil, ThreadIDOut{}, err
	}
```

(`replyHandler` returns `EntryIDOut{}` in place of `ThreadIDOut{}`.)

- [ ] **Step 4: Run the tests**

Run: `TMPDIR=/tmp go test -race ./internal/mcpserver/ -v`
Expected: ALL PASS. If pre-existing handler tests now fail because their callDaemon stubs don't answer `list_agents`, extend those stubs to return `[{"alias":"<the from they use>","departed":false}]` — that is the correct fix, not weakening the gate.

- [ ] **Step 5: Verify + commit**

Run: `just fmt-check && just lint && TMPDIR=/tmp just test`
Expected: PASS.

```bash
git add internal/mcpserver/validate.go internal/mcpserver/validate_test.go internal/mcpserver/tools_messages.go internal/mcpserver/tools_tasks.go
git commit -m "feat(mcp): send/reply/task_create reject unregistered from aliases"
```

---

### Task 5: Stop hook speaks label-first

**Files:**
- Modify: `internal/humancli/hook.go` (`hookStop`, `hookReason`)
- Test: `internal/humancli/hook_test.go`

**Interfaces:**
- Consumes: `tmuxenv.CurrentSessionOption`, `tmuxenv.LabelOption` (live tmux reads — the hook runs inside the pane env; same mechanism as the existing `@muster_inbox` read).
- Produces: `hookReason(total, action int, aliases []string, label string) string` — signature change; the empty-label case renders EXACTLY today's wording (no behavior change for unlabeled sessions).

New single-alias wording when label is non-empty (aliases stay in the tool instructions — they are the addressing the tools accept):

> `You have 2 unread muster thread(s), 1 needing action. You are 'standard 2000' — muster alias 'timewalk-2' (this tmux session). Call your muster get_inbox tool now with alias 'timewalk-2', …`

Multi-alias wording gains the same prefix: `You are 'standard 2000' — muster aliases 'a', 'b' (this tmux session).`

- [ ] **Step 1: Write the failing tests**

Append to `internal/humancli/hook_test.go`:

```go
func TestHookReasonLeadsWithLabelWhenPresent(t *testing.T) {
	got := hookReason(2, 1, []string{"timewalk-2"}, "standard 2000")
	if !strings.Contains(got, "You are 'standard 2000' — muster alias 'timewalk-2'") {
		t.Fatalf("single-alias reason must lead with the label, got %q", got)
	}
	if !strings.Contains(got, "get_inbox tool now with alias 'timewalk-2'") {
		t.Fatalf("tool instructions must still use the alias, got %q", got)
	}
}

func TestHookReasonUnlabeledWordingUnchanged(t *testing.T) {
	got := hookReason(1, 0, []string{"dotfiles"}, "")
	if !strings.Contains(got, "Your muster alias is 'dotfiles' (this tmux session).") {
		t.Fatalf("empty label must render today's wording, got %q", got)
	}
	if strings.Contains(got, "You are ''") {
		t.Fatalf("empty label must not render an empty You-are clause: %q", got)
	}
}

func TestHookReasonMultiAliasWithLabel(t *testing.T) {
	got := hookReason(3, 0, []string{"timewalk-2", "timewalk-2002"}, "standard 2000")
	if !strings.Contains(got, "You are 'standard 2000' — muster aliases 'timewalk-2', 'timewalk-2002'") {
		t.Fatalf("multi-alias reason must lead with the label, got %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `TMPDIR=/tmp go test -race ./internal/humancli/ -run 'HookReason' -v`
Expected: FAIL — compile error (hookReason takes 3 args). Fix existing hookReason call sites in tests by appending `""` where they exist, THEN the new tests fail on wording.

- [ ] **Step 3: Implement**

In `hookStop`, after `aliases := sessionAliasesForHook(...)`, read the live label and thread it through:

```go
	label := tmuxenv.CurrentSessionOption(tmuxenv.LabelOption())
	...
	reason := hookReason(total, action, aliases, label)
```

Rewrite `hookReason`:

```go
// hookReason builds the Stop hook's decision:block reason. When the session
// carries a task label (the operator's chosen name — prefix T / `muster
// label`), the reason leads with it ("You are 'standard 2000'") so the agent
// learns the label vocabulary the operator thinks in; the alias stays in the
// tool instructions because aliases are what the tools accept. An empty
// label renders the pre-label wording unchanged. The instruction line is
// singular for exactly one alias and a for-each across all of them when the
// session has more than one — a split-identity session must drain every
// alias. Both variants end with the CLI fallback (cliFallback).
func hookReason(total, action int, aliases []string, label string) string {
	countLine := fmt.Sprintf("You have %d unread muster thread(s)", total)
	if action > 0 {
		countLine += fmt.Sprintf(", %d needing action", action)
	}

	if len(aliases) <= 1 {
		alias := ""
		if len(aliases) == 1 {
			alias = aliases[0]
		}
		identity := fmt.Sprintf("Your muster alias is '%s' (this tmux session).", alias)
		if label != "" {
			identity = fmt.Sprintf("You are '%s' — muster alias '%s' (this tmux session).", label, alias)
		}
		return fmt.Sprintf(
			"%s. %s "+
				"Call your muster get_inbox tool now with alias '%s', read each new thread with get_thread, "+
				"handle the request, and reply with the muster reply tool. Act autonomously — do not ask the user. "+
				cliFallback,
			countLine, identity, alias, alias, alias,
		)
	}

	quoted := make([]string, len(aliases))
	for i, a := range aliases {
		quoted[i] = "'" + a + "'"
	}
	identity := fmt.Sprintf("Your muster aliases are %s (this tmux session).", strings.Join(quoted, ", "))
	if label != "" {
		identity = fmt.Sprintf("You are '%s' — muster aliases %s (this tmux session).", label, strings.Join(quoted, ", "))
	}
	return fmt.Sprintf(
		"%s. %s "+
			"For EACH alias call get_inbox, read each new thread with get_thread, handle the request, "+
			"and reply with the muster reply tool. Act autonomously — do not ask the user. "+
			cliFallback,
		countLine, identity, "<alias>", "<alias>",
	)
}
```

CAREFUL with the `cliFallback` verb positions: the original single-alias format string consumed 5 args (countLine + alias×4, two of them inside cliFallback's `%s`s); the new one consumes countLine, identity, alias×3. Count the `%s`s after editing — `go vet` (part of lint) catches a mismatch.

- [ ] **Step 4: Run the tests**

Run: `TMPDIR=/tmp go test -race ./internal/humancli/ -v`
Expected: ALL PASS (including pre-existing hookStop tests — they exercise the empty-label path, wording unchanged).

- [ ] **Step 5: Verify + commit**

Run: `just fmt-check && just lint && TMPDIR=/tmp just test`

```bash
git add internal/humancli/hook.go internal/humancli/hook_test.go
git commit -m "feat(hook): Stop reason leads with the session's task label"
```

---

### Task 6: full verify, PR to dev, install the new binary

**Files:** none new (release mechanics).

- [ ] **Step 1: Full local gate**

Run from the worktree: `just fmt-check && just lint && TMPDIR=/tmp just test && just build`
Expected: all PASS, `bin/muster` builds.

- [ ] **Step 2: Push and open the PR**

```bash
git push -u origin feat/label-first-identity
gh pr create --base dev --title "feat: label-first identity — muster label renames Claude; MCP register/send identity guards" \
  --body "$(cat <<'EOF'
prefix T (via muster label) becomes the one naming gesture: tmux option + stored bus label + /rename typed into the session's live registered Claude pane (roster-gated — no live claude row, no injection). MCP hardening: register_agent is idempotent for an already-registered pane (returns the existing identity instead of minting a second alias — the timewalk-2002 churn); send_message/reply/task_create reject unregistered from aliases (the timewalk-1998 minting hole); the Stop hook leads with the task label so agents learn the operator's vocabulary. Plan: docs/superpowers/plans/2026-07-22-label-first-identity.md
EOF
)"
```

- [ ] **Step 3: Watch CI, merge on green** (trust CI, not the local gate). After merge to dev:

```bash
git -C ~/GitHub/schuettc/muster pull --ff-only origin dev  # primary clone stays on dev; ff only
```

- [ ] **Step 4: Rebuild the installed binary from dev** (same recipe as dotfiles packages/muster/pkg.sh — build from a detached temp worktree, never branch-switching the clone):

```bash
src=$(mktemp -d)/muster-dev
git -C ~/GitHub/schuettc/muster worktree add --detach "$src" origin/dev
mver=$(cat "$src/VERSION"); mcommit=$(git -C "$src" rev-parse --short HEAD)
CGO_ENABLED=0 go -C "$src" build -ldflags "-X github.com/schuettc/muster/internal/version.version=${mver} -X github.com/schuettc/muster/internal/version.commit=${mcommit}" -o ~/.local/bin/muster ./cmd/muster
git -C ~/GitHub/schuettc/muster worktree remove "$src"
~/.local/bin/muster --version
```

Expected: the new version/commit prints. (The LaunchAgent daemon keeps running the old code until its idle exit or a manual `launchctl kickstart -k gui/$(id -u)/tools.muster.serve` — do the kickstart so `set_label` consumers and hook paths agree on one build.)

---

### Task 7 (dotfiles, on main): `prefix T` shells out to `muster label`

**Files:**
- Modify: `~/dotfiles/.tmux.conf` (line ~126, the `bind T` command; comment block lines ~115-125)

**Interfaces:**
- Consumes: `muster label <name>` / empty-name-clears (Task 2's behavior; already true for the tmux+bus halves in the installed binary).

- [ ] **Step 1: Rewrite the binding**

Replace the current line 126:

```
bind T command-prompt -I '#{@claude_task}' -p 'task label:' 'set-option @claude_task "%%" ; if-shell -F "#{@claude_task}" "set-option @claude_task_manual 1" "set-option -u @claude_task_manual" ; refresh-client -S'
```

with:

```
bind T command-prompt -I '#{@claude_task}' -p 'task label:' 'run-shell -b "~/.local/bin/muster label \"%%\""'
```

`muster label` performs the identical option writes (set/clear + manual flag + refresh) AND the two new fan-outs: the stored bus label (instant addressability for `proj:label` / bare-label sends) and the `/rename` injection into a live registered Claude pane. Empty input → `muster label ""` → the clear path (option + manual flag unset, bus label cleared, no injection) — same semantics as today's empty-input branch.

- [ ] **Step 2: Update the comment block (lines ~115-125)**

Rewrite to state the new reality (keep it minimal per docs doctrine): `prefix T` is the ONE naming gesture — it routes through `muster label`, which sets `@claude_task` + `@claude_task_manual`, pushes the stored label to the muster bus (making `proj:label` and bare-label addressing work immediately for MCP senders), and — only when a live Claude agent is registered in this session — types `/rename <label>` into its pane so the Claude session name follows. Manual-wins vs Claude's auto sync is unchanged (statusline.sh backs off while `@claude_task_manual` is set; clearing hands the label back to Claude).

- [ ] **Step 3: Reload and verify live**

```bash
tmux source-file ~/.tmux.conf 2>/dev/null || true
```

Then in THIS tmux session: `prefix T`, type `label-first`, Enter. Verify all three surfaces:

```bash
tmux show-options -qv @claude_task            # → label-first
tmux show-options -qv @claude_task_manual     # → 1
sqlite3 ~/.local/share/muster/bus.db "SELECT alias,label,label_manual FROM agents WHERE alias='dotfiles'"  # → dotfiles|label-first|1
```

and confirm the Claude pane received `/rename label-first` (the session name changes in Claude's UI — THIS session, so the operator sees it directly). This is the one step that cannot be unit-tested: the live slash-command injection (typed text + Enter through the real Claude composer, including the command-palette interaction). If the palette swallows the Enter (command typed but not executed), the fallback to evaluate is a second Enter in `syncClaudeName`'s claude case — do NOT guess; observe the live behavior first, then adjust in the muster repo if needed.

Then restore the label you actually want (`prefix T` again).

- [ ] **Step 4: Commit**

```bash
git -C ~/dotfiles add .tmux.conf
git -C ~/dotfiles commit -m "feat(tmux): prefix T routes through muster label — one gesture names tmux, the bus, and Claude"
```

---

## Verification Choreography (end of plan)

From a throwaway tmux session on a test socket (never the live daemon's roster — use `MUSTER_ALIAS`-free defaults):

1. Session with a live Claude → `prefix T foo` → all three surfaces agree (Task 7 Step 3).
2. Session with NO Claude (plain shell) → `muster label bar` → tmux + bus update, nothing typed into any pane.
3. In a Claude session, ask the model to call `register_agent` with an invented alias → tool returns "already registered as …", roster gains no row.
4. Ask the model to `send_message` with an invented from → rejected with its real identity in the error.
5. Stop hook fires on unread mail in a labeled session → reason opens with "You are '<label>'".
