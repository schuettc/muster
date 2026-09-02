# Wake discipline — polite broadcasts, fast direct, explicit break-glass

**Date:** 2026-09-01
**Status:** Draft

## Problem

A broadcast to N live sessions woke all N at once: `notifyForThread` badges every
recipient, and each session's channel **Carrier** (`internal/channel`) then
actively *pushes* the mail into the session, starting a turn — so N sessions
fire an upstream model request in the same instant. On the shared upstream
account that is a self-inflicted thundering herd: rate-limit failures across
every session.

Worse, it compounds. Every entry runs the same fan-out, including **replies**.
A broadcast thread concerns its whole audience, so each session's *ack* re-wakes
all N sessions, and each of those may ack again — O(N²). The 2026-09-01
standing-order broadcast (thread 353) did exactly this and rate-limited the
whole machine.

The active wake is too blunt: it treats a one-to-many announcement and a
one-to-one message identically, and it re-fans-out replies to an audience that
only ever needed to hear the *opener*.

## Two wake sinks — only one is the problem

- **Badge** (`notifyForThread` → `setSessionBadge`): passive. Sets the `📬<n>`
  count on the tmux session from the *true* unread. Drained on the session's
  next Stop-hook turn. Cheap; starts no turn. **Unchanged by this spec.**
- **Carrier push** (`internal/channel`): active. Tails the journal for a
  session's aliases (`list_events`) and *pushes* an envelope into the running
  session — this is what starts a turn and fires the upstream request. **All
  the discipline goes here.**

Because the badge always reflects true unread and the Stop-hook always drains
it, suppressing an active push never loses a message — it only declines to
*interrupt*. That is the whole lever.

## Decision — the delivery policy

The Carrier decides, per event it would otherwise push, whether to actively
push (fast) or leave it for the badge + next turn (polite):

| Case | Delivery |
|---|---|
| Direct message addressed to me, intent ≠ fyi | **push** (fast — direct default) |
| Reply on a thread **I originated**, intent ≠ fyi | **push** (the response returns to me fast) |
| Any **fyi** (direct / role / broadcast) | badge only, next turn |
| Broadcast / role **opening send** where I'm audience | badge only, next turn |
| Opening send tagged **break-glass** (`wake`) | **push** — overrides all, to every recipient |

Formally, for one candidate event and this session's alias set `mine`:

```
shouldPush(e):
    if e.Kind == "send" and e.Wake:                      # explicit break-glass opener
        return true
    directedAtMe = mine[e.Target]                        # a direct message/reply addressed to me
                   or (e.Kind == "reply" and mine[e.Origin])   # a reply on a thread I started
    return directedAtMe and e.Intent != "fyi"
```

Everything else is badge-only. Consequences, by design:

- **Broadcast opener → every audience session gets it next turn** (badge only),
  never a synchronized push. Broadcasts become the polite, sparing channel.
- **Broadcast/role reply → fast-paths ONLY the thread originator** (a reply is a
  response *back to whoever asked*); the incidental audience gets nothing active.
  This is the O(N²) ack storm reduced to at most one push (to the originator),
  and only if the reply isn't fyi.
- **Direct message → pushes by default** (fast), because it is one recipient,
  addressed to you, with no herd — the common coordination case stays snappy.
  A **fyi** direct is the polite exception (next turn).
- **Break-glass is opt-in and explicit** (a `wake` tag on the send), so a
  genuinely urgent announcement can still interrupt everyone — but it can never
  happen by accident, which is the failure mode we are closing.

## What rides on the event

The Carrier already receives each event's `Kind`, `Target` (`agent` alias /
`role:x` / `broadcast[:project]`), `Intent` (the thread's effective intent), and
`Subject`. It needs two more, both **query-time joins from the event's thread —
no new per-event storage**:

- **Origin** = `threads.from_agent` — to recognise a reply on a thread I started.
- **Wake** = `threads.wake` — the break-glass tag (a new thread column).

`Target`'s shape already distinguishes a direct message (a bare alias, matched
against `mine`) from role/broadcast (`role:`/`broadcast` prefixes), so no extra
field is needed to tell them apart.

## Changes by component

### Store + DynamoDB (`internal/store`, `internal/dynamostore`)

- **Schema:** `threads` gains `wake INTEGER NOT NULL DEFAULT 0` (broadcast
  break-glass; additive, default preserves every existing row). Migration is a
  single `ALTER TABLE ... ADD COLUMN`, no backfill; the DynamoDB item reads an
  absent attribute as 0.
- **`CreateThread`** persists `Wake` (only meaningful on a broadcast; harmless
  elsewhere and never consulted for a directed send).
- **`Events` query** joins the event's thread for `from_agent AS origin` and
  `wake`, exactly as it already joins `effectiveIntent`/`subject`. `Event` gains
  `Origin string` and `Wake bool`. Thread-less events read both as zero.
- Conformance asserts `Events` returns `Origin`/`Wake` for a threaded event on
  both stores, and that an unregistered/thread-less event zeroes them.

### Daemon (`internal/daemon`)

- `send_message` accepts an optional `wake` bool and passes it to
  `CreateThread`. No change to `notifyForThread` (the badge stays truthful).
- `standing_set` may pass `wake` too if desired later; **out of scope here** —
  standing orders are informational and should stay polite.

### Channel carrier (`internal/channel`)

- `Event` gains `Origin`/`Wake` (decoded from `list_events`).
- `Tick` filters the batch through `shouldPush`: events that should push are
  delivered in the envelope; the rest are **not pushed** (the cursor still
  advances — the badge and Stop-hook already carry them). If nothing in the
  batch should push, no envelope is sent.
- The one-envelope-per-tick batching is preserved for the pushable subset.

### MCP + CLI

- `send_message` MCP tool gains `wake bool` (broadcast-only break-glass;
  described as "interrupt every recipient now — use sparingly").
- CLI: `muster send --broadcast --wake …`. `--wake` without `--broadcast` is a
  usage error (break-glass is a broadcast concept; a direct send already pushes).

## Testing

- **Store/conformance (both backends):** `Events` returns `origin` + `wake` for a
  threaded event; thread-less/unregistered events zero them; `wake` round-trips
  through `CreateThread`/`GetThread`.
- **Carrier (`shouldPush`, the core):** direct non-fyi → push; direct fyi → hold;
  broadcast opener (fyi or not) → hold; broadcast reply where I'm audience →
  hold; broadcast reply where I'm the originator, non-fyi → push; role opener →
  hold; break-glass opener → push regardless of intent; self-authored → never
  (unchanged). A batch with a mix pushes only the pushable subset and still
  advances the cursor.
- **Daemon:** `send_message` with `wake` stores `Wake=true`; a broadcast opener
  with `wake` is the only send that fans an active push out (asserted via the
  event's `Wake` join).
- **CLI:** `--broadcast --wake` sends a wake broadcast; `--wake` without
  `--broadcast` errors.

## Out of scope

- Jitter/pacing of the break-glass fan-out (a `--wake` broadcast still pushes to
  all at once — that is the operator's explicit choice; pacing can follow).
- Harness-level upstream concurrency limiting (belongs in the pi provider guard,
  not muster).
- Changing the badge / Stop-hook drain semantics (the badge stays the truthful
  unread count for every recipient).
- Per-session quiet hours or rate caps on active pushes.
