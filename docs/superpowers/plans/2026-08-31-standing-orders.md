# Standing orders — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** A dedicated, keyed, retractable per-project standing-order concept on top of the shipped standing-broadcast transport: `muster standing set/retract/list`, superseding by `(project, key)` instead of appending.

**Architecture:** A standing order IS a standing broadcast thread carrying an identity (`standing_key`) and a lifecycle (`standing_retracted`). `set` is idempotent create-or-replace by `(project, key)` in one transaction (retract prior live order under the key, then create the replacement). `retract` marks the current order retracted. `list` returns live (non-retracted) orders. The shipped standing-watermark delivery + scoped-broadcast validation are reused verbatim; the only new unread rule is a `standing_retracted = 0` guard on the standing branch. Both stores change together, gated by conformance.

**Tech Stack:** Go, pure-Go SQLite, no cgo. Spec: `docs/superpowers/specs/2026-08-31-standing-orders-convention-design.md`.

## Global Constraints

- Worktree `/Users/courtschuett/GitHub/worktrees/muster-standing-orders`, branch `feat/standing-orders` (off `dev`). Never touch the primary clone.
- Gate: `just verify` (fmt, lint, `go test -race`, cross-compile, aws-free) + `just verify-dynamo` (both backends). Per-task, run the named package tests.
- `CGO_ENABLED=0` must keep building; add no deps beyond what's present.
- **Both stores or neither.** Every store behavior change lands in `internal/store` AND `internal/dynamostore`, proven equal by `internal/storetest/conformance.go`.
- Do not regress the shipped v1 behavior: un-keyed `--standing` broadcasts (`standing_key=''`) stay append-only and are never returned by `list`; the register seed / standing watermark / `MarkRead` dual-advance are unchanged.
- Identity is `(to_target, standing_key)` among **live** (`standing_retracted=0`) standing broadcast rows. `--key` defaults to `invariants`.

---

### Task 1: Store schema, migration, models

**Files:** `internal/store/schema.sql`, `internal/store/store.go` (migrations), `internal/store/models.go`.

- [ ] **Step 1:** Test asserts `threads.standing_key` and `threads.standing_retracted` exist with defaults `''` / `0`, migration idempotent.
- [ ] **Step 2:** Verify fail.
- [ ] **Step 3:** Add both columns to the `threads` CREATE TABLE and to the `alters` slice: `ALTER TABLE threads ADD COLUMN standing_key TEXT NOT NULL DEFAULT ''` and `ALTER TABLE threads ADD COLUMN standing_retracted INTEGER NOT NULL DEFAULT 0`. Add `StandingKey string` + `StandingRetracted bool` to `Thread` (one-line comments: order identity within project / lifecycle tombstone; broadcast-only).
- [ ] **Step 4/5:** Store tests; commit `store: standing_key + standing_retracted columns`.

---

### Task 2: Store — standing-order verbs + retracted unread guard

**Files:** `internal/store/threads.go` (new `SetStandingOrder`, `RetractStandingOrder`, `ListStandingOrders`; unread branch guard in `Inbox`/`UnreadCount`), `internal/store/agents.go` (`SessionUnread` guard), `internal/store/models.go` (a `StandingOrder` result struct), `internal/store/api.go` if there's an interface.

**Interfaces:**
- `SetStandingOrder(project, key, from, body string) (int64, error)` — one tx: `UPDATE threads SET standing_retracted=1 WHERE to_kind='broadcast' AND standing=1 AND to_target=? AND standing_key=? AND standing_retracted=0`, then insert a thread (`kind=message, from_agent=from, to_kind=broadcast, to_target=project, standing=1, standing_key=key, origin_project=<sender's>`) + its first entry (body). Returns new thread id. `key` defaults handled by callers (daemon/CLI), but empty `key` is rejected here (a keyed order must have a key).
- `RetractStandingOrder(project, key string) (bool, error)` — sets `standing_retracted=1` on the live order under `(project,key)`; returns whether a row changed; idempotent (absent/already-retracted → false, nil).
- `ListStandingOrders(project string) ([]StandingOrder, error)` — live orders for the project (`standing=1 AND standing_key!='' AND standing_retracted=0 AND to_target=project`), each `{Key, Body, From, CreatedAt}` (Body = the order's first entry), sorted by key.
- **Unread guard:** the standing branch in `Inbox`, `UnreadCount`, `SessionUnread` gains `AND recent.standing_retracted = 0` (join form: `r.`/`sess.` as appropriate) so a retracted order greets no future session and never re-surfaces as unread. Un-keyed standing broadcasts (retracted=0) unaffected.

- [ ] **Step 1:** Tests — set creates a listable order; second set under same key replaces (list shows one, new body; a NEW session sees only the new order unread); retract drops from list and from a new session's unread; retract idempotent; empty key rejected; un-keyed standing broadcast never in list; a session that already read an order is unaffected by a later retract.
- [ ] **Step 2:** Verify fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4/5:** Store tests (`-race`); commit `store: SetStandingOrder/RetractStandingOrder/ListStandingOrders + retracted unread guard`.

---

### Task 3: DynamoDB store — mirror verbs + retracted guard

**Files:** `internal/dynamostore/threads.go`, item builders (`standing_key`/`standing_retracted` on the thread item + `itemToThread`), `SetStandingOrder`/`RetractStandingOrder`/`ListStandingOrders`, and the `standing_retracted=0` guard in `unreadByThread`/`standingSet` filtering.

**Interfaces:** identical semantics to Task 2. For set's atomic retract-then-create, use a `TransactWriteItems` (retract prior via a conditional update if present + put the new thread meta + first entry) or the store's existing thread-create transaction extended with a preceding retract; keep it a single logical operation. `standingSet` (from v1) must exclude retracted rows so the unread branch never counts a retracted order.

- [ ] **Step 1:** dynamostore mirror tests (or rely on Task 4 conformance per package convention).
- [ ] **Step 2:** Verify fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4/5:** `just verify-dynamo`; commit `dynamostore: mirror standing-order verbs + retracted guard`.

---

### Task 4: Conformance — standing-order lifecycle across both stores

**Files:** `internal/storetest/conformance.go`.

Cases (both backends): set → listable + greets a new same-project session once; second set same key → replaces (one listed, new body, new session sees only the new); retract → gone from list + gone from a new session's unread; retract idempotent; empty key rejected; scope validation is the daemon's job (not here) but `Set`/`Retract`/`List` on a bare store work by project string; un-keyed standing broadcast never listed; other-project session never sees a scoped order; a session that read an order before retract is unaffected.

- [ ] **Steps:** add cases; run both stores; commit `storetest: standing-order lifecycle conformance`.

---

### Task 5: Daemon — standing_set / standing_retract / standing_list ops

**Files:** `internal/daemon/daemon.go`.

- `standing_set`: args `from`, `project`, `key` (default `invariants` if empty), `body`. Reuse `validateBroadcastTarget(project)` (reject unknown project with known-projects error). Call `SetStandingOrder`; journal a `standing` event; `notifyForThread` on the new thread (live sessions in the project get it now, exactly like a scoped standing broadcast).
- `standing_retract`: args `project`, `key` (default `invariants`). Validate project. Call `RetractStandingOrder`; journal.
- `standing_list`: args `project`. Return `ListStandingOrders`.

- [ ] Tests: set creates a listable/greeting order; retract stops greeting; list returns live only; unknown project rejected on set/retract; default key applied when omitted. Commit `daemon: standing_set/retract/list ops`.

---

### Task 6: MCP — standing tools

**Files:** `internal/mcpserver/tools_messages.go` (or a new `tools_standing.go`), registration.

- `standing_set` (from, project, key?, body), `standing_retract` (project, key?), `standing_list` (project) → `[]StandingOrderView`. Descriptions frame this as the durable per-project convention (set/replace/retract/list), distinct from ad-hoc `send_message --standing`. `standing_list --json` is the audit seam the onboarding skill reads.

- [ ] Tests + commit `mcp: standing set/retract/list tools`.

---

### Task 7: CLI — `muster standing` command group

**Files:** `internal/humancli/` (new `standing.go`), `registry.go` (register `standing` in an appropriate group), help/synopsis.

- `muster standing <project> [--json]` — list (default verb when only a project is given).
- `muster standing set <project> [--key <k>] "body" [--from <alias>]`.
- `muster standing retract <project> [--key <k>]`.
- `--key` defaults to `invariants`. Help explains the convention + audit use.

- [ ] Tests: set/list/retract round-trip through the CLI; `--json` shape; default key. Commit `cli: muster standing set/retract/list`.

---

### Task 8: Gate + end-to-end + PR

- [ ] `just verify` + `just verify-dynamo` green.
- [ ] E2E on an isolated bus via the real binary: `standing set web --key invariants "rules"`; register a new session in `web` → it sees the order once; `standing list web --json` shows it; `standing set web --key invariants "rules v2"` → list shows one (v2); a newer session sees only v2; `standing retract web` → list empty + a newer session sees nothing.
- [ ] Finish the branch per finishing-a-development-branch: push + open PR against `dev`; ping muster thread 346 (tool-standards) with the spec path + PR link for review.
