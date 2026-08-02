# `muster become` — claim your name

**Date:** 2026-08-01
**Status:** proposed
**Depends on:** durable alias identity (v0.8.0 — register outcome/unread, harness links, ancestry capture, resume reclaim)

## Problem

v0.8.0 made aliases durable but not meaningful. The launch wrapper seeds every
session's alias from its tmux session name — the right default at launch, when
nothing meaningful exists yet — but nothing ever replaces the seed. So the
roster and all traffic read in terminal vocabulary (`muster-2`,
`bettor-help-workspace-7`) even after a conversation has become something
nameable. Routing by alias only means what it should when the alias names the
conversation, not the terminal it started in.

The operator's verdict (this is the point of the whole effort, not a polish
item): traffic should route by a name the work deserves. Retroactive
coherence explicitly does NOT matter — old threads reading as the old name is
fine ("that's what it was called then").

## Decision

A session **claims** its real name once the work has one: a new identity row
is created under the claimed alias and the seed row retires. The alias itself
stays immutable — `become` is a claim-and-retire, never a rewrite — so the
stability promise that makes aliases worth routing on (written references
never dangle; history is append-only and truthful) holds after the claim.

Explicitly rejected: **rename-that-moves-history** (rewriting alias text
across threads/entries/events). Its only unique benefit is retroactive
coherence, which the operator doesn't want; its costs are breaking the
written-reference promise (the skill's own handoffs-use-aliases rule),
falsifying the journal, and a database-wide rewrite of the one field
everything keys on.

Also rejected: piggybacking on the label. The 0.7.4 division of labor stands
and gets cleaner: **label = what the session is doing right now** (topical,
churns freely, prefix T untouched); **alias = who the conversation is**
(claimed once, then stable). Prefix T must NOT trigger become — identity must
not churn with topic. Claude's `/rename` and tmux session renames stay
display-only.

## Mechanism

### 1. Daemon op `become` (the one canonical move)

`become {from, to}` — atomic, in one transaction where SQLite allows:

1. **Guard `from`:** must exist. (Departed `from` is fine — a session may
   claim after its seed was gc-tombstoned; the clone below revives nothing,
   it copies.)
2. **Guard `to`:** must not exist AT ALL. A live `to` is someone else's
   identity; a tombstoned `to` is some other conversation's history and
   read-state, and merging identities is exactly the confusion this feature
   kills. Fail loudly with a hint (`alias 'x' already has history; pick
   another name, or purge it with muster gc --purge-agents`). This is the
   black-hole-fix philosophy applied to naming: never silently fuse.
3. **Clone:** insert the `to` row copying from `from`: tuple (socket, session
   id/name/created, pane), harness_session_id, project, label, label_manual,
   role, model_type, **and the read watermark** (`last_read_entry_id`,
   `last_read_at`) — without the watermark copy the new identity would see
   all of history as unread. `registered_at` stamps now (it is a new
   identity's birth); `last_seen` stamps now.
4. **Retire:** tombstone `from` (DepartAgent — normal soft delete; its mail
   history stays drillable and drainable).
5. **Journal:** `Kind:"become", Agent:<to>, Detail:"<from> → <to>"`.
6. **Badges:** reconcile the tuple's badges (the @muster_agent alias list
   now shows the claimed name; departed rows are already excluded).

Response mirrors register's classified shape: `{"ok":true}` with
`{"from","to","unread"}` so surfaces can say "you are now 'x' — N unread".

Mail addressed to the retired seed afterwards: lands on its threads
(black-hole backstop still resolves exact aliases even departed — verify at
implementation; if resolve rejects departed targets, that is CORRECT and the
sender gets a loud error naming the live roster), and any pre-claim unread
drains normally — `session_aliases` includes departed aliases on purpose, so
the Stop hook still lists and drains the seed's stragglers. Notify skips
departed rows (existing behavior), which is fine: the live claimed alias on
the same tuple gets the badge.

### 2. CLI verb `muster become <name>`

Resolves the session's current identity the same way hooks do (env capture,
ancestry-walk fallback — `hookCapture`'s logic, which should be exported or
mirrored per the existing one-canonical-capture rule), finds the session's
live aliases via the tuple, and calls the op. Zero live aliases → error
("nothing to become from; register first"). Exactly one → that's `from`.
More than one → require `--from <alias>` (split identities are rare but
legal; never guess). Prints: `you are now 'alias-routing' (was 'muster-2') —
N unread thread(s)`.

### 3. MCP surface: a `become` flag on register_agent

No new tool. `register_agent` gains optional `become: true`. Today, calling
register_agent with a different alias from an already-registered pane REFUSES
("already registered as 'muster-2' — use that alias; not adding a second") —
the guard against accidental split identity. That refusal is exactly where
the claim belongs:

- `become:true` + pane already registered → the daemon op runs
  (from = the pane's current alias, to = the requested alias); Detail:
  `"you are now 'alias-routing' (was 'muster-2'); N unread thread(s)"`.
- `become:true` + pane NOT registered → plain register (nothing to claim
  from; degrade gracefully, don't error).
- The refusal message (become:false path) is updated to advertise the flag:
  `"already registered as 'muster-2' — use that alias, or pass become:true
  to claim '<requested>' as this session's name"`.

Deliberate split identity (two live aliases, one session) remains possible
via the CLI (`muster register <extra>`); the MCP path now nudges toward
become because accidental splits were always the bug there (lake-broker).

### 4. Teaching: two sentences, one nudge

- Coordination skill, in the register section: when the work has a name,
  claim it — `register_agent(alias:"<real-name>", become:true)`; the seed
  name (your tmux session's) is a placeholder, not your identity.
- No stop-hook wording changes, no wrapper changes, no dotfiles changes.
  The wrapper keeps seeding the tmux name at launch (correct: nothing
  meaningful exists yet); prefix T keeps setting labels; become is the rare,
  deliberate identity claim.

### Interplay with v0.8.0 machinery (all verified conceptually; test each)

- **Resume reclaim:** the claimed row carries the harness link, so
  SessionStart source:"resume" reclaims the claimed name onto the new tuple.
  The retired seed is departed and (usually) dead-tupled — reclaim revives
  the claimed alias, not the seed. Test the full chain: seed → become →
  kill tmux → resume → "reconnected as '<claimed>'".
- **SessionEnd:** tombstones every alias the pane owns — claimed row
  included, seed already departed. No change.
- **Ghost reap / DepartStaleSiblings:** become clones the CURRENT tuple with
  its real session_created, so the clone is never reaped as a ghost.
- **gc:** the retired seed is departed → purgeable per existing rules.

## Non-goals

- Renaming that rewrites history (rejected above).
- Automatic naming (deriving a name from the conversation topic): the claim
  is a deliberate act by the agent or operator; a wrong auto-name is worse
  than a placeholder.
- Merging two existing identities (become refuses existing `to`).
- Label semantics changes of any kind.

## Test surface

- Store/daemon: become clone copies watermark + identity fields; guards
  (missing from; existing to, live and tombstoned); journal event; badge
  reconcile; unread in response.
- CLI: single-alias happy path; multi-alias requires --from; zero-alias
  error; env-stripped resolution via walk.
- MCP: become:true claims through the pane guard; become:true unregistered
  degrades to plain register; refusal message advertises the flag.
- Integration: post-become Stop hook drains a straggler sent to the seed
  pre-claim; resume-after-become reclaims the claimed name.
