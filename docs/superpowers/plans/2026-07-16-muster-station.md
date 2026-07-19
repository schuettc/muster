# muster station Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.
> **Normative companion:** `docs/superpowers/specs/2026-07-16-muster-station-design.md` — every task brief below names its spec sections; the spec text governs on any ambiguity. Implementers receive both files.

**Goal:** `muster station` operator TUI + the four backend fixes (intent, session-level identity, list_threads, reply previews), shipped as v0.6.0.

**Architecture:** Backend first (store → daemon identity rewiring → daemon surface → hook/nudge → CLI → release safety), each slice independently testable; then the TUI in three slices (scaffold → threads → actions/lifecycle) on Bubble Tea with a model-owned cursor; docs+ship last. Station is a peer daemon client over the unix socket — no MCP changes beyond the `intent` input field.

**Tech Stack:** Go, modernc.org/sqlite, charmbracelet/bubbletea+bubbles+lipgloss (pinned; first UI dependency — pure Go). `CGO_ENABLED=0` everywhere including a new cross-build gate.

## Global Constraints

- `just verify` green after every task + standalone `golangci-lint run ./...; echo exit=$?`.
- Socket-using tests: `internal/mustertest.ShortHome()`, never `t.TempDir()` for socket dirs.
- stdout sacred in mcp mode; daemon/store stay tmux-agnostic; `internal/nudge` is the only send-keys path; no cgo; no LIKE against aliases; knobs not constants.
- Journal writes stay best-effort. All migrations additive + idempotent (duplicate-column guard), fresh `schema.sql` updated in the same task as each ALTER.
- Session tuple = exact `(socket_path, session_id)`, both non-empty; empty tuples are singletons, never grouped.
- Branch `feat/station` in a worktree off `origin/dev`. VERSION → `0.6.0` in the final task only.

---

### Task 1: Store foundations — intent, entry-ID watermark, SessionUnread, Threads()

**Spec:** §1, §2, §3 (store bullets), Testing (store). This is the largest store task; it is one task because the unread predicates, intent CASE, and watermark are one coherent semantic change.

**Files:** Modify `internal/store/{schema.sql,store.go,models.go,threads.go,agents.go}`; Test `internal/store/{threads_test.go,agents_test.go,migrate_test.go}`.

**Interfaces (later tasks consume exactly these):**

```go
// models.go
const (IntentFYI = "fyi"; IntentReply = "reply-requested"; IntentAction = "action-requested")
// Thread gains: Intent string `json:"intent"`; LastFrom string `json:"last_from"`;
//               LastAt int64 `json:"last_at"`; EntryCount int `json:"entry_count"`
//               (last three query-time only, populated by Threads()).
// Agent gains: LastReadEntryID int64 `json:"last_read_entry_id"`.

// threads.go
// effectiveIntent is the ONE canonical SQL fragment (spec §2):
const effectiveIntent = `CASE WHEN threads.kind='task' AND COALESCE(threads.intent,'')='' THEN 'action-requested' ELSE COALESCE(threads.intent,'') END`
func (s *Store) Threads(limit int) ([]Thread, error) // updated_at DESC, id DESC; limit clamp [1..500], <=0 → 100
// CreateThread validates intent ∈ {"", fyi, reply-requested, action-requested} → error otherwise.

// agents.go
func (s *Store) SessionUnread(socketPath, sessionID string) (total, action int, err error)
// MarkRead(alias): in ONE transaction, read COALESCE(MAX(id),0) FROM entries and write it
// to last_read_entry_id (last_read_at also updated, display-only).
// UnreadCount(alias) predicate becomes e.id > agent's last_read_entry_id (drop the
// wall-clock comparison; keep threadConcerns + from_agent != alias shape).
```

**Migrations (store.go alters list + schema.sql):**
```sql
ALTER TABLE threads ADD COLUMN intent TEXT NOT NULL DEFAULT ''
ALTER TABLE agents  ADD COLUMN last_read_entry_id INTEGER NOT NULL DEFAULT 0
-- one-time init, guarded by "only when column was just added" is NOT detectable;
-- instead run always and idempotently:
UPDATE agents SET last_read_entry_id =
  COALESCE((SELECT MAX(e.id) FROM entries e WHERE e.created_at <= agents.last_read_at), 0)
  WHERE last_read_entry_id = 0 AND last_read_at > 0
```

**SessionUnread SQL (spec §3 — COUNT DISTINCT threads, session-based actor exclusion):**
```sql
WITH sess AS (SELECT alias, last_read_entry_id FROM agents
              WHERE socket_path = ?1 AND session_id = ?2 AND ?1 != '' AND ?2 != '')
SELECT
  COUNT(DISTINCT t.id),
  COUNT(DISTINCT CASE WHEN <effectiveIntent> = 'action-requested' THEN t.id END)
FROM threads t
JOIN sess ON <threadConcerns with sess.alias>            -- see implementation note
WHERE EXISTS (SELECT 1 FROM entries e WHERE e.thread_id = t.id
              AND e.id > sess.last_read_entry_id
              AND e.from_agent NOT IN (SELECT alias FROM sess))
```
Implementation note: `threadConcerns` binds a literal alias 3×; for the JOIN form, inline an equivalent predicate against `sess.alias` (agent-target = sess.alias OR role subquery on sess.alias OR broadcast OR from_agent = sess.alias) — keep it adjacent to `threadConcerns` with a comment binding the two, and add a test asserting both predicates agree on a fixture matrix (this is the "one canonical predicate" rule surviving a join).

**Threads() shape (spec §1 — limit-first, last entry by MAX(id)):**
```sql
WITH recent AS (SELECT * FROM threads ORDER BY updated_at DESC, id DESC LIMIT ?),
     last AS (SELECT e.thread_id, MAX(e.id) AS max_id, COUNT(*) AS n FROM entries e
              WHERE e.thread_id IN (SELECT id FROM recent) GROUP BY e.thread_id)
SELECT recent.<threadCols…>, <effectiveIntent> AS intent, le.from_agent, le.created_at, last.n
FROM recent JOIN last ON last.thread_id = recent.id
            JOIN entries le ON le.id = last.max_id
ORDER BY recent.updated_at DESC, recent.id DESC
```

- [ ] Failing tests first, notably: `TestSessionUnreadCountsDistinctThreads` (broadcast concerning two sibling aliases → total 1), `TestSessionUnreadExcludesSiblingAuthors` (alias A writes; sibling B stays read), `TestMarkReadRecordsEntryWatermark` (same-millisecond entry after snapshot stays unread — no fake clock needed), `TestThreadsLastEntrySameMillisecond` (two entries same ts → MAX(id) wins), `TestIntentValidationAtStore`, `TestEffectiveIntentOldTasksAreAction` (pre-migration task row with intent '' counts in action), migration test on a hand-built v0.5 schema reopened twice with watermark init asserted.
- [ ] Implement; existing UnreadCount tests will need the watermark rewrite (they get SIMPLER — delete fakeTick usage where it existed only for read-watermark ordering).
- [ ] `just verify` + lint; commit `store: intent, entry-id read watermark, SessionUnread, Threads()`.

---

### Task 2: Daemon — session identity rewiring (locks, coalesced notify, strict get_inbox, session_aliases, reconciliation)

**Spec:** §3 complete. **Files:** `internal/daemon/daemon.go`; tests `internal/daemon/{daemon_test.go,wake_wiring_test.go}`.

**Interfaces:**
```go
// daemon.go
type Daemon struct { …existing…; sessMu sync.Mutex; sessLocks map[string]*sync.Mutex }
func (d *Daemon) sessionLock(socket, session string) *sync.Mutex // lazily created; key socket+"\x00"+session
// notifyForThread: build affected SESSIONS (group originator+recipients' agents by tuple,
// singletons for empty tuples), DROP the actor's entire session, then per session under
// sessionLock: (total, action) := s.SessionUnread(...); n.Notify(socket, session, total);
// ONE journal row {Kind:"notify", Agent:<the alias that put the session in scope>,
// ThreadID, Count: total, Detail: lit/cleared/error…}.
// get_inbox: threads := Inbox(alias); if MarkRead fails → fail(err), NO read event, badge
// untouched. On success, under sessionLock: recompute SessionUnread; set option to total
// (Clear when 0); journal read event; sum/tmux errors journal detail "error: …", never blind Clear.
// New op session_aliases {socket_path, session_id} (both non-empty else fail) →
// {"aliases": sorted deduped []string}.
// register_agent / deregister_agent / gc's deregister path: after mutation, under
// sessionLock recompute + rewrite the badge for every tuple touched (old AND new on re-register).
```

- [ ] Failing tests: `TestNotifyCoalescesSiblingAliases` (two aliases one tuple, one broadcast → exactly ONE notify row, one Notify call, count 1), `TestLit2Regression` (peer replies on threads concerning both siblings → badge = distinct count; drain ONE alias → badge rewritten to remainder, not cleared), `TestNotifyDrainInterleaving` (injectable notifier whose Notify blocks on a channel: start notify, complete a get_inbox drain mid-flight, release → final option value equals post-drain recompute, not the stale pre-drain count), `TestGetInboxFailsWhenMarkReadFails` (close the store's DB handle trick or an alias with no agent row per current MarkRead semantics — pick the injectable seam that exists; if none, add an error-injecting store wrapper seam in the test file), `TestSessionAliasesRejectsEmptyTuple`, `TestReRegisterReconcilesOldSessionBadge`.
- [ ] Implement. The actor-session drop replaces `delete(recipients, actor)` — resolve the actor's tuple via its agent record; unregistered actor (e.g. "operator") falls back to alias-only exclusion.
- [ ] `just verify` + lint; commit `daemon: session-level unread — coalesced notify, locked recompute, strict get_inbox, session_aliases`.

---

### Task 3: Daemon surface — list_threads, intent pass-through, get_thread pagination, reply preview + sanitizeForDisplay

**Spec:** §1 (op), §2 (pass-through), §4. **Files:** `internal/daemon/daemon.go`, `internal/store/events.go` (events join gains intent), new `internal/humancli/display.go` — NO: sanitizer must be daemon-reachable without importing humancli → new package `internal/display` with `func Sanitize(s string, maxWidth int) string`; humancli re-exports/uses it. Tests alongside.

**Interfaces:**
```go
// internal/display (new): Sanitize strips C0/C1 controls, ESC/CSI sequences, bidi controls
// (U+202A–202E, U+2066–2069), NUL; collapses \t\n\r to single spaces; truncates by
// DISPLAY WIDTH (go doesn't need a dep: use golang.org/x/text? NO new deps — implement
// width via unicode.Is(unicode.Mn)==0-width + East Asian Wide/Fullwidth ranges table,
// ~30 lines, tested). Appends '…' when cut.
// daemon: send_message/task_create pass str(a,"intent") into CreateThread (store validates).
// reply journals Detail: display.Sanitize(body, 80).
// get_thread args gain {offset, limit} (both optional; absent/0 = everything, back-compat);
// response unchanged in shape.
// list_threads {limit} → {"threads": [...]} — side-effect-free.
// store Events(): LEFT JOIN adds <effectiveIntent> AS intent; Event/eventRow gain Intent
// (query-time, json:"intent").
```

- [ ] Failing tests: `TestListThreadsMarksNothingRead` (snapshot agents table + tmux option before/after — byte-identical), `TestIntentPassThroughAndRejection` (bad intent → fail from store validation), `TestGetThreadPagination` (offset/limit slices; no args = all), `TestReplyPreviewSanitized` (body with ESC sequence + newline → journal detail clean), display fuzz test (control corpus + wide runes; property: output printable, width ≤ max).
- [ ] Implement; `just verify` + lint; commit `daemon: list_threads, intent pass-through, get_thread pagination, sanitized reply previews`.

---

### Task 4: Hook + nudge — multi-alias drain, action wording, drain-and-act nudge text

**Spec:** §3 (hook bullet), §3b, §2 (drain wording). **Files:** `internal/humancli/hook.go`, `internal/nudge/nudge.go`; tests alongside.

- Hook Stop: capture tuple via tmuxenv; call `session_aliases`; ≥1 alias → reason lists each: `Your muster aliases are 'a', 'b' (this tmux session). For EACH alias call get_inbox, read new threads with get_thread, handle, and reply.` Count line becomes `You have N unread muster thread(s)` and appends `, M needing action` when M>0 — N and M read from a new tiny op? NO: hook reads @muster_inbox for N as today (now the session count); M requires a query — add `session_aliases` sibling data? Simplest per spec: hook calls a new op `session_unread {socket_path, session_id}` → `{"total":n,"action":m}` (daemon: SessionUnread under the lock-free read path). Fall back to the tmux option value on op failure. (This op also serves station later.)
- Nudge typed line becomes: `check your muster inbox: call get_inbox, read each new thread with get_thread, handle the request, and reply on the thread — act autonomously.` (One const in nudge.go; unknown-model typed-only unchanged.)
- [ ] Failing tests: hook block message with two aliases (fake tmuxenv Run seam + test daemon), op-failure fallback to session-name text, action-count wording gated on M>0; nudge test asserts the new const typed.
- [ ] Implement; `just verify` + lint; commit `hook+nudge: multi-alias drain, action counts, drain-and-act nudge`.

---

### Task 5: CLI — send --intent, journal intent tags + reply previews

**Spec:** §2 (CLI/journal), §4 (renderer). **Files:** `internal/humancli/{humancli.go,events.go}`; tests.

- `cmdSend` gains `--intent` flag (validated client-side against the three values + empty for a clearer error, though the store re-validates).
- Renderer: eventRow gains Intent; `what()` appends ` [fyi]` / ` [reply?]` / ` [action]` for send/task rows with intent set; reply rows with non-empty Detail render `↳ <detail>` instead of the subject. `oneLine` call sites migrate to `display.Sanitize` (width-aware) — delete `oneLine`/rune `truncate` where superseded, keeping printed-width discipline from v0.5.1.
- [ ] Failing tests: send --intent lands on the thread (via list_threads or DB); journal shows the tag; reply row shows `↳` preview not the duplicated subject (extends the v0.5.1 duplicate-look finding's test).
- [ ] Implement; `just verify` + lint; commit `cli: send --intent; journal intent tags and reply previews`.

---

### Task 6: Release safety — cross-build gate before release creation

**Spec:** §5 dependency bullet. **Files:** `justfile`, `.github/workflows/{ci.yml,release.yml}`.

- `just cross`: loop darwin/linux × arm64/amd64, `CGO_ENABLED=0 GOOS=… GOARCH=… go build -o /dev/null ./cmd/muster`. Add to `verify` recipe (fast — build cache) and CI.
- `release.yml`: reorder so all four target builds complete BEFORE `gh release create`; a build failure leaves NO release. Keep asset names/checksums identical.
- [ ] Verify by reading the workflow diff carefully (no live release to test); `just cross` runs locally in the worktree.
- [ ] Commit `release: cross-build all targets before creating the release; just cross gate`.

---

### Task 7: Station scaffold — dependency, model skeleton, tick-driven fetches, roster + feed

**Spec:** §5 (dependency, identity, layout roster/feed, data loop, keys/flags subset). **Files:** new `internal/station/{station.go,model.go,poll.go,…}` (package station; humancli stays thin: `case "station": return station.Run(args[1:])`), `cmd/muster/main.go` usage, `go.mod` (pinned bubbletea/bubbles/lipgloss). Tests `internal/station/model_test.go` (model-level Update/View — no PTY).

**Interfaces:** `station.Run(args []string) error`. Model owns `cursor int64`; `tickMsg` → three `tea.Cmd`s (events/agents/threads) each yielding its own msg type; cursor advances ONLY in the events-msg handler (spec data-loop section verbatim — including regression reset and never deriving decisions from mixed bundles). Registration/collision/conditional-deregister per spec §5 identity — implement registration in this task, defer-and-restore lifecycle hardening lands in Task 9.

- [ ] Failing model tests: `TestCursorAdvancesOnlyOnAppliedEvents` (events msg applied → cursor moves; threads fetch failure msg → cursor untouched, nothing skipped on next tick), `TestRosterRendersLabelsAndCounts`, `TestFeedUsesRendererVocabulary` — the renderer lives in humancli today; EXTRACT it in THIS task: move the v0.5.1 renderer (renderer struct, eventRow/eventsPage, fetchEvents, loadLabels) into a new `internal/render` package consumed by both humancli and station (mechanical move; internal/render imports `internal/display` from Task 3 for Sanitize; all existing humancli tests keep passing against thin wrappers or updated imports).
- [ ] Implement; `just verify` (now incl. cross — proves bubbletea survives the matrix) + lint; commit `station: scaffold — bubbletea skeleton, model-owned cursor, roster+feed`.

---

### Task 8: Station threads pane + thread view

**Spec:** §5 threads/thread-view bullets. **Files:** `internal/station/…`; tests.

- Threads pane from `list_threads`: intent groups (action → reply → rest; effective intent already server-side), `updated_at DESC, id DESC` in group; selection preserved by thread ID across refresh. Enter → thread view via paginated `get_thread` (limit 200, lazy "load older" on reaching top).
- Open-to-acknowledge: opening a thread addressed to `station` (or station's chosen alias) triggers get_inbox for station's alias — the ONLY read station ever performs; focus alone reads nothing.
- [ ] Failing model tests: grouping order, selection stability across regroup, opening a station-addressed thread acknowledges it (fake daemon asserts exactly one get_inbox, only on open), pagination lazy-load.
- [ ] Implement; `just verify` + lint; commit `station: threads pane + paginated thread view + open-to-acknowledge`.

---

### Task 9: Station actions + lifecycle

**Spec:** §5 composer/keys + identity lifecycle. **Files:** `internal/station/…`; tests.

- Composer: `r` reply (thread context), `s` send (roster-filtered target picker), intent cycled F/R/A, Enter → `reply`/`send_message` ops, Esc cancel, errors to status line. `n` nudge with `y/n` confirm (calls the nudge package path — station is an operator surface; reuse `internal/nudge` exactly as cmdNudge does, including the self-report). `/` filter on focused pane; `a` aliases toggle; `q` quit.
- Lifecycle: single deferred conditional deregister (tuple must still match) covering q/signal/panic; terminal restore on all exits; registration rollback if TUI init fails; goroutines stopped.
- [ ] Failing model tests: composer send invokes op with intent (fake daemon), reply targets selected thread, nudge confirm gate, collision failover station-2, conditional deregister leaves re-registered alias untouched.
- [ ] Implement; `just verify` + lint; commit `station: composer, nudge-with-confirm, filter, lifecycle hardening`.

---

### Task 10: Docs, VERSION, ship

- README: station section (what it is, keys, addressable as `station`), intent flag, session-badge semantics note, gc/hook wording updates. site/index.html: one sentence where the CLI/mailbox is described, per the page's copy standards. Usage lines (main.go + humancli) gain `station`.
- `printf '0.6.0\n' > VERSION`.
- Live smoke ($TMUX/TMUX_PANE scrubbed for any register in the smoke — 07-16 lesson): run station against a scratch MUSTER_HOME with a scripted send; capture pane renders the thread; quit deregisters.
- [ ] `just verify`; commit `docs+version: muster station ships as 0.6.0`. PR feat/station → dev; promote dev → main (release v0.6.0 auto-cuts, now build-gated); `contrib/release-sign.sh v0.6.0`.
