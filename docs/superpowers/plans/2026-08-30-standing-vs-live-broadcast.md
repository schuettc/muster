# Standing vs Live Broadcast Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop a new session inheriting the entire broadcast backlog, and split broadcast into two behaviors: plain `--broadcast` is **live-only** (reaches sessions live at/after send; invisible to sessions that start later), and `--broadcast --standing` reaches every session that starts later, once, until it reads.

**Architecture:** Two changes, both riding the existing watermark machinery. (1) `RegisterAgent`'s INSERT branch seeds `last_read_entry_id = MAX(entries.id)` so a first-seen alias starts caught-up — plain broadcast becomes live-only. (2) A `standing` bit on the broadcast thread plus a second, un-seeded watermark `last_read_standing_entry_id` on the agent: the unread computation picks the standing watermark for standing threads (0 for a new session → the standing backlog shows once) and the ordinary watermark for everything else. `MarkRead` advances both. The concern predicate is unchanged; only unread computation branches. Both stores (SQLite + DynamoDB) change together, gated by the conformance suite.

**Tech Stack:** Go, pure-Go SQLite (`modernc.org/sqlite`), no cgo. Spec: `docs/superpowers/specs/2026-08-30-standing-vs-live-broadcast-design.md`.

## Global Constraints

- Work in the worktree `/Users/courtschuett/GitHub/worktrees/muster-standing-broadcast` on branch `feat/standing-vs-live-broadcast` (off `dev`). Never touch the primary clone.
- The full gate is `just verify` (gofmt, golangci-lint, `go test -race`, build). Run it before the final commit; per-task, run the named package tests.
- `CGO_ENABLED=0` must keep building. Add no dependencies.
- stdout is sacred in mcp mode; no stray prints (all diagnostics to stderr — this plan adds none).
- **Both stores or neither.** Every store-layer behavior change lands in `internal/store` AND `internal/dynamostore`, and is proven equal by a case in `internal/storetest/conformance.go`. A change that passes SQLite tests but not conformance is not done.
- **Watermark invariants that must not regress:** the ON CONFLICT (re-register) branch never touches either watermark (a returning alias keeps read-state — `TestRegisterAgentRevivePreservesReadState` and siblings); `MarkRead` self-exclusion (`from_agent != alias`) is retained everywhere.
- Standing is **message-broadcast-only**: `standing=1` requires `to_kind='broadcast'`; a standing task is rejected. `--standing` requires `--broadcast`.

---

### Task 1: Store schema, migration, and models

**Files:**
- Modify: `internal/store/schema.sql` (agents +`last_read_standing_entry_id`, threads +`standing`)
- Modify: `internal/store/store.go:43-58` (append two `ALTER TABLE` migrations)
- Modify: `internal/store/models.go` (`Agent.LastReadStandingEntryID int64`; `Thread.Standing bool`)
- Test: `internal/store/store_test.go` (migration idempotency / column presence)

**Interfaces:**
- Produces: fresh DBs and migrated DBs both have `agents.last_read_standing_entry_id INTEGER NOT NULL DEFAULT 0` and `threads.standing INTEGER NOT NULL DEFAULT 0`. `Agent` carries `LastReadStandingEntryID`; `Thread` carries `Standing`. Later tasks read/write these.

- [ ] **Step 1: Write the failing test** — a test that opens a store, inserts an agent + a broadcast thread, and asserts the two new columns exist with correct defaults (0 / false); and that `migrate()` is idempotent across two `Open`s on the same file.
- [ ] **Step 2: Run to verify it fails.**
- [ ] **Step 3: Implement** — add the column to both `CREATE TABLE` blocks in `schema.sql`; append to the `alters` slice in `store.go`:
  ```
  `ALTER TABLE agents ADD COLUMN last_read_standing_entry_id INTEGER NOT NULL DEFAULT 0`,
  `ALTER TABLE threads ADD COLUMN standing INTEGER NOT NULL DEFAULT 0`,
  ```
  Add `LastReadStandingEntryID int64` to `Agent` and `Standing bool` to `Thread` in `models.go` with a one-line comment on each (standing = replayed to future sessions until read; the standing watermark is un-seeded on register). No backfill needed — both default correctly for existing rows (pre-upgrade broadcasts become live-only; that is the intended migration, per spec).
- [ ] **Step 4: Run the store package tests.**
- [ ] **Step 5: Commit** — `store: add standing column + standing read watermark (schema+migration)`.

---

### Task 2: Store — seed new-session watermark; dual-advance MarkRead; standing-aware unread

**Files:**
- Modify: `internal/store/agents.go` — `RegisterAgent` INSERT (seed), column lists in `scanAgent`/`GetAgent`/`ListAgents`, `MarkRead` (dual advance), `UnreadCount` (branch)
- Modify: `internal/store/threads.go` — `Inbox` unread CTE (branch), select `recent.standing`
- Modify: `internal/store/poll.go` — `SessionUnread` (branch) if it computes unread there
- Test: `internal/store/agents_test.go`, `internal/store/threads_test.go`

**Interfaces:**
- Consumes: Task 1 columns/fields.
- Produces: (a) first INSERT seeds `last_read_entry_id = COALESCE((SELECT MAX(id) FROM entries),0)`, `last_read_standing_entry_id` left at 0; ON CONFLICT unchanged. (b) The unread predicate everywhere is:
  ```sql
  ((recent.standing = 1 AND e.id > COALESCE((SELECT last_read_standing_entry_id FROM agents WHERE alias=?),0))
   OR (recent.standing = 0 AND e.id > COALESCE((SELECT last_read_entry_id FROM agents WHERE alias=?),0)))
  AND e.from_agent != ?
  ```
  (join form uses `sess.*` columns). (c) `MarkRead` sets both watermarks to the snapshot max entry id in one tx. Later tasks and conformance rely on: a plain broadcast before register is hidden; a standing broadcast before register shows once then clears; both scoped forms respect project concern.

- [ ] **Step 1: Write the failing tests** in `agents_test.go` / `threads_test.go`:
  - `TestRegisterSeedsLiveWatermarkToMax`: create entries, then register a fresh alias → its `LastReadEntryID == max entry id`, `LastReadStandingEntryID == 0`.
  - `TestNewSessionSkipsPlainBroadcastBacklog`: send a plain broadcast, THEN register a new alias → not in its inbox / unread 0.
  - `TestNewSessionSeesStandingBroadcastOnce`: send a standing broadcast, THEN register → unread 1; `MarkRead`; unread 0; send a second standing broadcast → unread 1.
  - `TestReviveKeepsBothWatermarks`: existing revive test extended to assert `LastReadStandingEntryID` is preserved across depart→re-register.
  - `TestMarkReadAdvancesBothWatermarks`.
- [ ] **Step 2: Run to verify they fail.**
- [ ] **Step 3: Implement** — seed in the INSERT VALUES (add `last_read_entry_id` column with the `(SELECT COALESCE(MAX(id),0) FROM entries)` subselect; keep it OUT of the ON CONFLICT SET list). Add `last_read_standing_entry_id` to every agent column list + `scanAgent`. Branch the unread predicate in `Inbox`, `UnreadCount`, `SessionUnread`; add `recent.standing` (and the join form's `sess.last_read_standing_entry_id`) to the relevant CTEs; update every affected bind-arg list (the standing watermark subselect adds one bind per surface — update counts carefully).
- [ ] **Step 4: Run the store package tests** (`go test ./internal/store/... -race`).
- [ ] **Step 5: Commit** — `store: live-only default via register seed; standing watermark unread + MarkRead`.

---

### Task 3: Store — CreateThread persists `standing`; reject non-broadcast standing

**Files:**
- Modify: `internal/store/threads.go` — `CreateThread` (accept + persist `standing`; guard)
- Modify: `internal/store/models.go` if a create-params struct carries the flag
- Test: `internal/store/threads_test.go`

**Interfaces:**
- Produces: `CreateThread` writes `threads.standing`; returns an error if `standing && to_kind != 'broadcast'`. Daemon (Task 5) is the primary gate; this is defense in depth.

- [ ] **Step 1: Write failing tests** — standing broadcast round-trips `Standing==true` via `GetThread`/`Threads`/`Inbox`; `CreateThread` with standing + non-broadcast target errors.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run store tests.**
- [ ] **Step 5: Commit** — `store: CreateThread persists standing, rejects non-broadcast standing`.

---

### Task 4: DynamoDB store — mirror register seed, standing watermark, standing thread

**Files:**
- Modify: `internal/dynamostore/agents.go` — `RegisterAgent` seed live watermark to current max event id (first-insert only); persist/scan `last_read_standing_entry_id`
- Modify: `internal/dynamostore/threads.go` — `unreadByThread`/`unreadFor`/`Inbox`/`UnreadCount`/`SessionUnread` branch on the thread's `standing` and the standing watermark; `MarkRead` advances both; persist `standing` on the thread item
- Modify: `internal/dynamostore/store.go` if item marshalling lives there
- Test: `internal/dynamostore/*_test.go` (mirrors of the store tests; keep any existing local-only harness gating)

**Interfaces:**
- Consumes: `store.Agent.LastReadStandingEntryID`, `store.Thread.Standing`.
- Produces: DynamoDB behaves identically to SQLite for every case in Task 5's conformance additions. Note `unreadByThread(..., after int64)` currently takes ONE watermark — extend it to take both (or split standing vs non-standing entries and apply each), keeping the existing self-exclude semantics.

- [ ] **Step 1: Write failing tests** (dynamostore mirrors, or rely on Task 5 conformance if that is the package's convention — check how dynamostore tests are structured first).
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — register seed uses the current max event/entry id; `unreadByThread` takes `liveAfter, standingAfter int64` and picks per entry by its thread's standing; `MarkRead` writes both `#last_read_entry_id` and `#last_read_standing_entry_id`; thread item carries `standing`.
- [ ] **Step 4: Run dynamostore tests.**
- [ ] **Step 5: Commit** — `dynamostore: mirror register seed + standing watermark + standing thread`.

---

### Task 5: Conformance — standing vs live across both stores

**Files:**
- Modify: `internal/storetest/conformance.go`
- Test: runs via both `internal/store` and `internal/dynamostore` conformance entrypoints

**Interfaces:**
- Produces: the canonical cross-store contract. Cases (each asserted on both backends):
  - fresh register seeds live watermark to max; plain broadcast sent before register is hidden.
  - standing broadcast sent before register shows once, clears on MarkRead, a later standing broadcast reappears.
  - plain broadcast sent AFTER register appears (live contract intact).
  - revive preserves both watermarks.
  - standing composes with project scope (same-project new alias sees it; other-project never does).
  - `MarkRead` zeroes unread for both standing and live threads.

- [ ] **Step 1: Add the cases.**
- [ ] **Step 2: Run conformance against both stores** (the store-level `go test` and the dynamostore entrypoint / its local harness).
- [ ] **Step 3: Commit** — `storetest: standing vs live broadcast conformance`.

---

### Task 6: Daemon — validate standing at send time

**Files:**
- Modify: `internal/daemon/daemon.go` — `send_message` op handler: pass `standing` through to `CreateThread`; reject `standing && to_kind != 'broadcast'` with a clear usage error; compose with existing project-scope validation
- Test: `internal/daemon/broadcast_test.go` (or a new `standing_test.go`)

**Interfaces:**
- Consumes: op fields; `store.CreateThread` standing param.
- Produces: a standing broadcast op creates a standing thread; standing + non-broadcast is rejected; live wake fan-out (`notifyForThread`) is UNCHANGED (standing does not alter who is badged live).

- [ ] **Step 1: Write failing tests** — standing broadcast op → stored thread `Standing==true`; standing + `to_kind=agent` rejected; standing scoped broadcast to unknown project still rejected with known-projects error; wake fan-out identical for standing vs plain.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run daemon tests (`-race`).**
- [ ] **Step 5: Commit** — `daemon: accept + validate standing broadcasts`.

---

### Task 7: MCP — `standing` param on send_message

**Files:**
- Modify: `internal/mcpserver/tools_messages.go` (schema + plumb `standing`)
- Modify: `internal/mcpserver/call.go` if arg decoding is centralized
- Test: `internal/mcpserver/*_test.go`

**Interfaces:**
- Produces: `send_message` accepts optional `standing` bool (default false), valid only with `to_kind=broadcast`; description explains the split (live now vs reaches future sessions until read; use for standing orders, not transient holds). No new tool.

- [ ] **Step 1: Write failing test** — MCP `send_message` with `standing:true, to_kind:broadcast` stores a standing thread; with a non-broadcast target it errors.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement (schema + plumb + description).**
- [ ] **Step 4: Run mcpserver tests.**
- [ ] **Step 5: Commit** — `mcp: standing flag on send_message (broadcast-only)`.

---

### Task 8: CLI — `--standing` on `muster send --broadcast`

**Files:**
- Modify: `internal/humancli/humancli.go` / the `cmdSend` flag set + `registry.go` synopsis + `send` help
- Test: `internal/humancli/send_broadcast_test.go`

**Interfaces:**
- Produces: `muster send --broadcast --standing "body"` (global standing); `--broadcast --standing --project <p>` (scoped standing); `--standing` without `--broadcast` is a usage error (mirrors `--project`). Multi-word unquoted body still joins and stays global unless `--project`.

- [ ] **Step 1: Write failing tests** — the three cases above.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run humancli tests.**
- [ ] **Step 5: Commit** — `cli: --standing flag on send --broadcast`.

---

### Task 9: Display — mark standing in listings (optional polish)

**Files:**
- Modify: `internal/render/renderer.go`, `internal/station/*` — tag a standing broadcast (e.g. `broadcast (standing)` / a column marker)
- Test: `internal/render/renderer_test.go`, `internal/station/*_test.go`

- [ ] **Step 1: Write failing tests.**
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run tests.**
- [ ] **Step 5: Commit** — `render/station: surface standing broadcasts`.

---

### Task 10: Gate and end-to-end sanity

- [ ] **Step 1: Run the full gate** — `just verify` (gofmt, golangci-lint, `go test -race ./...`, build). `CGO_ENABLED=0 go build ./...`.
- [ ] **Step 2: End-to-end smoke on an isolated bus** (`MUSTER_HOME=$(mktemp -d)`): register agent A; `muster send --broadcast "live hold"`; `muster send --broadcast --standing "standing order"`; register a NEW agent B; assert B's inbox has ONLY the standing order (not the live hold); `muster inbox B` marks read; re-check unread 0.
- [ ] **Step 3: Commit any gate fixes; final commit if dirty.** Then finish the branch per the finishing-a-development-branch skill (push + open PR against `dev` — the repo's default working base).
