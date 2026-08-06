# Conversation-as-identity: one name across Claude, tmux, and the bus

Status: draft for review
Date: 2026-08-05
Scope: muster (this repo) + dotfiles (statusline, prefix T shim). Each side must
remain fully usable without the other.

## 1. Problem

Two incidents, one root cause: the name a human uses for a session is not the
identity the machinery routes on.

**The nfl-a2 trap.** A session was visibly titled `nfl-a2` on every surface —
Claude session name, tmux tab, roster — yet "send a message to nfl-a2" failed.
The name had flowed Claude → statusline → tmux option as an *auto* label
(`label_manual = 0`), and the resolver (correctly) routes only on manually
pinned labels. Every surface displayed a name that no surface would route on.

**The nfl-3 restart confusion.** A laptop restart killed the tmux server. The
user resumed the conversation custom-titled `nfl-3` in a fresh tmux session
that happened to receive the recycled tmux ID `$1`. Registration correctly
reclaimed the conversation's alias row (`bettor-help-workspace-3`) by harness
session id — but a legacy named row (`nfl-research-agent`, created pre-v0.8.0
with no `harness_session_id` and `session_created = 0`) sat tombstoned on the
same `$1` tuple. The badge aggregation and roster matched it against the new
live session by tuple alone, so the resumed session was told it owned two
aliases with two unread counts, and mail addressed to the dead name pointed at
nobody. The user's intent was simple: *the thing I resumed is nfl-3; it should
just be nfl-3.*

## 2. Verified facts (2026-08-05, Claude Code 2.1.222)

These were confirmed against live docs, the local install, and real
transcripts; the design leans on all of them.

- The transcript records a user-set name as a structured line:
  `{"type":"custom-title","customTitle":"nfl-3","sessionId":"…"}`, re-emitted
  periodically through the file. **A custom-title record is proof of an
  intentional naming gesture.** (The statusline docs do not mention it; the
  transcript format carries it regardless.)
- The statusline JSON input includes both `session_name` and
  `transcript_path`. `session_name` merges custom names and AI-generated
  topics with no source flag — useless alone for intent, which is why the
  current statusline sync classifies everything from Claude as auto.
- There is no hook event for rename. `SessionStart` hooks receive
  `transcript_path` and may *return* `sessionTitle` (set the name at start);
  no other hook can.
- Mid-session, the only external way to set Claude's name is typing
  `/rename <name>` into the pane — muster's existing `syncAgentName` path.
- Codex has no `/rename` and no transcript custom-title; its naming gesture
  remains tmux-side only.

## 3. The model

**The conversation is the identity.** A Claude conversation (harness session
UUID + transcript) survives laptop restarts, tmux server death, resumes, and
pane moves. Its custom title is the one durable, user-set name. Everything
else is a projection, re-derived whenever the conversation lands somewhere:

| Layer | Role | Lifetime |
|---|---|---|
| Transcript `custom-title` | **The name.** Written by any naming gesture. | Conversation |
| Bus alias (e.g. `bettor-help-workspace-3`) | Stable machine id; threads/mail anchor. Never the human name. | Conversation (via harness-id reclaim) |
| Bus label + `label_manual` | The address (v0.9.1: labels ARE addresses). Projection of the name. | Follows the name |
| tmux `@claude_task` / `@claude_task_manual` | Neutral display cache: status-left, tab titles, proj picker. Shared contract between dotfiles and muster; owned by neither. | tmux session |
| Claude `session_name` | What Claude shows; fed by the custom title. | Conversation |

Two rules make the projections safe:

1. **Intent is explicit.** Every intentional gesture produces the manual flag
   (tmux `_manual` option, bus `label_manual`); every automatic sync writes
   only the label and never sets or clears the flag. New: a `custom-title`
   record counts as intent — the sync *promotes* to manual when one exists,
   and never demotes.
2. **Identity is never guessed from tmux state.** tmux tuples are recyclable
   display coordinates. Registration reclaims by `harness_session_id` only;
   tuple matches always require an incarnation check (§5.1).

## 4. Flows

**New unnamed session.** Registers at SessionStart exactly as today
(anonymous alias — badges, wake, and reachability from minute one). Displays
the auto-topic. Not addressable by label until named. *(The "don't join until
named" alternative was considered and rejected: the restart incident was
caused by a lineage-less legacy row, not by registration timing, and deferred
joining would cost unnamed sessions their inbox badges and reachability.)*

**Naming gestures — all converge on the custom title.**
- *prefix T* → `muster label <name>` (or the muster-less fallback, §5.3):
  writes tmux option + manual flag, syncs the bus, and types `/rename <name>`
  into a live Claude pane — which writes the custom-title record. Steady
  state: all layers agree.
- */rename inside Claude* → custom-title record appears → statusline sync
  reads it (via `transcript_path`), recognizes intent, writes tmux label
  **with** the manual flag, and syncs the bus when muster is present. The
  previously "cosmetic" gesture becomes first-class.
- *"rename yourself" (agent-initiated)* → optional follow-up, not required
  for this design (§8).

**Resume / restart — the payoff.** At SessionStart the muster hook already
registers via `whereami` + harness-id reclaim. New behavior: it also reads
the transcript's last `custom-title` record; if present, it registers with
that as the manual label AND writes the tmux option pair (via the explicitly
resolved socket — hook environments are stripped, `whereami` provides the
coordinates). Resuming `nfl-3` in any pane on any day yields: same alias row,
label `nfl-3` (manual), correct tab title, and `send nfl-3` routes —
zero gestures.

**Collisions stay loud.** If another live session already holds the same
manual label, resolution reports ambiguity (existing `uniqueOrErr` contract)
rather than misrouting, and the SessionStart projection surfaces the conflict
rather than silently stealing the name. No silent fallback.

## 5. Changes by component

### 5.1 muster

- **SessionStart projection** (hook path, `internal/humancli/hook.go`): after
  the existing register, read `transcript_path` from the hook payload, scan
  for the last `custom-title` record, and when found: set bus label
  (manual) via `set_label` and write the tmux option pair through `tmuxenv`
  using the whereami-resolved socket. Best-effort like `syncLabelToBus` — a
  failed projection degrades to today's behavior, never a wrong write.
- **Incarnation guards on every tuple match.** Wherever a stored row is
  matched to a live tmux session by (socket, session id) — badge/unread
  aggregation, roster live-dot enrichment, label re-reads — require
  `session_created` to match the live session's creation epoch, and treat
  `session_created = 0` as *never matching*. This kills the double-identity
  confusion and the known departed-live-dot display quirk with one rule.
- **Legacy-row sweep.** A one-time prune (or `muster agents --prune`
  extension) retiring rows with empty `harness_session_id` — they are
  permanently unreclaimable by the ancestry walk and exist only as collision
  bait. Their threads remain readable by id; only the agent rows retire.
- Registration timing is unchanged: register at SessionStart, anonymous is
  fine, lineage does the work.

### 5.2 dotfiles

- **statusline.sh sync upgrade.** Today: copies `session_name` → tmux label,
  auto-only, backs off behind the manual flag. New: also check
  `transcript_path` for a `custom-title` record (cheap `grep -m1` on tail /
  last occurrence). If the current `session_name` matches a custom title,
  write the label WITH the manual flag (promote); otherwise keep today's
  auto behavior. Never demote a manual flag. Muster-free by design.
- **prefix T shim** (`bin/tmux-muster-label.sh`): drop the hard `muster`
  dependency. If `command -v muster` → delegate exactly as today — **the
  /rename injection is retained unchanged; it is working well and this
  design must not regress it** (operator-confirmed 2026-08-05). Otherwise
  fall back to plain `tmux set-option` for the label + manual flag +
  `refresh-client`. No send-keys in the fallback — typing into panes stays
  muster's job (nudge handles liveness/idle); standalone prefix T aligns
  tmux + tabs and leaves Claude's internal name to /rename.

### 5.3 The contract (cross-repo, documented in both)

The tmux option pair `@claude_task` / `@claude_task_manual` is the neutral
meeting point: intentional writers set both, automatic syncs write only the
label and defer to the flag, readers (display and routing alike) trust the
pair. muster already honors this (`MUSTER_LABEL_OPTION` keeps the option name
configurable); dotfiles honors it in statusline + shim. A short section in
each repo's CLAUDE.md pins the contract so the two cannot drift.

## 6. Standalone guarantees

- **dotfiles without muster:** prefix T labels tmux and tabs (fallback path);
  /rename flows to tabs via the statusline, now correctly marked manual.
  Nothing references the bus; nothing breaks. What's absent is only what
  cannot exist without a bus: addressing.
- **muster without dotfiles:** `muster label`, the SessionStart projection,
  and the resolver are self-contained via `tmuxenv`. No prefix T binding, no
  statusline sync — labels come from `muster label` and the projection.
- **Both:** the gestures upgrade transparently; same option pair, same rules.

## 7. Edge cases

- **Codex sessions:** no custom-title, no /rename. prefix T / `muster label`
  remains their only naming gesture — unchanged, still fully addressable.
- **/rename after prefix T:** both write the custom title (prefix T via
  injection), so the newest gesture wins everywhere. If injection was skipped
  (Codex, dead pane), the tmux manual label stands and the sync never
  clobbers it.
- **Multiple Claude panes in one tmux session:** bus labels are per-agent and
  stay accurate; the session-scoped tmux option is display-only and
  last-writer-wins. Acceptable: the option is a cache, not the identity.
- **Same conversation resumed twice concurrently:** existing become/CAS
  guards apply; the second registration's projection surfaces the conflict.
- **Transcript unreadable / no custom-title:** projection is a no-op;
  behavior degrades to exactly today's.

## 8. Non-goals

- Reviving `nfl-research-agent` or any legacy named row (its unread mail is
  operator cleanup, not design).
- An MCP "rename yourself" tool — nice-to-have follow-up once this lands;
  the gestures above cover the observed workflows without it.
- Detecting /rename in real time (no hook exists); statusline-tick latency
  (seconds) is accepted.
- Any change to alias assignment, become, or the resolver's precedence rules.

## 9. Hosted-backend integration (feat/hosted-backend)

The hosted backend (2026-08-03 design/plan: per-device daemons, Lambda +
DynamoDB store behind a bearer token, `device_id` on agent rows, global
roster, `device_poll` wake) changes where state lives but not where naming
happens. The integration points, verified against the branch:

- **The projection is device-local and store-agnostic.** The SessionStart
  projection runs in the hook on the device hosting the conversation —
  transcript read, `tmuxenv` writes, `set_label` op. `set_label` is already
  on the remote write-op allowlist with idempotency keys, and dynamostore's
  `SetSessionLabel` is device-scoped. Remote mode changes nothing in the
  flow. No component ever writes tmux state for another device (the daemon
  never types; cross-device changes arrive as badge reconciliation via
  `device_poll`).
- **Resolution goes global — that's the feature.** `resolveAgentTarget`
  stays daemon-side over `ListAgents`, which in remote mode is the global
  roster: the laptop can `send nfl-3` to a session on the desktop. Duplicate
  manual labels in the same project across devices hit the existing loud
  ambiguity error. Device-qualified addressing (beyond `proj:label`) is a
  non-goal for v1.
- **Incarnation guards are device-scoped tuples.** tmux session IDs (`$1`)
  collide across devices *by construction*, and the branch already threads
  `device_id` through every tuple match (`sameSession`,
  `DepartStaleSiblings`, `SetSessionLabel`). The §5.1 guard is therefore
  `(device_id, socket_path, session_id)` + `session_created` equality.
- **Attribution and reaping stay distinct decisions.** The branch's
  `DepartStaleSiblings` deliberately *spares* `session_created = 0` rows
  from tombstoning (a pre-upgrade row on a still-live session self-heals on
  re-register). The §5.1 guard does not conflict: it refuses to *attribute*
  such rows to a live session (badges, roster live-dot, label re-reads)
  without ever tombstoning them. Both rules are the conservative direction
  for their respective irreversibility.
- **Guard and sweep live at the store-API layer**, implemented and tested in
  BOTH stores (SQLite + dynamostore) so local and remote mode cannot drift.
  The sweep criteria tighten for the sparing rule above: retire rows with
  empty `harness_session_id` only when they are also departed or stale by
  `last_seen` — a live pre-upgrade session that will self-heal is not
  swept.
- **Conversation identity is device-portable by construction.** Harness-id
  reclaim runs against the global roster; if a conversation ever lands on
  another device, re-registration updates the row's `device_id` and the
  projection re-derives the name there. No new mechanism needed.
- **Sequencing:** this design lands on `dev`; `feat/hosted-backend` already
  merges `dev` forward regularly. The guard/sweep must be expressed as
  store-API semantics before the hosted branch's next `dev` merge, so
  dynamostore implements them from the interface rather than retrofitting.

## 10. Testing

- **muster:** unit tests for the incarnation guard (recycled tuple +
  `session_created = 0` never matches; mismatched epoch never matches;
  same tuple on a different `device_id` never matches); hook-projection
  tests against fixture transcripts (with/without custom-title, unreadable
  path); sweep tests pinning that only empty-harness-id rows that are
  departed-or-stale retire. Guard and sweep tests run against both stores
  once dynamostore lands. `just verify` gates as always.
- **dotfiles:** extend the existing `tests/*.test.zsh` harness — shim
  fallback with muster absent from PATH; statusline promote-on-custom-title,
  never-demote, auto backoff behind the manual flag.
- **Operator acceptance (the merge gate):** restart tmux server, resume a
  custom-titled conversation in a fresh session, and verify with no gestures:
  correct tab title, `muster agents` shows the manual label, and a peer's
  `send <name>` routes to it.
