# Wake discipline — implementation plan

> **For agentic workers:** superpowers:subagent-driven-development / executing-plans. Checkbox steps.

**Goal:** Make the channel Carrier decide *whether to actively push* (interrupt) vs leave a message for the badge + next turn, per the delivery policy — so broadcasts stop herding/storming while direct messages stay fast, with an explicit `--wake` break-glass.

**Architecture:** All discipline lives in the Carrier's `shouldPush`. The badge (`notifyForThread`) is unchanged and stays the truthful unread. Two query-time joins ride on each event — `Origin` (thread from_agent) and `Wake` (a new thread column, break-glass). Both stores change together, gated by conformance. Spec: `docs/superpowers/specs/2026-09-01-wake-discipline-design.md`.

## Global Constraints

- Worktree `/Users/courtschuett/GitHub/worktrees/muster-wake-discipline`, branch `feat/wake-discipline` (off dev). Never touch the primary clone.
- Gate: `just verify` + `just verify-dynamo`.
- Both stores or neither; conformance is the gate.
- Do NOT change `notifyForThread`/badge semantics or the Stop-hook. The badge must keep reflecting true unread for every recipient — this change only gates the *active push*.
- The cursor advances past non-pushed events (badge + Stop-hook carry them); never re-push.

---

### Task 1: Store + DynamoDB — `wake` column and `origin`/`wake` on events

**Files:** `internal/store/schema.sql`, `internal/store/store.go`, `internal/store/models.go`, `internal/store/threads.go` (CreateThread + Events join), `internal/store/events.go`, `internal/dynamostore/{threads,events}.go`, item builders.

- [ ] **Step 1 (tests):** `Events` returns `Origin`(thread from_agent) + `Wake`(thread.wake) for a threaded event; thread-less event zeroes both; `wake` round-trips CreateThread→GetThread. Schema-column presence test.
- [ ] **Step 2:** verify fail.
- [ ] **Step 3:** add `threads.wake` column (CREATE TABLE + ALTER migration); `Thread.Wake bool`; `Event.Origin string` + `Event.Wake bool`; CreateThread persists `wake`; `Events` query joins `threads.from_agent AS origin, threads.wake AS wake` (SQLite) and the DynamoDB equivalent; item builder + itemToThread carry `wake`.
- [ ] **Step 4:** store tests + `just verify-dynamo` (dynamo half can come with Task 2's conformance run).
- [ ] **Step 5:** commit `store+dynamostore: wake column + origin/wake on events`.

---

### Task 2: Conformance — origin/wake on events, both stores

**Files:** `internal/storetest/conformance.go`.

- [ ] Cases: a reply event on a broadcast carries `Origin`=opener's alias; a `wake` broadcast's opening event carries `Wake=true`; a thread-less event (e.g. nudge/log) zeroes both; `wake` false by default.
- [ ] Run both backends; commit `storetest: origin/wake event conformance`.

---

### Task 3: Daemon + MCP + CLI — accept `wake`

**Files:** `internal/daemon/daemon.go` (send_message `wake`), `internal/mcpserver/tools_messages.go` (send_message `wake` field + desc), `internal/humancli/humancli.go` (`--wake` flag + `--wake` requires `--broadcast`; add to sendBoolFlags), `registry.go` synopsis.

- [ ] Tests: daemon send_message with `wake` stores `Wake=true`; MCP passes it; CLI `--broadcast --wake` sends wake, `--wake` alone errors.
- [ ] Commit `daemon+mcp+cli: --wake break-glass on broadcast send`.

---

### Task 4: Carrier — `shouldPush` delivery policy (the core)

**Files:** `internal/channel/envelope.go` or a new `internal/channel/deliver.go` (the pure `shouldPush`), `internal/channel/carrier.go` (apply in `Tick`), `internal/channel/*` Event struct (add Origin, Wake decode).

**Interface:**
```
shouldPush(e Event, mine map[string]bool) bool:
    if e.Kind == "send" && e.Wake { return true }
    directed := mine[e.Target] || (e.Kind == "reply" && mine[e.Origin])
    return directed && e.Intent != "fyi"
```
- Carrier `Event` gains `Origin`/`Wake` (json from list_events).
- In `Tick`, after building the concerning/non-self `batch`, filter to `pushable := [e for e in batch if shouldPush(e, mine)]`; push only `pushable` (still advance cursor over the whole batch). No pushable → no push.

- [ ] **Step 1 (tests):** table-driven `shouldPush` covering every row of the policy table (direct non-fyi push; direct fyi hold; broadcast opener hold; broadcast reply as audience hold; broadcast reply as originator non-fyi push; role opener hold; break-glass opener push regardless of intent). Plus a Carrier `Tick` test: a mixed batch pushes only the pushable subset and advances the cursor past all.
- [ ] **Step 2:** verify fail.
- [ ] **Step 3:** implement.
- [ ] **Step 4:** `go test ./internal/channel/...`.
- [ ] **Step 5:** commit `channel: shouldPush delivery policy (polite broadcasts, fast direct, break-glass)`.

---

### Task 5: Gate + end-to-end + PR

- [ ] `just verify` + `just verify-dynamo` green.
- [ ] E2E on an isolated bus with two live sessions (fake carriers/or a scripted check): a plain broadcast lights both badges but pushes to neither; a direct non-fyi message pushes to its recipient; a reply on a broadcast pushes only to the originator's carrier; a `--wake` broadcast pushes to all. (If full two-session harness is impractical, assert via `shouldPush` + carrier Tick unit coverage and a daemon-level event-join check.)
- [ ] finishing-a-development-branch: push + open PR against `dev`. (Note: pre-push hook needs the pusher's muster inbox drained — known hermeticity bug.)
