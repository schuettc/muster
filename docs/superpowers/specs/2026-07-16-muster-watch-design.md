# muster watch — bus journal + live tail (design)

Date: 2026-07-16 · Status: reviewed (muster-2 adversarial pass, thread 19 —
all 16 findings folded in or explicitly decided), pending user approval
Local-only spec (docs/ is untracked in the public repo).

## Problem

The bus has no live observability surface. The 2026-07-16 originator-blindness
incident took an hour of SQLite + transcript forensics to reconstruct a
timeline the operator should have been able to *watch*. v0.4.x added an events
table, but it records only the wake layer (mailbox notifies and inbox reads),
not the bus traffic itself — sends, replies, task claims and transitions. The
incident needed both halves interleaved.

## Goal

1. **The events table becomes the full bus journal**: every bus action is one
   append-only row, written by the daemon at dispatch time.
2. **`muster watch` is a plain streaming tail of that journal** — one line per
   event, filters, runs until Ctrl-C. No TUI: that is the next milestone, and
   it consumes exactly what this slice builds (journal + incremental fetch +
   filters).

## Non-goals

- No TUI / curses / screen redraws (v3 milestone).
- No streaming protocol change: the daemon keeps its one-request/one-response
  newline-JSON protocol; watch polls.
- No `inbox --wait` for agents (separate idea; the Stop-hook drain covers
  agents today).

## Design

### 1. Journal schema (store)

`events` gains one column (additive migration — v0.4.x DBs already have the
table):

```sql
ALTER TABLE events ADD COLUMN target TEXT NOT NULL DEFAULT ''
```

Event kinds (existing + new):

| kind         | agent (actor)     | target                      | thread_id | count  | detail                  |
|--------------|-------------------|-----------------------------|-----------|--------|-------------------------|
| `send`       | from-agent        | `agent:x` / `role:r` / `broadcast` | id  | 0      | subject                 |
| `reply`      | from-agent        | ''                          | id        | 0      | ''                      |
| `task`       | from-agent        | `agent:x` / `role:r` / `broadcast` | id  | 0      | subject                 |
| `claim`      | claiming agent    | ''                          | id        | 0      | ''                      |
| `transition` | acting agent      | ''                          | id        | 0      | new status              |
| `nudge`      | operator (`''`)   | nudged alias                | 0         | 0      | `typed` / `submitted`   |
| `notify`     | notified agent    | ''                          | id        | unread | `lit`/`cleared`/`skipped: …`/`error: …` |
| `read`       | reading agent     | ''                          | 0         | 0      | ''                      |

Store API changes:

- `AppendEvent` unchanged (Event struct gains `Target string`; `eventRow` in
  the CLI gains `ID` and `Target` — watch needs the ID for its cursor).
- `RecentEvents(agent string, limit int)` becomes
  `Events(q EventQuery) ([]Event, error)` with
  `EventQuery{Agent, Kind string, ThreadID, AfterID int64, Limit int,
  Backlog bool}` — mode is **explicit** (`Backlog`), never inferred from
  `AfterID == 0`:
  - Backlog mode: newest-first, `LIMIT` rows (`Limit == 0` returns none —
    watch uses that to learn the cursor without printing history).
  - Follow mode: `id > AfterID`, **oldest-first**.
  - The **agent filter** matches events the alias is concerned in, exactly
    (no LIKE — aliases must never become SQL patterns):
    `agent = ?` OR `target = 'agent:'||?` OR `target = ?` (nudge's bare
    alias) OR, for events with `thread_id > 0`, the event's thread satisfies
    `threadConcerns` for the alias — this is what makes replies/claims/
    transitions on *your* threads match even though their `target` is empty.
    Role matching uses the alias's *current* role (same semantics as
    `threadConcerns` everywhere else); historical-role drift is accepted and
    documented.
  - Validation: `Limit` clamped to 1000; negative `AfterID`/`ThreadID`
    rejected; an unknown `Kind` just matches nothing.
- `Events()` LEFT JOINs `threads` to attach each event's thread **subject**
  (empty for thread-less events) — dogfooding showed a bare thread id makes
  the journal unreadable without a second lookup ("what are those two
  messages?"). The wire event gains a `subject` field.
- `MaxEventID() (int64, error)` — the journal high-water mark.
- `PruneEvents(olderThanMillis int64) (int64, error)`: `DELETE WHERE ts <
  cutoff` (a row exactly at the cutoff survives; boundary tested).

### 2. Daemon journaling

Each dispatch handler appends its journal row **immediately after the
successful store mutation and before `notifyForThread`**, so a bus action
always precedes the wake rows it causes. Journaling stays best-effort
(`logEvent`): an append failure never fails or blocks the op. Consequence,
stated plainly: the journal is an operator/forensics feed with append-order
IDs, not a transactional audit log — a crash between mutation and append can
drop a row, and concurrent handlers can interleave. Acceptable for v1; a
transactional outbox is a deliberate non-goal.

- `send_message` → `send`, `task_create` → `task`, `reply` → `reply`,
  `task_claim` → `claim`, `task_transition` → `transition`.
- **`task_claim` also gains `notifyForThread`** — today it's the one mutation
  that wakes nobody (an originator never learns their task was claimed).
  Journaling all activity makes that asymmetry visible; fix it deliberately
  rather than inherit it.
- New op `log_event`: lets a client report an action the daemon cannot see.
  The daemon **constructs the canonical event itself** — the client supplies
  only `target` (must be a registered alias; verified) and `detail`
  (`typed` | `submitted`, validated); kind is forced to `nudge`, agent to
  `""`, thread/count to 0. Note the unix socket has no authenticated caller
  identity — this is a constrained client assertion, not provenance.
- `list_events` op args: `agent`, `kind`, `thread_id`, `after_id` (sent as a
  **decimal string** — the proto's float64 numbers lose integer precision
  past 2^53), `limit`, `backlog`. The response is `{events, max_id}` —
  `max_id` is the journal high-water mark at query time, returned in **every**
  response so a caller always has a cursor even when zero rows match.
- New op `prune_events` (`older_than_ms` > 0, validated) — gc's path to
  retention; the CLI has no direct store access.

`muster nudge` reports itself via `log_event` after typing. The daemon still
never types into panes.

### 3. `muster watch` (CLI)

```
muster watch [--agent <alias>] [--thread <id>] [--kind <k>] \
             [--interval <dur>] [--backlog <n>]
```

- Prints the last `--backlog` (default **10**; `0` = follow only, no history)
  matching events, then follows: polls `list_events` with `after_id = cursor`
  every `--interval` (default **1s**), printing one line per event until
  Ctrl-C / SIGTERM.
- **Cursor discipline:** the cursor starts at the backlog response's
  `max_id` (never inferred from printed-slice position, which is fragile
  across the newest-first/oldest-first flip) and advances to each response's
  `max_id`. If a response's `max_id` is *lower* than the cursor, the DB was
  replaced under a respawned daemon — watch resets the cursor to the new
  `max_id` and says so on stderr rather than silently going quiet forever.
- **Errors and shutdown:** a failed poll (daemon restarting, socket blip)
  retries on the next interval with a one-line stderr note — a live tail
  must survive a daemon respawn, not die mid-incident. The wait is
  signal-aware (select on signal channel + timer), so Ctrl-C is immediate,
  never delayed up to an interval.
- Line format matches `muster events`: TIME KIND AGENT TARGET THREAD COUNT
  DETAIL SUBJECT — subject joined from the thread so a row like
  `notify muster-2 19 2 lit` reads as *what* lit, not just that something
  did. Tabs/newlines in subjects and details are replaced with spaces at
  render time so one event is always exactly one line; subject truncated to
  keep lines sane (60 chars, render-side).
- `muster events` and `watch` are **side-effect-free observers**: they never
  mark anything read and never touch mailboxes. (Contrast: `muster inbox
  <alias>` runs the real `get_inbox`, which marks the agent's inbox read and
  clears its 📬 — an operator peeking at an agent's inbox consumes its flag.
  A `--peek` mode for inbox is noted as a follow-up, out of scope here; watch
  removes most reasons to peek.)
- The poll loop takes an injectable wait func and an optional max-iterations
  bound used by tests.
- `muster events` gains the same `--kind` / `--thread` filters for parity
  (one-shot view over the same query). Both subcommands appear in the usage
  line (routing already flows through `humancli.Dispatch` — v0.4.1).

### 4. Retention

`muster gc` prunes journal rows older than `--events-keep` (a Go duration,
default **720h** = 30 days; Go's duration syntax has no "d" unit) via the
`prune_events` op, reporting the pruned count. `--events-keep <= 0` is
rejected ("delete everything" must not be a typo away). gc reaps dead agents
first, then prunes; each step reports its own result or error so a failure
in one never masquerades as success in the other. Nothing else expires.

### 5. Testing

- **store**: journal row per kind; follow ordering (oldest-first) vs backlog
  (newest-first) with `Limit > 1` under interleaved inserts; agent filter
  matches actor, `agent:` target, bare-alias target, AND thread-concern
  (reply on an originated thread with empty target — the finding-1 case);
  migration: a **hand-built v0.4 schema with data** (not one created by
  current `Open`) gains `target` with `''` defaults, reopened twice;
  `MaxEventID`; prune exact-boundary (`ts == cutoff` survives); limit clamp
  and negative-arg rejection.
- **daemon**: each of the five ops journals the right row **in order**
  (action row before its notify rows — assert interleaving, not just
  presence); claim now notifies; `log_event` forces canonical fields,
  rejects unregistered targets and non-nudge kinds; `list_events` honors
  after_id (as string)/kind/thread/backlog and always returns `max_id`;
  `prune_events` validates.
- **humancli**: watch with injectable wait + iteration bound — backlog then
  follow, `--backlog 0`, cursor-regression reset, poll-error retry, filters;
  events filter flags; gc `--events-keep` validation; one-line rendering of
  a subject containing a newline.
- Full `just verify` gate; VERSION bump `0.5.0` on ship.

## Sequencing (implementation slices)

1. Store: `target` column + `EventQuery`/`Events()` + `PruneEvents` + tests.
2. Daemon: journal appends in the five handlers + `log_event` + richer
   `list_events` + tests.
3. CLI: `watch` + `events` filters + nudge self-report + `gc --events-keep` +
   tests.
4. Docs: README (watch/events/gc), site tools section touch. Release 0.5.0.

## Risks / notes

- Journal writes add one INSERT per op on the daemon's single connection —
  negligible at this scale; best-effort so a journaling failure never blocks
  the bus.
- Polling latency ≤ interval; acceptable for a human tail. The TUI can revisit
  transport later if it ever needs sub-second push.
- `log_event` is the first client-asserted journal row; constraining v1 to
  `nudge` keeps the journal trustworthy as forensics.
