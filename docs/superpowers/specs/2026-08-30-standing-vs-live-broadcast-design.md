# Standing vs live broadcast

**Date:** 2026-08-30
**Status:** Draft

## Problem

Broadcast delivery is pull-by-watermark, and a brand-new session starts with
its watermark at zero — so it inherits the **entire broadcast backlog** on
first registration.

The mechanism: a broadcast is a thread with `to_kind='broadcast'` (see
[project-broadcast design](2026-07-22-project-broadcast-design.md)). An agent
"concerns" it via the one canonical `threadConcerns` predicate, and whether it
is *unread* is decided per-agent against a read watermark
(`internal/store/threads.go`):

```sql
e.id > COALESCE((SELECT last_read_entry_id FROM agents WHERE alias=?), 0)
```

`RegisterAgent`'s INSERT branch (`internal/store/agents.go`) never sets
`last_read_entry_id`, so a first-seen alias defaults to `0`. Every historical
broadcast entry has `id > 0`, so all of them land in the new session's inbox.
A *returning* alias keeps its watermark and correctly sees only what is new.

This conflates two intents the operator actually has:

| Intent | Wanted | Today |
|---|---|---|
| "Welcome / standing orders" — read this before you touch the repo | reaches sessions now **and** future ones | ✅ (the only behavior) |
| "Hold while I refactor X" — transient, meaningless once the hold clears | reaches **only live** sessions; invisible to future ones | ❌ (does not exist) |

Every broadcast today behaves like the first row. The transient case has no
expression, and there is no way to stop the backlog from piling onto new
sessions.

## Decision

Two changes, together:

1. **New sessions start caught-up.** `RegisterAgent`'s INSERT branch seeds
   `last_read_entry_id = MAX(entries.id)` at registration time. A first-seen
   alias sees nothing sent before it existed — only broadcasts (and direct
   mail) that arrive after it joins. Returning aliases (the ON CONFLICT
   branch) are untouched, exactly as today. This makes plain broadcast
   **live-only** by default and fixes the backlog complaint outright.

2. **`--standing` broadcasts are exempt from that seed.** A standing broadcast
   is delivered to every newly-registered session on start — even one that
   joins long after the send — until that session reads it once. This is the
   "welcome to the project" channel, preserved from the behavior step 1 would
   otherwise remove.

Standing is tracked with a **second watermark**, not a new per-message "seen"
table: add `last_read_standing_entry_id` to the `agents` row, defaulting to
`0` and — unlike `last_read_entry_id` — **not** seeded on register. The unread
computation picks the watermark per thread:

- standing broadcast entry is unread when `e.id > last_read_standing_entry_id`
  (0 for a new session → the standing backlog shows once),
- every other entry is unread when `e.id > last_read_entry_id`
  (seeded to `MAX` on register → the ordinary backlog is skipped).

`MarkRead` advances **both** watermarks to the max entry read, so a standing
message badges a session once on start and then goes quiet, and a later
standing broadcast (higher id) badges again.

Standing is orthogonal to project scope: `--standing` composes with the
existing global and `--project`-scoped forms.

### Why a second watermark, not a `standing_seen(alias, thread_id)` set

The set is more precise (per-message dismissal) but heavier: a new table, a
join in every unread surface, and a bespoke set representation to model and
prove equal in DynamoDB. The second watermark reuses the existing watermark
machinery verbatim — one scalar column, the same `e.id > watermark` shape, the
same `MarkRead` update — which is the only way to keep the SQLite and DynamoDB
stores in lockstep cheaply (see conformance note below). Per-message dismissal
is not a requirement here; "badge each new session once, then quiet" is.

## Semantics

- **Plain broadcast (`--broadcast`)** — live-only. Delivered to every session
  live at (or after) send time via the existing wake fan-out and inbox concern;
  a session that registers *later* never sees it, because its seeded watermark
  is already past the entry.
- **Standing broadcast (`--broadcast --standing`)** — delivered to live
  sessions now (identical wake fan-out) **and** to any session that registers
  later, once, until it reads. Both the global and `--project` scoped forms
  support `--standing`.
- **Send-time wake is unchanged.** `notifyForThread` fans out to concerning
  live agents identically for standing and live broadcasts — standing only
  changes what a *not-yet-registered* session finds in its inbox on start. No
  daemon fan-out change is required.
- **Concern is unchanged.** `threadConcerns` / `threadConcernsJoin` are not
  touched; a standing broadcast concerns the same set (global, or the scoped
  project). Only the *unread* computation branches on the standing flag.
- **Standing is message-only.** `--standing` requires `--broadcast`; both
  without the pairing are usage errors. A standing flag on a task broadcast is
  rejected (standing tasks are out of scope — a task's lifecycle already gives
  it persistence and an owner).
- **Direct and role mail are unaffected** by the register-time seed in
  practice: a first-seen alias has no prior direct/role mail by definition, so
  seeding the watermark to `MAX` cannot hide anything addressed to it. The seed
  only ever suppresses history that predates the session — which for a new
  alias is exactly the broadcast backlog.

## Migration

On upgrade, pre-existing broadcast rows carry no standing flag (column
defaults to 0), so they become **live-only for future sessions**: a session
that starts after the upgrade will not replay them. There is no way to know
retroactively which historical broadcasts were meant as standing orders, so
this is accepted — re-send anything that must persist with `--standing`.
Sessions already registered keep their existing watermark and are unaffected.

The new `last_read_standing_entry_id` column is added with default `0` for all
existing agent rows. Existing sessions therefore see standing broadcasts sent
*after* the upgrade replay once (harmless, and correct — those are genuinely
new standing orders). No historical standing broadcasts exist to replay.

## Changes by component

### Store (`internal/store`)

- **Schema:** `agents` gains `last_read_standing_entry_id INTEGER NOT NULL
  DEFAULT 0`; `threads` gains `standing INTEGER NOT NULL DEFAULT 0` (a
  migration in the store's schema setup).
- **`RegisterAgent` (`agents.go`):** the INSERT branch seeds
  `last_read_entry_id = (SELECT COALESCE(MAX(id),0) FROM entries)`. The ON
  CONFLICT branch is unchanged and must **not** touch either watermark, so a
  returning alias keeps its read-state (the invariant the existing doc comment
  and revive tests already guard). `last_read_standing_entry_id` is left at its
  `0` default on INSERT — never seeded.
- **Unread predicate:** every surface that counts unread entries branches on
  the thread's `standing` flag:

  ```sql
  (recent.standing = 1 AND e.id > COALESCE(last_read_standing_entry_id, 0))
  OR
  (recent.standing = 0 AND e.id > COALESCE(last_read_entry_id, 0))
  ```

  applied in `Inbox()` (`threads.go`), `UnreadCount`, and `SessionUnread`
  (`poll.go` / the session-join form). The `from_agent != alias` self-exclusion
  is retained in all of them.
- **`MarkRead` (`agents.go`):** set both `last_read_entry_id` and
  `last_read_standing_entry_id` to the snapshot max entry id in the same
  transaction that reads the count.
- **`CreateThread`:** accept and persist the `standing` flag on broadcast
  threads; reject `standing=1` for any `to_kind != 'broadcast'` (defense in
  depth behind the daemon check).

### DynamoDB store (`internal/dynamostore`) + conformance

The hosted backend is a full peer of the SQLite store and
`internal/storetest/conformance.go` is the gate that forces them to behave
identically. Every store change above lands in `dynamostore` too: the agent
item carries `last_read_standing_entry_id`, the thread item carries `standing`,
register seeds the live watermark to the current max event id, `MarkRead`
advances both, and the unread/inbox projections branch on `standing`. New
conformance cases (below) run against **both** stores and are the definition of
"done" for this layer.

### Daemon (`internal/daemon/daemon.go`)

- **Validation:** in the `send_message` op handler, reject `standing=1` unless
  `to_kind=='broadcast'`, with a clear usage error. Standing composes with the
  existing project-scope validation (a standing project broadcast is still
  rejected if no non-departed agent is in that project).
- **`notifyForThread`:** unchanged. Standing does not alter live wake fan-out.
- **`targetOf` / journal:** no change — standing is a property of the thread,
  not the target string; `broadcast` / `broadcast:<project>` render as today.
  (Station/render surfacing of the standing flag is optional polish, below.)

### MCP (`internal/mcpserver`)

`send_message` gains an optional `standing` boolean (default false), valid only
with `to_kind=broadcast`. The tool description explains the split: "a plain
broadcast reaches every session live now; set `standing:true` to also reach
sessions that start later, until they read it once — use it for standing orders
like 'read CONTRACT.md before editing', not for transient holds." No new tool.

### CLI (`internal/humancli`)

- `muster send --broadcast --standing "body"` — standing global broadcast.
- `muster send --broadcast --standing --project <p> "body"` — standing scoped.
- `muster send --standing …` without `--broadcast` is a usage error, mirroring
  `--project`. Help text and the `registry.go` synopsis document the flag and
  when to reach for it.

### Render / station (optional polish)

`internal/render` and `internal/station` may mark a standing broadcast in
listings (e.g. a `standing` tag beside `broadcast` / `broadcast:<project>`) so
an operator can tell a standing thread from a live one at a glance. Not
required for the feature to work; can follow.

## Testing

- **Store / conformance (both backends):**
  - a first-seen alias registers with `last_read_entry_id = MAX(entries.id)`;
    a broadcast sent *before* it registered is **not** in its inbox.
  - a returning (departed→revived) alias keeps its prior watermark — the
    existing revive test still passes.
  - a **standing** broadcast sent before a new alias registers **is** in its
    inbox once; after `MarkRead` it is gone; a later standing broadcast (higher
    id) reappears.
  - a plain (non-standing) broadcast sent before registration stays hidden;
    one sent after registration appears — the live-only contract.
  - standing composes with `--project`: a new same-project alias sees a
    standing scoped broadcast; an other-project alias never does.
  - `MarkRead` advances both watermarks; unread count is zero immediately after
    for both standing and live threads.
  - `TestThreadConcernsSessionJoinEquivalence` still passes (concern predicate
    is unchanged); add standing thread shapes to the unread-branch fixtures.
- **Daemon:** `standing=1` with a non-broadcast target is rejected; standing
  scoped broadcast to an unknown/departed-only project is rejected with the
  known-projects error; live wake fan-out badges the same live sessions for a
  standing broadcast as for a plain one.
- **CLI:** `--broadcast --standing` sends a standing broadcast; `--standing`
  without `--broadcast` is a usage error; a multi-word unquoted body with
  `--standing` still joins into the body and stays global unless `--project`.

## Out of scope

- Per-message dismissal / a `standing_seen` set (the second watermark is
  sufficient for "badge each new session once").
- Standing **tasks** — standing is message-broadcast-only.
- Broadcast TTL/expiry and an operator "clear all broadcasts" command
  (considered; weaker — a stale-but-unexpired hold still hits new sessions, and
  a manual clear is easy to forget). The register-time seed makes both
  unnecessary for the stated problem.
- Retroactive classification of pre-upgrade broadcasts as standing (see
  Migration).
- Editing/withdrawing a standing broadcast after send (re-send or let read
  quiet it; withdrawal can follow if needed).
