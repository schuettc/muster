# Standing orders — a keyed, retractable convention

**Date:** 2026-08-31
**Status:** Draft (v2 follow-up to [standing-vs-live-broadcast](2026-08-30-standing-vs-live-broadcast-design.md))

## Problem

[Standing-vs-live broadcast](2026-08-30-standing-vs-live-broadcast-design.md)
(shipped, PR #124) gave broadcast a durable arm: `muster send --broadcast
--standing` reaches sessions that start later, once, until they read. That is
the right **transport**, but it is the wrong **shape** for the standardization
program's actual need, which `tool-standards` stated precisely (thread 346):

- A project's standing orders are **one durable statement per project**
  (its invariants), authored at project setup — not a growing pile of threads.
- A standing broadcast is **append-only**: "update the invariants" means send a
  new one, but the old one still greets never-seen sessions until they read it,
  and there is **no retract at all**. Stale orders accumulate and rot — the same
  failure mode as the four copied `release.yml` blocks the family is trying to
  kill.
- Standing orders that cannot be **listed** are write-only and drift. The
  onboarding skill must both SET a project's orders and VERIFY they are present
  and current — both need a machine-readable listing.

So append-only standing broadcasts cannot be the convention surface. This spec
adds a dedicated, keyed, retractable concept on top of the shipped machinery.

## Decision

A **standing order** is a broadcast thread carrying a `(project, key)` identity
and a lifecycle, addressed and delivered exactly like a standing broadcast — so
it inherits the shipped standing-watermark delivery unchanged — but managed by
a dedicated verb family that **supersedes by key** instead of appending:

```
muster standing set <project> [--key <k>] "body"   # idempotent create-or-replace
muster standing retract <project> [--key <k>]       # stop greeting new sessions
muster standing <project> [--json]                  # list what greets a new session
```

`--key` defaults to `invariants` (the one-order-per-project common case; a
project may hold several orders under distinct keys). `set` is idempotent by
`(project, key)`: it retracts any prior live order under that key in the same
transaction and creates the replacement, so a re-run with unchanged text
converges and a re-run with new text replaces. `retract` marks the current
order under `(project, key)` retracted; a retracted order greets no future
session and drops out of `list`.

This is a distinct durable concept, **not** annotated broadcasts and not a new
mechanism: the delivery path, the register-time seed, and the standing
watermark are all reused verbatim. What is new is identity (`project` + `key`)
and lifecycle (supersede / retract) on top of a standing broadcast thread.

### Why reuse standing broadcast threads rather than a new table

The shipped standing broadcast already delivers "reaches future sessions once,
until read" correctly across both stores, gated by conformance. A separate
`standing_orders` table would re-derive that delivery — including the
register-time seed, the two-store parity, and the watermark math — for no new
capability. A standing order **is** a standing broadcast with an identity and a
lifecycle; modelling it as one keeps a single delivery path and a single set of
conformance guarantees. (`tool-standards` confirmed this shape in thread 346.)

## Data model

Two additive columns on `threads`, both broadcast-only and both defaulting to
the current append-only behavior so every existing standing broadcast is
unaffected:

- `standing_key TEXT NOT NULL DEFAULT ''` — the order's key within its project.
  Empty on every ordinary standing broadcast (an un-keyed, append-only standing
  message stays exactly as shipped). Non-empty only on orders created by
  `standing set`.
- `standing_retracted INTEGER NOT NULL DEFAULT 0` — 1 once retracted (by
  `retract`, or by `set` superseding the prior order under the same key).

The order's `to_target` is the project scope (reusing the existing scoped
broadcast machinery); `standing = 1` as today. Identity is
`(to_target, standing_key)` among live (`standing_retracted = 0`) standing
broadcast rows.

Both stores (`internal/store` + `internal/dynamostore`) carry the columns;
conformance asserts the lifecycle below on both. Migration is additive with the
defaults above — no backfill.

## Semantics

- **Delivery is unchanged.** A live keyed standing order is a standing broadcast:
  a session that registers later gets it once, until read, via the standing
  watermark. `retract` (setting `standing_retracted = 1`) removes it from the
  concern/unread computation for **future** sessions — the retracted row stops
  greeting new sessions. A session that already read it is unaffected either way.
- **`set` is idempotent by `(project, key)`**, in one transaction: retract the
  prior live order under that key (if any), then create the replacement standing
  broadcast. Convergent on re-run; a text change replaces rather than stacks.
- **`set` RE-GREETS already-registered sessions with the updated order**, not
  only future ones — the property that makes this a "current invariants"
  convention rather than a set-once greeting. The replacement is a new standing
  entry with an id above the retracted one, so a session that already read the
  prior order has its standing watermark below the new id and the updated order
  surfaces as unread on its next inbox check — and only the updated order (the
  retracted prior is filtered). This is what lets a project fix a WRONG
  invariant mid-flight and have every running session get the correction.
  (Asserted by `StandingOrderSetReGreetsAnAlreadyReadSession` on both stores.)
- **`retract` is idempotent**: retracting an absent or already-retracted order is
  a no-op success, so an onboarding/audit skill can retract without first checking.
- **`list`** returns the live (non-retracted) standing orders for a project —
  `(key, body, from, created_at)` — sorted by key. `--json` is the audit/verify
  seam the onboarding skill reads. A retracted order never appears.
- **Scope validation** reuses scoped-broadcast validation: `set`/`retract` on a
  project with no non-departed agents is rejected with the known-projects error,
  exactly as `send --broadcast --project`.
- **The un-keyed `send --broadcast --standing` stays** as the ad-hoc primitive
  (empty `standing_key`, append-only) — it is the low-level transport; the keyed
  verb family is the durable convention layered on it. `list` ignores un-keyed
  standing broadcasts (they are messages, not managed orders).

## Retraction and the read watermark — the one subtlety

Retract must stop greeting **future** sessions and must not resurrect as unread
for anyone. Because unread is judged by the standing watermark, a retracted
order simply must be excluded from the concern/unread predicate when
`standing_retracted = 1` — a one-clause addition to the standing branch, applied
identically in `Inbox`/`UnreadCount`/`SessionUnread` and their DynamoDB
counterparts. No watermark is moved on retract; the row is filtered, not
rewritten, so a session that had already read it sees no change and a session
that never saw it never will.

## Surfaces

- **Store:** the two columns; `SetStandingOrder(project, key, from, body)` (the
  transactional retract-prior-then-create), `RetractStandingOrder(project, key)`,
  `ListStandingOrders(project) []StandingOrder`. The unread predicate gains the
  `standing_retracted = 0` guard on its standing branch.
- **DynamoDB store:** mirror of all three, plus the retracted guard in the
  unread branch; conformance cases below run on both.
- **Daemon:** `standing_set`, `standing_retract`, `standing_list` ops, each
  reusing scoped-broadcast validation. `set`/`retract` journal an event so the
  bus tail shows the lifecycle.
- **MCP:** `standing_set` / `standing_retract` / `standing_list` tools (the
  onboarding skill drives these), described as the durable per-project
  convention, distinct from ad-hoc `send_message` broadcasts.
- **CLI:** the `muster standing` command group above.
- **Conformance (both stores):** `set` creates a listable order; a second `set`
  under the same key replaces (list shows one, the new body; a newly-registered
  session sees only the new one); `retract` drops it from `list` and from a new
  session's unread; retract is idempotent; scope validation rejects unknown
  projects; an un-keyed standing broadcast never appears in `list`.

## Onboarding-skill contract (informational)

`tool-standards` will add a "set the project standing orders" step to the
`tools-family-onboarding` skill: at project setup, `standing set <project> --key
invariants "<golden rules>"`; on audit, `standing <project> --json` to verify
presence and currency. The initial `.tools` payload (from thread 346): shared
clone — git fetch+rebase before any push; coordinate before touching a
load-bearing tool (branch target + collisions); HUMAN-ONLY guardrail-blocked
steps (gh secret set / editing a CI workflow to add role-assumption / curl|sh);
read `tools-ops/docs/superpowers/specs/2026-08-30-family-tools-standard.md` + the
onboarding skill.

## Out of scope

- History/versioning of superseded orders (retract is a tombstone, not a log).
- Per-key delivery ordering or priority.
- Cross-project / global standing orders (a standing order is per-project by
  construction; the global un-keyed `send --broadcast --standing` covers the
  rare global case).
- Editing an order's key in place (retract + set under the new key).
