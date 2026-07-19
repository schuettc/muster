# muster station — operator TUI + intent + session identity (design)

Date: 2026-07-16 · Status: reviewed (muster-2 adversarial pass, thread 27 —
22 findings, 6 blockers; all folded in or explicitly decided), approved to
build
Local-only spec (docs/ is untracked in the public repo). Ships as v0.6.0.

## Problem

The operator has no live surface: watching the bus means a `muster watch`
tail in one shell, `muster agents` in another, and `get_thread` spelunking to
read anything. Three defects found during live use ride along because the
station is half-blind without them:

1. **No intent on messages** — "1.2.2 shipped, FYI" and "please review this
   spec" are indistinguishable to inboxes, drains, and the journal.
2. **Split-alias identity** — a session registered under both its tmux
   session name and a chosen alias (bettor-help-workspace-2 +
   nfl-research-agent, live case) has two read-states. The Stop hook drains
   the session-name alias while peers address the chosen one, so the chosen
   alias's unread count accretes wrongly (`lit(2)` incident).
3. **Reply rows masquerade as duplicates** — journal reply rows render the
   thread subject, so an announcement and its reply look like a double-send.

## Goal

`muster station`: a full-screen operator TUI — roster, live feed, threads,
and the ability to act (read / reply / send / nudge) — plus the three backend
fixes. Station is a **peer client of the daemon** over the unix socket (no
MCP; MCP is the LLM-agent adapter) and is **addressable**: it registers as
agent `station`.

## Non-goals

- No MCP server changes beyond the `intent` input field.
- No push/streaming transport — station polls like watch does.
- No mouse support, no themes/config files. Keyboard only; knobs are flags.
- The dedicated tmux session that launches station is dotfiles work,
  requested over the bus after release — out of scope here.

## Design

### 1. Backend: `list_threads` op (side-effect-free)

The threads pane must never consume an agent's mailbox (`get_inbox`
mark-reads — the peek problem, which stranded a live message on 07-16).

- Store: `Threads(limit int) ([]Thread, error)` — newest `updated_at` first
  (ties broken by `id DESC`), `limit` clamped to 500 (default 100 when
  <= 0). The entry annotations aggregate ONLY over the already-limited
  thread set (subquery first, then join), never the full entries table —
  station polls this every second.
- Last-entry identification is by `MAX(entries.id)` (the append order), and
  `last_from`/`last_at` come from that same row — never a GROUP BY picking a
  non-aggregated column, never `MAX(created_at)` (millisecond ties).
- Daemon op `list_threads` args `{limit}` → `{"threads": [...]}`. Reads
  nothing else, marks nothing, notifies nobody (regression test: all flags
  and read-states identical before/after).
- Wire Thread gains `intent` (effective value, §2), `last_from`, `last_at`,
  `entry_count`.

### 2. Backend: message `intent`

- Schema: `ALTER TABLE threads ADD COLUMN intent TEXT NOT NULL DEFAULT ''`
  (additive migration; fresh CREATE updated to match).
- Vocabulary: `''` (unspecified) | `fyi` | `reply-requested` |
  `action-requested`. Validation and defaulting live at the STORE boundary
  (`CreateThread` rejects unknown values) so MCP, CLI, station, and tests
  cannot diverge; the daemon just passes through.
- **Effective intent is a derived value, one canonical SQL fragment**:
  `CASE WHEN kind='task' AND intent='' THEN 'action-requested' ELSE intent
  END` — used by every read surface (Threads, SessionUnread action count,
  the events join). A task IS a request for action, including every
  pre-existing v0.5 task row; no migration backfill, no retroactive
  inconsistency between old and new tasks.
- Surfaces:
  - MCP `send_message`/`task_create` inputs gain optional `intent` (schema
    docs teach agents to mark FYIs — the drain loop gets cheaper).
  - CLI: `muster send --intent fyi|reply-requested|action-requested`.
  - Journal: send/task rows append ` [fyi]` / ` [action]` / ` [reply?]` to
    WHAT when intent is set. No schema change to events: `Events()` adds
    `COALESCE(threads.intent,'')` to its existing LEFT JOIN (exactly like
    subject) and the wire Event/eventRow gain a query-time `intent` field;
    the renderer maps it to the tag.
  - Stop hook drain text: unchanged wording plus, when any unread thread's
    effective intent is `action-requested`, the count is split: "You have N
    unread muster thread(s), M needing action." Both numbers come from
    `SessionUnread` (§3) — one query, session-level, no second counting
    path.
- Inbox ordering: `Inbox()` unchanged (newest first). Station groups by
  intent client-side; the MCP surface stays stable.

### 3. Backend: split-alias identity fix — session-level unread model

Design principle (review finding): **all aliases sharing one exact
`(socket_path, session_id)` tuple are ONE actor identity** for unread math,
actor exclusion, and notification. Records are not deduped (a session may
keep its session-name alias and a chosen alias); their accounting is
unified. Empty socket/session tuples are never grouped (an agent without
tmux identity is its own singleton).

- **Read watermark becomes an entry ID, not a wall clock.** `agents` gains
  `last_read_entry_id INTEGER NOT NULL DEFAULT 0` (additive migration;
  initialized once from the max `entries.id` with `created_at <=
  last_read_at` so existing read-states carry over). `MarkRead` records the
  max entry ID visible in the same transaction as the Inbox read. All
  unread predicates become `e.id > last_read_entry_id` — the strict-`>`
  same-millisecond loss class (and its fake-clock test contortions) goes
  away. `last_read_at` stays for display only.
- **One canonical session-unread query.**
  `SessionUnread(socketPath, sessionID string) (total, action int)`:
  `COUNT(DISTINCT t.id)` over threads T where ∃ alias A in the session such
  that T concerns A (threadConcerns) AND ∃ entry `e.id >
  A.last_read_entry_id` whose `from_agent` is **not any alias of the
  session** (actor exclusion is session-based — a session's own writes
  under either alias never make it unread). `action` counts the subset
  whose effective intent is action-requested. No summing of per-alias
  counts anywhere — that double-counts shared threads.
- **Notify coalesces by session.** `notifyForThread` builds the affected
  *sessions* (originator + recipients, grouped by tuple), drops the actor's
  entire session, and per session: computes `SessionUnread` once, sets
  `@muster_inbox` once, journals ONE notify row (Agent = the addressed
  alias that put the session in scope; Count = the session count). No
  duplicate lit rows for sibling aliases.
- **Mutation→recompute→write is locked.** The daemon holds a per-session-
  tuple mutex (lazily created map) around the {store mutation, SessionUnread
  recompute, tmux option write} sequence in both notify and get_inbox
  paths, with the recompute inside the lock — closes the stale-write race
  where a concurrent drain's smaller count is overwritten by an in-flight
  notify's larger one. Tests drive deterministic interleavings via the
  injectable notifier, not just sequential flows.
- **get_inbox semantics.** If Inbox succeeds but MarkRead fails, the op
  FAILS (no read event journaled, badge untouched) — a read that didn't
  persist must not report success. On success it recomputes the session
  count inside the lock: drains of one alias leave the badge at the
  remainder (the lit(2) fix). Sum-query or tmux failures journal
  `detail: "error: …"` and never blind-Clear.
- **Hook drains every alias of the session.** New op `session_aliases` args
  `{socket_path, session_id}` (both non-empty or the op fails) →
  `{"aliases": [...]}` sorted, deduplicated. The hook's block message lists
  them all with get_inbox instructions per alias; zero rows or op failure →
  current behavior (session name). Documented as a point-in-time hint over
  mutable registration data, not identity.
- **Reconciliation on identity changes.** register/deregister/gc recompute
  and rewrite the badge for any session tuple they touch, so stale flags
  don't survive re-registration or reaping.

### 3b. Backend: nudge text carries the drain instruction

Live finding (2026-07-16, thread 27): a nudged Codex checked its inbox,
*listed* the new thread, and idled — "check your inbox" satisfies itself,
and by then its own get_inbox has cleared the flag so the Stop hook never
escalates. The nudge line becomes the hook's wording: "check your muster
inbox: call get_inbox, read each new thread with get_thread, handle the
request, and reply on the thread — act autonomously." One string in
`internal/nudge`; the typed-only path for unknown models unchanged.

### 4. Backend: reply rows carry a body preview

`reply` journal rows get `Detail = sanitizeForDisplay(body, 80)` at journal
time (detail is currently empty for replies; the duplication into the
30-day-pruned local journal is accepted deliberately — no entry_id column).
Renderer: reply WHAT becomes `↳ <detail>` when detail is non-empty, else
the subject. `claim`/`transition` unchanged.

**`sanitizeForDisplay` becomes the one display sanitizer** (renderer +
journal previews + station panes): strips ALL control characters — ESC/CSI
sequences, NUL, bidi controls — not just `\t\n\r`, and truncates by
**display width** (wide/combining runes counted properly) rather than rune
count, so one hostile subject can't corrupt the TUI or wrap a line.
Fuzz-tested with control-sequence and wide-character corpora.

### 5. `muster station` (TUI)

- **Dependency**: `github.com/charmbracelet/bubbletea` + `bubbles` +
  `lipgloss`, versions pinned (pure Go — the build stays `CGO_ENABLED=0`).
  First dependency beyond modernc sqlite and the MCP SDK; accepted
  deliberately. **Gated, not asserted**: `just verify` gains a `cross`
  step building all four release targets (darwin/linux × arm64/amd64)
  under `CGO_ENABLED=0`, and `release.yml` is reordered to BUILD all
  targets before creating the GitHub release — today a failed cross-build
  leaves an already-created empty release (pre-existing defect, fixed
  here because the new dependency raises the odds of hitting it).
- **Identity, collision-safe**: on start, station registers alias
  `station` (role `operator`, model `station`) with its real tmux identity.
  If the alias already exists on a LIVE record with a *different* session
  tuple, registration fails over to `station-2`, `station-3`, … (a dead
  record is taken over). Deregistration is deferred (runs on q, signal, and
  panic paths after terminal restore) and CONDITIONAL: it re-fetches the
  record and deregisters only if the session tuple still matches its own —
  a second station or a re-registered agent is never deleted by the first's
  exit. TUI init failure rolls the registration back. `gc` covers hard
  crashes. `nudge` treats model `station` as typed-only (existing
  unknown-model default).
- **Layout** (three panes + status line, focus cycles with Tab):
  - **Roster** (left, ~30 cols): project-grouped agents — live dot, label
    (alias fallback, the v0.5.1 `disp` rules), unread count. Cursor selects
    an agent; `n` nudges it (confirmation line first: "nudge <label>? y/n").
  - **Feed** (right-top): the journal tail, v0.5.1 renderer verbatim (WHO
    arrows, labels, width-capped WHAT). Follows unless scrolled up;
    `End`/`G` snaps back to live.
  - **Threads** (right-bottom): from `list_threads` — id, intent tag,
    participants (from → to, labels), last speaker + age, subject.
    `action-requested` group pinned on top, then `reply-requested`, then
    the rest, newest first within groups. Enter opens the thread view.
  - **Thread view** (overlay): entries oldest first, wrapped, loaded via
    `get_thread` with its new `{offset, limit}` pagination (station passes
    limit 200 with lazy "load older"; args absent = everything, so existing
    callers are untouched). `r` opens the composer as a reply; Esc closes.
    Reading is side-effect-free everywhere, with ONE explicit exception:
    **opening** a thread addressed to `station` acknowledges it (a targeted
    read for station's alias) — there is NO focus-based or pane-based
    auto-read, so a station left running unattended never silently consumes
    requests (review finding; the earlier auto-read design is dropped).
  - **Composer** (bottom line, opens on `s`/`r`): single-line input; for
    `s`, a target picker filtered from the roster precedes it (label or
    alias match); `--intent` cycled with a keystroke (F/R/A indicator);
    Enter sends via the same `send_message`/`reply` ops the CLI uses; Esc
    cancels. Errors land in the status line, never a crash.
- **Data loop — the MODEL owns the cursor.** No free-running poller
  goroutine: on each `tea.Tick` (default `--interval` 1s) the model issues
  three independent Cmds — `list_events after_id=<model's cursor>`,
  `list_agents`, `list_threads` — each returning its own message. The event
  cursor advances only when the model APPLIES an event page, so a dropped
  or failed delivery can never skip events; roster/threads failures don't
  block the feed. Panes are independently versioned — no decision is ever
  derived from a mixed tick bundle (there is no cross-pane snapshot to
  trust). Cursor discipline otherwise identical to watch (max_id,
  regression reset). Errors: status-line note + retry, never exit. Thread
  selection is preserved by thread ID across regrouping, so a poll never
  jumps the operator's cursor.
- **Keys**: Tab focus · j/k or arrows move · Enter open · Esc back · `s`
  send · `r` reply · `n` nudge · `/` filter (fuzzy over the focused pane) ·
  `a` toggle aliases/labels · `q` quit (deregisters).
- **Flags**: `--interval` (1s), `--aliases`, `--width` handled by the
  framework, `--alias` (default `station`, so two stations on one machine
  don't collide).

### 6. Testing

- **store**: `Threads()` ordering (updated_at DESC, id DESC ties),
  limit-first aggregation, last-entry-by-max-id under same-millisecond
  entries; intent + `last_read_entry_id` migrations on a hand-built v0.5
  DB (reopened twice, watermark initialized from last_read_at); intent
  validation + effective-intent CASE at every read surface (old task rows
  count as action); `SessionUnread` — the double-count case (broadcast
  concerning both sibling aliases counts ONCE), session-based actor
  exclusion (sibling alias's write is not unread), empty-tuple singleton.
- **daemon**: `list_threads` marks nothing read (flags/read-states
  byte-identical before/after); session-coalesced notify — two sibling
  aliases, one lit row, one badge write; the lit(2) regression (drain one
  alias → badge shows remainder); deterministic interleaving of
  notify-vs-drain under the session lock (injectable notifier ordering);
  get_inbox fails when MarkRead fails (no read event, badge untouched);
  `session_aliases` rejects empty fields, returns sorted/deduped;
  register/deregister/gc badge reconciliation; reply preview sanitized;
  get_thread pagination back-compat (no args = all entries).
- **hook**: multi-alias drain text (sorted); op-failure fallback; "M
  needing action" wording gated on action-requested unread; nudge line
  carries the full drain-and-act instruction (§3b).
- **station**: model-level Update/View tests (no PTY): cursor advances only
  on applied event pages (fail the threads fetch after a successful events
  fetch → nothing skipped); roster labels/counts; intent grouping with
  selection preserved across regroup; open-to-acknowledge (opening a
  station thread reads it; focus alone reads nothing); composer send/reply
  invoke the real ops against a test daemon; nudge confirmation; collision
  fail-over to station-2; conditional deregister leaves a re-registered
  alias alone.
- **rendering**: `sanitizeForDisplay` fuzz — ESC/CSI/NUL/bidi stripped,
  display-width truncation with wide/combining runes.
- Full `just verify` incl. the new `cross` matrix step; VERSION `0.6.0`.

## Sequencing (implementation slices)

1. Store foundations: intent column + store-boundary validation +
   effective-intent fragment; `last_read_entry_id` migration + ID-watermark
   MarkRead; `SessionUnread`; `Threads()`; migration tests.
2. Daemon identity/notify rewiring: session grouping + per-session lock,
   coalesced notify, get_inbox strict semantics, `session_aliases`,
   register/deregister/gc reconciliation + interleaving tests.
3. Daemon surface: `list_threads`, intent pass-through, get_thread
   pagination, reply preview + `sanitizeForDisplay` (shared with renderer)
   + fuzz tests.
4. Hook + nudge: multi-alias drain, action-count wording, nudge
   drain-and-act text + tests.
5. CLI: `send --intent`, journal intent tag + `↳` preview rendering +
   tests.
6. Release safety: `just cross` matrix step in verify/CI; release.yml
   builds before creating the release.
7. Station scaffold: pinned dependencies, model/update/view skeleton,
   tick-driven Cmds with model-owned cursor, roster + feed panes + model
   tests.
8. Station threads + thread view (paginated) + intent grouping +
   open-to-acknowledge.
9. Station actions + lifecycle: composer (send/reply with intent picker),
   nudge with confirm, filter, aliases toggle; collision fail-over,
   conditional deferred deregister, terminal restore on all exit paths.
10. Docs (README + site one-sentence mention), usage line, VERSION 0.6.0,
    live smoke ($TMUX-scrubbed per the 07-16 lesson), ship.

## Risks / notes

- bubbletea is the first sizeable dependency; it is pure Go and vendored by
  go.sum like everything else. If it ever blocks `CGO_ENABLED=0`, that is a
  release blocker by rule.
- The session-level badge changes `@muster_inbox` semantics from per-alias
  to per-session distinct-thread counts. The tmux render and hook read the
  option identically; only the number can differ (it becomes truthful).
  Documented in README.
- `last_read_entry_id` supersedes wall-clock read semantics; `last_read_at`
  is retained as display metadata only. The one-time watermark
  initialization is approximate for pre-migration edge cases (entries at
  exactly last_read_at count as read) — acceptable, matches prior strict-`>`
  behavior.
- Two records per session remain legal; the fix makes them coherent, not
  forbidden. A future `muster register --takeover` could absorb the
  session-name record; deliberately out of scope.
