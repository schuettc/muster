# Hook Pane Ownership Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `muster hook` events act only when the calling pane owns the session's registered identity, so harness subagents (full Claude sessions in sibling panes) stop stealing registrations, draining the primary conversation's mail, and tombstoning it on exit.

**Architecture:** Client-side ownership checks in `internal/humancli`'s hook paths using the existing `get_agent` op + a new `tmuxenv.IsPaneAlive` helper. No daemon/store/wire changes; explicit CLI `register`/`deregister` and MCP `register_agent` keep their current steal semantics.

**Tech Stack:** Go, worktree `~/GitHub/schuettc/muster-wt-pane-owner`, branch `feat/hook-pane-ownership` off `dev` (base 5e29930).

**Spec:** `docs/superpowers/specs/2026-07-18-hook-pane-ownership-design.md` (this worktree) — read it first; it defines the rule, the per-event semantics, and the degrade-to-today failure posture.

## Global Constraints

- Only these files change: `internal/tmuxenv/tmuxenv.go`, `internal/tmuxenv/tmuxenv_test.go`, `internal/humancli/hook.go`, `internal/humancli/hook_test.go` (+ `internal/humancli/identity.go` ONLY if the alias-resolution helper naturally extracts there).
- Hooks must never block or error a session: every new check degrades to TODAY'S behavior when the daemon or tmux is unreachable, and `cmdHook` keeps returning nil.
- The gate engages only when the roster names an owner pane and it isn't the caller; empty stored `pane_id` = no owner named = behave as today (this keeps every existing hook test passing unmodified — if an existing test needs editing, stop and re-read the spec).
- Explicit `muster register` / `muster deregister` commands are NOT gated.
- Gate before commit: `just verify` from the worktree root.

---

### Task 1: `IsPaneAlive` + the three hook gates

**Files:**
- Modify: `internal/tmuxenv/tmuxenv.go` (helper beside `IsSessionAlive`), `internal/tmuxenv/tmuxenv_test.go`
- Modify: `internal/humancli/hook.go` (gates + small helpers), `internal/humancli/hook_test.go`

**Interfaces:**
- Produces: `tmuxenv.IsPaneAlive(socket, paneID string) bool` — true iff `query(socket, paneID, "#{pane_id}")` returns non-empty; empty socket/pane → false. Unexported hook helpers in `hook.go`: `hookAlias(c tmuxenv.Capture) string` ($MUSTER_ALIAS else `c.SessionName` — mirrors cmdRegister/cmdDeregister's no-arg precedence), `hookGetAgent(alias string) (agentFull, bool)` (calls `get_agent`, decodes `{"found":bool,"agent":{...}}`; false on any error — check whether an equivalent fetch already exists near nudge's pane resolution and reuse it instead of duplicating), `hookMayClaimIdentity(c) bool`, `hookOwnsIdentity(c) bool`.

- [ ] **Step 1: Failing tests — `IsPaneAlive`** (`tmuxenv_test.go`, via the `Run` override like `IsSessionAlive`'s tests): alive (stub returns `"%5"` → true), dead (stub returns error → false), empty pane / empty socket → false without calling Run.

- [ ] **Step 2: Failing tests — hook gates** (`hook_test.go`, `startTestDaemon(t)` + `callData` registrations WITH `pane_id` args + `t.Setenv("TMUX", …)/("TMUX_PANE", …)` + `hookRun` stubs; `IsPaneAlive`'s query keys on `"#{pane_id}"` in the hookRun map). Cover, with one test function per behavior:
  - SessionStart no-op: alias registered to same tuple, foreign pane `%1`, stub `"#{pane_id}": "%1"` (alive), my pane `%2` → after `cmdHook([]string{"SessionStart"})` the roster's pane_id is STILL `%1` (assert via `callData("get_agent", …)`).
  - SessionStart claims when: foreign pane dead (stub returns `""` for `"#{pane_id}"`), stored pane empty, stored pane == mine, alias absent, foreign tuple. (Table-drive these; assert roster pane_id becomes mine.)
  - Stop silent for non-owner: session alias pane `%1` alive, my pane `%2`, `@muster_inbox` stub `"3"`, unread mail seeded → `cmdHook` Stop emits NOTHING.
  - Stop drains for owner: same but my pane `%1` → block decision emitted (reuse `runHook`).
  - Stop fallback preserved: registration with EMPTY pane_id → drains exactly as today (this asserts the existing-tests-keep-passing property explicitly).
  - SessionEnd no-op for non-owner (roster keeps the agent un-departed); SessionEnd deregisters for owner; SessionEnd no-op when alias registered to a different tuple.

- [ ] **Step 3: Run new tests, verify they fail** (`go test ./internal/tmuxenv/ ./internal/humancli/ -run 'PaneAlive|HookSessionStart|HookStop.*Owner|HookStop.*Pane|HookSessionEnd' -v`).

- [ ] **Step 4: Implement.** `IsPaneAlive` per the Produces block. In `hook.go`:

```go
// hookMayClaimIdentity is the SessionStart gate (spec: first live claimant
// wins the session's primary-agent pane). Degrades to true — today's
// register — whenever tmux identity or the roster can't answer.
func hookMayClaimIdentity(c tmuxenv.Capture) bool {
	if c.SocketPath == "" || c.PaneID == "" {
		return true
	}
	ag, found := hookGetAgent(hookAlias(c))
	if !found {
		return true
	}
	if ag.SocketPath != c.SocketPath || ag.SessionID != c.SessionID {
		return true // cross-session takeover: a renamed/recreated session reclaims its name
	}
	if ag.PaneID == "" || ag.PaneID == c.PaneID {
		return true
	}
	return !tmuxenv.IsPaneAlive(c.SocketPath, ag.PaneID)
}

// hookOwnsIdentity is the SessionEnd gate: deregister only what this pane
// owns — a dying sibling (subagent) must not tombstone the primary.
func hookOwnsIdentity(c tmuxenv.Capture) bool {
	ag, found := hookGetAgent(hookAlias(c))
	if !found {
		return false
	}
	if ag.SocketPath != c.SocketPath || ag.SessionID != c.SessionID {
		return false
	}
	return ag.PaneID == "" || ag.PaneID == c.PaneID
}
```

  Wire into `cmdHook`'s SessionStart/SessionEnd cases (capture env once). In `hookStop`, after the alias list is obtained, insert the pane gate: if my `TMUX_PANE` is non-empty and at least one alias resolves to a non-empty `pane_id` (via `hookGetAgent`) but NONE equals mine → return silently; otherwise proceed exactly as today. Match the file's existing comment voice; comments state the ownership rule and the degrade posture, not the change history.

- [ ] **Step 5: New tests green, then the full pair of packages** (`go test -race ./internal/tmuxenv/ ./internal/humancli/`) — existing hook tests must pass UNMODIFIED.

- [ ] **Step 6: `just verify`, then commit** (`feat(hook): pane ownership — subagent sessions no longer claim, drain, or tombstone the primary`; append the session trailer).

### Task 2: Sandboxed e2e, VERSION 0.7.1, PR to dev

**Files:**
- Modify: `VERSION` (`0.7.0` → `0.7.1` — behavior fix, patch bump)

**Interfaces:**
- Consumes: Task 1's binary behavior. Produces: PR `feat/hook-pane-ownership → dev`, not merged.

- [ ] **Step 1: Build sandbox binary** to a SHORT path (`/tmp/muster-po-e2e/muster`; macOS `sun_path` ~104-char limit rules out the deep scratchpad — same lesson as the badge e2e).

- [ ] **Step 2: e2e against a throwaway tmux server** (isolated `MUSTER_HOME=/tmp/muster-po-e2e/home`, tmux `-S /tmp/muster-po-e2e/tmux`, session `sbx` with TWO panes; capture their `%ids` as A and B; the live daemon/tmux/binary stay untouched). Drive the hook binary itself with `TMUX=<sock>,1,0 TMUX_PANE=<pane>` env per call:
  1. `TMUX_PANE=A muster hook SessionStart claude` → `debug get_agent alias=sbx` shows pane A.
  2. `TMUX_PANE=B muster hook SessionStart claude` → pane STILL A (no steal).
  3. Seed mail to `sbx`; set `@muster_inbox 1` on the session; `TMUX_PANE=B ... hook Stop` (stdin `{}`) → NO output; `TMUX_PANE=A ... hook Stop` → block JSON.
  4. `TMUX_PANE=B ... hook SessionEnd` → agent still not departed; `TMUX_PANE=A ... hook SessionEnd` → departed.
  5. Re-register from A, kill pane A (`tmux kill-pane`), `TMUX_PANE=B ... hook SessionStart` → pane becomes B (dead-owner takeover).
  Capture observed vs expected at each step; tear the sandbox down even on failure.

- [ ] **Step 3: VERSION → `0.7.1`, commit** (`chore: bump VERSION to 0.7.1 (hook pane ownership)` + session trailer).

- [ ] **Step 4: Push; `gh pr create --base dev`** titled `fix: hook pane ownership — subagent sessions must not act as the session's agent`, body summarizing the three misbehaviors + the rule + spec path. Report PR URL + CI. Do NOT merge.

---

## Self-Review Notes

- Spec coverage: rule 1 → hookMayClaimIdentity + SessionStart tests; rule 2 → hookStop gate + owner/non-owner/fallback tests; rule 3 → hookOwnsIdentity + SessionEnd tests (incl. cross-tuple); helper → IsPaneAlive + tests; scoping (CLI/MCP ungated, no daemon change) → enforced by the file allowlist; failure posture → degrade branches in both helpers and the Stop gate's "only when an owner is named" condition.
- Existing-tests-unmodified is stated twice deliberately (Global Constraints + Task 1 Step 5): it is the regression contract for pane-less registrations.
- Stop gate deliberately does NOT check IsPaneAlive (dead owner → silence until the next SessionStart claims the identity); noted in spec, matched here.
