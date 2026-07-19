# muster watch — bus journal + live tail: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every bus action becomes one journal row; `muster watch` tails that journal live with filters.

**Architecture:** The v0.4 `events` table grows into the full bus journal (new kinds `send`/`reply`/`task`/`claim`/`transition`/`nudge` beside `notify`/`read`, plus a `target` column and a thread-subject join). The daemon journals at dispatch time, best-effort, action row before its notify rows. `muster watch` polls the existing one-request/one-response protocol with an explicit backlog/follow mode and a `max_id` cursor. Spec: `docs/superpowers/specs/2026-07-16-muster-watch-design.md` (read it first — it carries the reviewed decisions).

**Tech Stack:** Go, modernc.org/sqlite (cgo-free), stdlib only. No new dependencies.

## Global Constraints

- `just verify` green after every task (gofmt, golangci-lint, `go test -race`, `CGO_ENABLED=0` build). Also run `golangci-lint run ./...` standalone and echo its exit code.
- macOS sockets: tests that open a daemon socket use `internal/mustertest.ShortHome()` — never `t.TempDir()` for socket dirs.
- stdout is sacred in mcp mode; nothing here may print from library code.
- Knobs, not constants: every default in this plan is flag-tunable.
- No LIKE against user-supplied aliases anywhere. Exact matches only.
- The journal is best-effort: an `AppendEvent` failure must never fail or block the op it describes.
- Work on branch `feat/watch` in a worktree off `origin/dev`. VERSION bumps to `0.5.0` in the final task.

---

### Task 1: Store — `target` + `subject` columns, journal kinds, migration

**Files:**
- Modify: `internal/store/schema.sql` (events CREATE gains `target`)
- Modify: `internal/store/store.go` (migrate() gains the ALTER)
- Modify: `internal/store/models.go` (Event gains `Target`, `Subject`)
- Modify: `internal/store/events.go` (AppendEvent writes target)
- Test: `internal/store/events_test.go`, `internal/store/migrate_test.go` (new)

**Interfaces:**
- Consumes: existing `store.Event`, `AppendEvent`, `Open`.
- Produces: `Event{ID, TS int64; Kind, Agent, Target string; ThreadID int64; Count int; Detail, Subject string}` — `Subject` is query-time only (joined, never stored); `AppendEvent(e Event) error` persists `Target`.

- [ ] **Step 1: Write the failing migration test**

`internal/store/migrate_test.go`:

```go
package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrateAddsTargetToV04Events builds the exact v0.4 events schema by
// hand (current Open's CREATE IF NOT EXISTS would not alter it), inserts a
// row, then opens the store twice: the ALTER must apply once, idempotently,
// and the old row must read back with target ''.
func TestMigrateAddsTargetToV04Events(t *testing.T) {
	dir := t.TempDir() // no sockets here; plain file DB is fine
	dbPath := filepath.Join(dir, "bus.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL, kind TEXT NOT NULL,
		agent TEXT NOT NULL DEFAULT '', thread_id INTEGER NOT NULL DEFAULT 0,
		count INTEGER NOT NULL DEFAULT 0, detail TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO events (ts, kind, agent, thread_id, count, detail)
		VALUES (1, 'notify', 'api', 7, 1, 'lit')`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	for i := 0; i < 2; i++ { // reopen twice: migration must be idempotent
		s, err := Open(dbPath)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		evs, err := s.Events(EventQuery{Backlog: true, Limit: 10})
		if err != nil {
			t.Fatalf("events %d: %v", i, err)
		}
		if len(evs) != 1 || evs[0].Target != "" || evs[0].Agent != "api" {
			t.Fatalf("v0.4 row after migrate: %+v", evs)
		}
		_ = s.Close()
	}
}
```

(`Events`/`EventQuery` arrive in Task 2 — write this test now, it compiles
after Task 2's types exist; within this task assert via `AppendEvent` + a
direct `SELECT target FROM events` instead if you want it green earlier.
The committed version at the END of Task 2 must be the one above.)

For THIS task, the compiling failing test is the simpler round-trip in
`events_test.go`:

```go
func TestAppendEventPersistsTarget(t *testing.T) {
	s := newTestStore(t)
	if err := s.AppendEvent(Event{Kind: "send", Agent: "web", Target: "agent:api", ThreadID: 3, Detail: "subj"}); err != nil {
		t.Fatal(err)
	}
	var target string
	if err := s.DB().QueryRow(`SELECT target FROM events`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != "agent:api" {
		t.Fatalf("target = %q, want agent:api", target)
	}
}
```

- [ ] **Step 2: Run it, verify it fails**

Run: `go test ./internal/store/ -run TestAppendEventPersistsTarget`
Expected: compile error — `Event` has no field `Target`.

- [ ] **Step 3: Implement**

`models.go`: add to Event (after `Agent`):

```go
	Target string `json:"target"` // 'agent:x' / 'role:r' / 'broadcast' / bare alias (nudge)
```

and after `Detail`:

```go
	// Subject is joined from the event's thread at query time (empty for
	// thread-less events). Never stored on the row.
	Subject string `json:"subject"`
```

`schema.sql` events CREATE gains, after `agent`:

```sql
    target    TEXT NOT NULL DEFAULT '',    -- 'agent:x' / 'role:r' / 'broadcast' / bare alias (nudge)
```

`store.go` migrate() alters list gains:

```go
		`ALTER TABLE events ADD COLUMN target TEXT NOT NULL DEFAULT ''`,
```

`events.go` AppendEvent:

```go
	_, err := s.db.Exec(`
INSERT INTO events (ts, kind, agent, target, thread_id, count, detail)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		clock.NowMillis(), e.Kind, e.Agent, e.Target, e.ThreadID, e.Count, e.Detail)
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/store/`
Expected: PASS (RecentEvents still exists and ignores target — fine until Task 2).

- [ ] **Step 5: Commit** — `git add -A && git commit -m "store: events journal gains target column (schema + idempotent migration)"`

---

### Task 2: Store — `Events(EventQuery)`, `MaxEventID`, subject join

**Files:**
- Modify: `internal/store/events.go` (replace `RecentEvents` with `Events`; add `MaxEventID`)
- Modify: `internal/daemon/daemon.go:` the `list_events` case only — mechanical swap to keep the build green (full op behavior is Task 5)
- Test: `internal/store/events_test.go`, finalize `migrate_test.go` from Task 1

**Interfaces:**
- Consumes: `threadConcerns` (`internal/store/threads.go` — binds alias 3×), `threadCols`.
- Produces:

```go
type EventQuery struct {
	Agent    string // exact-alias concern filter ("" = all)
	Kind     string // exact kind ("" = all)
	ThreadID int64  // >0 filters to one thread
	AfterID  int64  // follow mode: id > AfterID, oldest-first
	Limit    int    // backlog mode row cap; 0 in backlog mode = no rows
	Backlog  bool   // true: newest-first LIMIT; false: follow mode
}
func (s *Store) Events(q EventQuery) ([]Event, error)
func (s *Store) MaxEventID() (int64, error)
```

Validation inside `Events`: `Limit` clamped to 1000; negative `AfterID`/`ThreadID` → error `fmt.Errorf("negative id")`; backlog `Limit <= 0` returns nil rows, no error. Unknown Kind matches nothing (plain equality).

- [ ] **Step 1: Write the failing tests**

Append to `events_test.go` (replace the old `TestEventsAppendListFilterAndLimit` body where it calls `RecentEvents` — the semantics it asserted move here):

```go
func TestEventsBacklogAndFollowModes(t *testing.T) {
	s := newTestStore(t)
	for i, k := range []string{"send", "reply", "notify"} {
		if err := s.AppendEvent(Event{Kind: k, Agent: "web", ThreadID: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	back, err := s.Events(EventQuery{Backlog: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[0].Kind != "notify" || back[1].Kind != "reply" {
		t.Fatalf("backlog newest-first limit 2: %+v", back)
	}
	follow, err := s.Events(EventQuery{AfterID: back[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(follow) != 1 || follow[0].Kind != "notify" {
		t.Fatalf("follow after id %d: %+v", back[1].ID, follow)
	}
	if none, _ := s.Events(EventQuery{Backlog: true, Limit: 0}); len(none) != 0 {
		t.Fatalf("backlog limit 0 must return no rows, got %d", len(none))
	}
	if _, err := s.Events(EventQuery{AfterID: -1}); err == nil {
		t.Fatal("negative AfterID must error")
	}
	max, err := s.MaxEventID()
	if err != nil || max != back[0].ID {
		t.Fatalf("MaxEventID = %d (%v), want %d", max, err, back[0].ID)
	}
}

// TestEventsAgentFilterMatchesThreadConcern is the finding-1 regression: a
// reply row has empty target, so only the thread-concern join can match the
// originator.
func TestEventsAgentFilterMatchesThreadConcern(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(Thread{Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api"}, "req")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Event{
		{Kind: "send", Agent: "web", Target: "agent:api", ThreadID: id, Detail: "req"},
		{Kind: "reply", Agent: "api", ThreadID: id},
		{Kind: "nudge", Target: "web"},
		{Kind: "send", Agent: "x", Target: "agent:zzz", ThreadID: 999},
	} {
		if err := s.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Events(EventQuery{Agent: "web", Backlog: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 { // its send (actor), api's reply (thread concern), the nudge (bare target)
		t.Fatalf("agent=web should match 3 events, got %d: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Agent == "x" {
			t.Fatalf("unrelated event leaked through agent filter: %+v", e)
		}
	}
}

func TestEventsJoinsThreadSubject(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(Thread{Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api", Subject: "hello subj"}, "b")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(Event{Kind: "notify", Agent: "api", ThreadID: id, Count: 1, Detail: "lit"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(Event{Kind: "read", Agent: "api"}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Events(EventQuery{Backlog: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if evs[1].Subject != "hello subj" || evs[0].Subject != "" {
		t.Fatalf("subject join: notify=%q (want hello subj), read=%q (want empty)", evs[1].Subject, evs[0].Subject)
	}
}
```

- [ ] **Step 2: Run, verify failure** — `go test ./internal/store/ -run 'TestEvents'` → compile error (`EventQuery` undefined).

- [ ] **Step 3: Implement `Events` + `MaxEventID`** in `events.go`:

```go
// maxEventLimit bounds any single Events query.
const maxEventLimit = 1000

// EventQuery selects journal rows. Mode is explicit: Backlog=true reads
// newest-first up to Limit; otherwise follow mode reads id > AfterID
// oldest-first. Agent matches events the alias is CONCERNED in — as actor,
// as exact 'agent:<alias>' target, as bare-alias target (nudge), or via the
// event's thread satisfying threadConcerns (what makes replies on your
// threads match despite their empty target). Role matching uses the alias's
// current role, same as threadConcerns everywhere else.
type EventQuery struct {
	Agent    string
	Kind     string
	ThreadID int64
	AfterID  int64
	Limit    int
	Backlog  bool
}

func (s *Store) Events(q EventQuery) ([]Event, error) {
	if q.AfterID < 0 || q.ThreadID < 0 {
		return nil, fmt.Errorf("negative id in event query")
	}
	if q.Backlog && q.Limit <= 0 {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 || limit > maxEventLimit {
		limit = maxEventLimit
	}
	where := []string{"1=1"}
	var args []any
	if q.Agent != "" {
		where = append(where, `(events.agent = ?
   OR events.target = 'agent:'||?
   OR events.target = ?
   OR (events.thread_id > 0 AND EXISTS (
        SELECT 1 FROM threads WHERE threads.id = events.thread_id AND `+threadConcerns+`)))`)
		args = append(args, q.Agent, q.Agent, q.Agent, q.Agent, q.Agent, q.Agent)
	}
	if q.Kind != "" {
		where = append(where, "events.kind = ?")
		args = append(args, q.Kind)
	}
	if q.ThreadID > 0 {
		where = append(where, "events.thread_id = ?")
		args = append(args, q.ThreadID)
	}
	order := "events.id DESC"
	if !q.Backlog {
		where = append(where, "events.id > ?")
		args = append(args, q.AfterID)
		order = "events.id ASC"
	}
	args = append(args, limit)
	rows, err := s.db.Query(`
SELECT events.id, events.ts, events.kind, events.agent, events.target,
       events.thread_id, events.count, events.detail,
       COALESCE(threads.subject, '')
FROM events LEFT JOIN threads ON threads.id = events.thread_id
WHERE `+strings.Join(where, " AND ")+`
ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.Agent, &e.Target, &e.ThreadID, &e.Count, &e.Detail, &e.Subject); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxEventID returns the journal high-water mark (0 on an empty journal).
func (s *Store) MaxEventID() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&n)
	return n, err
}
```

Delete `RecentEvents` and `defaultEventLimit`. In `daemon.go`'s `list_events` case, mechanical swap so the tree compiles (real op behavior is Task 5):

```go
	case "list_events":
		evs, err := d.s.Events(store.EventQuery{Agent: str(a, "agent"), Backlog: true, Limit: 50})
```

Note `threadConcerns` binds the alias 3× — hence the six `q.Agent` args (3 for the direct forms + 3 for the join). Verify count against the fragment before committing.

- [ ] **Step 4: Run** — `go test ./internal/store/ ./internal/daemon/ ./internal/humancli/` → PASS (humancli events still renders; its filter arg keeps working through the swap). Finalize `migrate_test.go` to the Events-based version from Task 1 Step 1.

- [ ] **Step 5: Commit** — `git commit -am "store: EventQuery/Events with explicit modes, concern filter, subject join; MaxEventID"`

---

### Task 3: Store + daemon + gc — retention (`PruneEvents` → `prune_events` → `gc --events-keep`)

**Files:**
- Modify: `internal/store/events.go`, `internal/daemon/daemon.go`, `internal/humancli/identity.go` (cmdGC)
- Test: `internal/store/events_test.go`, `internal/daemon/daemon_test.go`, `internal/humancli/identity_test.go`

**Interfaces:**
- Produces: `func (s *Store) PruneEvents(olderThanMillis int64) (int64, error)`; daemon op `prune_events` args `{older_than_ms}` (accepts string or number via `i64`) → `{"pruned": n}`, rejects `older_than_ms <= 0`; `muster gc [--events-keep <dur>]` default `720h`, rejects `<= 0`, reports both reap and prune results independently.

- [ ] **Step 1: Failing store test** (fake clock, exact boundary):

```go
func TestPruneEventsExactBoundarySurvives(t *testing.T) {
	fakeTick(t) // from threads_test.go — strictly increasing clock
	s := newTestStore(t)
	for i := 0; i < 3; i++ { // rows at ts 1, 2, 3
		if err := s.AppendEvent(Event{Kind: "read", Agent: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneEvents(2) // DELETE WHERE ts < 2: only ts=1 goes
	if err != nil || n != 1 {
		t.Fatalf("pruned %d (%v), want 1", n, err)
	}
	left, _ := s.Events(EventQuery{Backlog: true, Limit: 10})
	if len(left) != 2 { // ts=2 (exactly at cutoff) must survive
		t.Fatalf("rows after prune = %d, want 2", len(left))
	}
}
```

- [ ] **Step 2: Run, fails** (`PruneEvents` undefined). Implement:

```go
// PruneEvents deletes journal rows with ts < olderThanMillis (a row exactly
// at the cutoff survives), returning the count deleted.
func (s *Store) PruneEvents(olderThanMillis int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM events WHERE ts < ?`, olderThanMillis)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
```

- [ ] **Step 3: Daemon op + test.** In dispatch (near `list_events`):

```go
	case "prune_events":
		cutoff := i64(a, "older_than_ms")
		if cutoff <= 0 {
			return fail(fmt.Errorf("older_than_ms must be > 0"))
		}
		n, err := d.s.PruneEvents(cutoff)
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"pruned": n})
```

Daemon test: append two events via store with fake clock, call op with the middle ts, assert `pruned: 1` and that `older_than_ms: 0` fails.

- [ ] **Step 4: gc flag + test.** `cmdGC` gains a flag set (it currently takes none — give it one): `eventsKeep := fs.Duration("events-keep", 720*time.Hour, ...)`; reject `<= 0` with `fmt.Errorf("--events-keep must be > 0")`; after the reap loop, call `prune_events` with `older_than_ms = clock.NowMillis() - eventsKeep.Milliseconds()` **as a string** (`strconv.FormatInt`) and print `pruned N event(s)` on its own line; a prune error prints to the same writer but does not mask the reap summary. Test drives `Dispatch([]string{"gc", "--events-keep", "1h"})` against the test daemon after inserting an old event with the fake clock.

- [ ] **Step 5: `just verify`, commit** — `git commit -am "retention: PruneEvents + prune_events op + gc --events-keep (720h default)"`

---

### Task 4: Daemon — journal the five bus actions; claim notifies

**Files:**
- Modify: `internal/daemon/daemon.go` (send_message, task_create, reply, task_claim, task_transition cases)
- Test: `internal/daemon/wake_wiring_test.go`

**Interfaces:**
- Consumes: `logEvent`, `store.Event{Target, Subject}`.
- Produces: journal rows per spec's kind table. Exact target encoding: `to_kind == "broadcast"` → `"broadcast"`; else `to_kind + ":" + to_target`. Ordering: journal row appended immediately after the successful mutation, **before** `notifyForThread`. `task_claim` now also calls `d.notifyForThread(threadID, by)`.

- [ ] **Step 1: Failing test** (extend wake_wiring_test.go):

```go
// TestBusActionsJournalInOrder: one send + reply must produce an interleaved
// journal — send row BEFORE its notify row, reply row BEFORE its notify row —
// and task claim must both journal and notify.
func TestBusActionsJournalInOrder(t *testing.T) {
	n := &fakeNotifier{}
	sock, s := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "api", "model_type": "claude", "socket_path": "/s", "session_id": "$2"})
	resp := call(t, sock, "send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "subj", "body": "x"})
	tid := threadIDOf(t, resp) // helper: unmarshal resp.Data.thread_id (extract from TestReplyNotifiesOriginatorWithUnread)
	call(t, sock, "reply", map[string]any{"thread_id": tid, "from": "api", "body": "done"})

	evs, err := s.Events(store.EventQuery{Backlog: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	// oldest-first for assertion readability
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"send", "notify", "reply", "notify"}
	if len(kinds) != 4 || !slices.Equal(kinds, want) {
		t.Fatalf("journal order = %v, want %v", kinds, want)
	}
	if evs[0].Target != "agent:api" || evs[0].Detail != "subj" || evs[0].Agent != "web" {
		t.Fatalf("send row: %+v", evs[0])
	}
}

func TestTaskClaimJournalsAndNotifies(t *testing.T) {
	n := &fakeNotifier{}
	sock, s := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "api", "model_type": "claude", "socket_path": "/s", "session_id": "$2"})
	resp := call(t, sock, "task_create", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "do it", "body": "x"})
	tid := threadIDOf(t, resp)
	before := len(n.snap(&n.notified))
	call(t, sock, "task_claim", map[string]any{"thread_id": tid, "by": "api"})
	if got := n.snap(&n.notified); len(got) != before+1 || got[len(got)-1] != "$1" {
		t.Fatalf("claim must notify the originator's session, notified=%v", got)
	}
	evs, _ := s.Events(store.EventQuery{Kind: "claim", Backlog: true, Limit: 5})
	if len(evs) != 1 || evs[0].Agent != "api" || evs[0].ThreadID != tid {
		t.Fatalf("claim journal row: %+v", evs)
	}
}
```

- [ ] **Step 2: Run, fails** (no journal rows; claim doesn't notify).

- [ ] **Step 3: Implement.** In each case, right after the successful store call and before `notifyForThread`:

```go
	// send_message:
	d.logEvent(store.Event{Kind: "send", Agent: str(a, "from"), Target: targetOf(a), ThreadID: id, Detail: str(a, "subject")})
	// task_create: same but Kind: "task"
	// reply:
	d.logEvent(store.Event{Kind: "reply", Agent: str(a, "from"), ThreadID: i64(a, "thread_id")})
	// task_claim (also ADD the notify call after it):
	d.logEvent(store.Event{Kind: "claim", Agent: str(a, "by"), ThreadID: i64(a, "thread_id")})
	d.notifyForThread(i64(a, "thread_id"), str(a, "by"))
	// task_transition:
	d.logEvent(store.Event{Kind: "transition", Agent: str(a, "by"), ThreadID: i64(a, "thread_id"), Detail: str(a, "status")})
```

with one helper near `logEvent`:

```go
// targetOf renders a thread address as a journal target: 'broadcast' or
// '<to_kind>:<to_target>'.
func targetOf(a map[string]any) string {
	if str(a, "to_kind") == "broadcast" {
		return "broadcast"
	}
	return str(a, "to_kind") + ":" + str(a, "to_target")
}
```

- [ ] **Step 4: Run** — `go test ./internal/daemon/`. The pre-existing `TestNotifySkipsAgentsWithoutSession` now sees a `send` journal row too — it asserts on `n.notified`, unaffected. PASS.

- [ ] **Step 5: Commit** — `git commit -am "daemon: journal send/task/reply/claim/transition; task_claim now notifies"`

---

### Task 5: Daemon — `log_event` (canonical nudge row) + full `list_events`; nudge self-reports

**Files:**
- Modify: `internal/daemon/daemon.go`, `internal/humancli/humancli.go` (cmdNudge)
- Test: `internal/daemon/daemon_test.go`, `internal/humancli/humancli_test.go`

**Interfaces:**
- Produces: op `log_event` args `{target, detail}` — target must be a registered alias (`GetAgent` found), detail must be `"typed"` or `"submitted"`; daemon constructs `Event{Kind: "nudge", Agent: "", Target: target, Detail: detail}`, everything else zero. Any other shape → `fail`.
- Produces: op `list_events` args `{agent, kind, thread_id, after_id, limit, backlog}` (`after_id` arrives as a decimal **string**; `i64` already coerces) → `{"events": [...], "max_id": n}`; `max_id` present in every response.
- cmdNudge: after typing, best-effort `callData("log_event", map[string]any{"target": alias, "detail": detailWord})` where `detailWord` is `"submitted"` unless `--no-submit` → `"typed"`; a log_event error must not fail the nudge (print nothing).

- [ ] **Step 1: Failing daemon tests**

```go
func TestLogEventConstructsCanonicalNudge(t *testing.T) {
	n := &fakeNotifier{}
	sock, s := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "api", "model_type": "claude", "socket_path": "/s", "session_id": "$2"})
	// attempted pollution: kind/agent/thread_id/count must all be overwritten
	resp := call(t, sock, "log_event", map[string]any{"target": "api", "detail": "submitted", "kind": "send", "agent": "fake", "thread_id": 9, "count": 5})
	if !resp.OK {
		t.Fatalf("log_event: %+v", resp)
	}
	evs, _ := s.Events(store.EventQuery{Kind: "nudge", Backlog: true, Limit: 5})
	if len(evs) != 1 {
		t.Fatalf("want 1 nudge row, got %+v", evs)
	}
	e := evs[0]
	if e.Agent != "" || e.Target != "api" || e.ThreadID != 0 || e.Count != 0 || e.Detail != "submitted" {
		t.Fatalf("canonical nudge row violated: %+v", e)
	}
	if resp := call(t, sock, "log_event", map[string]any{"target": "ghost", "detail": "typed"}); resp.OK {
		t.Fatal("unregistered target must be rejected")
	}
	if resp := call(t, sock, "log_event", map[string]any{"target": "api", "detail": "hacked"}); resp.OK {
		t.Fatal("detail outside typed|submitted must be rejected")
	}
}

func TestListEventsMaxIDAndFollow(t *testing.T) {
	n := &fakeNotifier{}
	sock, _ := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "api", "model_type": "claude", "socket_path": "/s", "session_id": "$2"})
	// empty journal: backlog with limit 0 must still return max_id 0
	resp := call(t, sock, "list_events", map[string]any{"backlog": true, "limit": 0})
	var out struct {
		Events []store.Event `json:"events"`
		MaxID  int64         `json:"max_id"`
	}
	decode(t, resp, &out) // helper: json.Marshal(resp.Data) → Unmarshal
	if out.MaxID != 0 || len(out.Events) != 0 {
		t.Fatalf("empty journal: %+v", out)
	}
	call(t, sock, "send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "s", "body": "b"})
	resp = call(t, sock, "list_events", map[string]any{"after_id": "0"})
	decode(t, resp, &out)
	if out.MaxID < 1 || len(out.Events) < 1 || out.Events[0].Kind != "send" {
		t.Fatalf("follow from 0: %+v", out)
	}
}
```

- [ ] **Step 2: Run, fails.** Implement both ops:

```go
	case "log_event":
		target, detail := str(a, "target"), str(a, "detail")
		if detail != "typed" && detail != "submitted" {
			return fail(fmt.Errorf("log_event: detail must be typed|submitted"))
		}
		if _, found, err := d.s.GetAgent(target); err != nil || !found {
			return fail(fmt.Errorf("log_event: unknown target %q", target))
		}
		// The daemon constructs the canonical event; client fields beyond
		// target/detail are ignored so the journal can't be polluted.
		d.logEvent(store.Event{Kind: "nudge", Target: target, Detail: detail})
		return ok(nil)
	case "list_events":
		evs, err := d.s.Events(store.EventQuery{
			Agent: str(a, "agent"), Kind: str(a, "kind"),
			ThreadID: i64(a, "thread_id"), AfterID: i64(a, "after_id"),
			Limit: int(i64(a, "limit")), Backlog: boolArg(a, "backlog"),
		})
		if err != nil {
			return fail(err)
		}
		maxID, err := d.s.MaxEventID()
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"events": evs, "max_id": maxID})
```

cmdNudge, after the type/submit succeeds:

```go
	detailWord := "submitted"
	if *noSubmit {
		detailWord = "typed"
	}
	_, _ = callData("log_event", map[string]any{"target": alias, "detail": detailWord}) // best-effort journal
```

humancli test: after a nudge against the test daemon (nudge needs a registered agent with a pane — follow the existing nudge test's fake setup), assert a `nudge` journal row exists via a `list_events` call.

- [ ] **Step 3: Run both packages, PASS. Commit** — `git commit -am "daemon: log_event (canonical nudge) + list_events with max_id; nudge self-reports"`

---

### Task 6: CLI — shared event rendering; `events` gains filters + new columns

**Files:**
- Modify: `internal/humancli/events.go`
- Test: `internal/humancli/events_test.go`

**Interfaces:**
- Consumes: `list_events` op (Task 5 shape).
- Produces (Task 7 consumes all three):

```go
type eventRow struct {
	ID       int64  `json:"id"`
	TS       int64  `json:"ts"`
	Kind     string `json:"kind"`
	Agent    string `json:"agent"`
	Target   string `json:"target"`
	ThreadID int64  `json:"thread_id"`
	Count    int    `json:"count"`
	Detail   string `json:"detail"`
	Subject  string `json:"subject"`
}
type eventsPage struct {
	Events []eventRow `json:"events"`
	MaxID  int64      `json:"max_id"`
}
// fetchEvents calls list_events; afterID < 0 means backlog mode.
func fetchEvents(agent, kind string, threadID, afterID int64, limit int) (eventsPage, error)
// printEventLine writes exactly one line; oneLine() maps \t and \n to spaces
// and truncates subject to 60 runes.
func printEventLine(w io.Writer, e eventRow)
func eventHeader(w io.Writer)
```

- [ ] **Step 1: Failing test**

```go
func TestEventsFiltersAndOneLineRendering(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "api", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "line1\nline2\ttabbed", "body": "b"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Dispatch([]string{"events", "--kind", "send"}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "SUBJECT") || !strings.Contains(got, "line1 line2") {
		t.Fatalf("send row with sanitized subject expected:\n%s", got)
	}
	if lines := strings.Count(got, "\n"); lines != 2 { // header + one row
		t.Fatalf("multi-line subject leaked, %d lines:\n%s", lines, got)
	}
	out.Reset()
	if err := Dispatch([]string{"events", "--kind", "read", "--thread", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "send") {
		t.Fatalf("kind filter leaked send rows:\n%s", out.String())
	}
}
```

- [ ] **Step 2: Run, fails. Implement.** Rework `events.go`: `cmdEvents` gains `--kind` and `--thread` flags; calls `fetchEvents(*agent, *kind, int64(*thread), -1, *limit)` (backlog: pass `backlog: true`, omit after_id); default `--limit` stays 50 (flag). Rendering:

```go
func oneLine(s string) string {
	s = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
```

Header `TIME\tKIND\tAGENT\tTARGET\tTHREAD\tCOUNT\tDETAIL\tSUBJECT`; row prints `truncate(oneLine(e.Subject), 60)` and `oneLine(e.Detail)`; thread renders empty when 0 as today. `fetchEvents` sends `after_id` as `strconv.FormatInt(afterID, 10)` when `afterID >= 0` and `backlog: true` otherwise — never both.

- [ ] **Step 3: Run package, PASS. Commit** — `git commit -am "cli: events filters (--kind --thread), target+subject columns, one-line rendering"`

---

### Task 7: CLI — `muster watch`

**Files:**
- Create: `internal/humancli/watch.go`
- Modify: `internal/humancli/humancli.go` (dispatch case), `cmd/muster/main.go` (usage string)
- Test: `internal/humancli/watch_test.go`

**Interfaces:**
- Consumes: `fetchEvents`, `printEventLine`, `eventHeader` (Task 6).
- Produces: `muster watch [--agent A] [--thread N] [--kind K] [--interval 1s] [--backlog 10]`. Internal seam for tests:

```go
// watchOpts carries the loop's injectable seams. Zero value = production:
// wait sleeps interval or returns early on signal; maxPolls 0 = forever.
type watchOpts struct {
	wait     func(d time.Duration) bool // false = shutdown requested
	maxPolls int
	errw     io.Writer // stderr for retry/reset notes; nil = os.Stderr
}
func cmdWatch(args []string, out io.Writer, o watchOpts) error
```

Dispatch calls `cmdWatch(args[1:], out, watchOpts{})`.

- [ ] **Step 1: Failing test**

```go
func TestWatchBacklogThenFollowsAndResets(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "api", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "first", "body": "b"}); err != nil {
		t.Fatal(err)
	}
	polls := 0
	var out, errw bytes.Buffer
	o := watchOpts{
		maxPolls: 2,
		errw:     &errw,
		wait: func(time.Duration) bool {
			polls++
			if polls == 1 { // inject a new event between poll 1 and 2
				if _, err := callData("reply", map[string]any{"thread_id": 1, "from": "api", "body": "done"}); err != nil {
					t.Fatal(err)
				}
			}
			return true
		},
	}
	if err := cmdWatch([]string{"--interval", "1ms"}, &out, o); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "send") || !strings.Contains(got, "first") {
		t.Fatalf("backlog missing:\n%s", got)
	}
	if !strings.Contains(got, "reply") {
		t.Fatalf("followed event missing:\n%s", got)
	}
	sendIdx, replyIdx := strings.Index(got, "send"), strings.Index(got, "reply")
	if sendIdx > replyIdx {
		t.Fatalf("backlog must print before followed rows:\n%s", got)
	}
}

func TestWatchBacklogZeroPrintsNoHistory(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "api", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "old", "body": "b"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	o := watchOpts{maxPolls: 1, wait: func(time.Duration) bool { return true }}
	if err := cmdWatch([]string{"--backlog", "0"}, &out, o); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "old") {
		t.Fatalf("--backlog 0 printed history:\n%s", out.String())
	}
}
```

- [ ] **Step 2: Run, fails. Implement `watch.go`:**

```go
package humancli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// cmdWatch tails the bus journal: prints the last --backlog matching events,
// then polls list_events with the max_id cursor every --interval until
// interrupted. Side-effect-free: never marks anything read.
func cmdWatch(args []string, out io.Writer, o watchOpts) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agent := fs.String("agent", "", "only events concerning this alias")
	kind := fs.String("kind", "", "only this event kind")
	thread := fs.Int64("thread", 0, "only this thread")
	interval := fs.Duration("interval", time.Second, "poll interval")
	backlog := fs.Int("backlog", 10, "history lines to print before following (0 = none)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: muster watch [--agent <alias>] [--thread <id>] [--kind <k>] [--interval <dur>] [--backlog <n>]")
	}
	if o.errw == nil {
		o.errw = os.Stderr
	}
	if o.wait == nil {
		o.wait = signalWait()
	}

	page, err := fetchEvents(*agent, *kind, *thread, -1, *backlog) // backlog mode
	if err != nil {
		return err
	}
	eventHeader(out)
	for i := len(page.Events) - 1; i >= 0; i-- { // newest-first → print oldest-first
		printEventLine(out, page.Events[i])
	}
	cursor := page.MaxID

	for polls := 0; o.maxPolls == 0 || polls < o.maxPolls; polls++ {
		if !o.wait(*interval) {
			return nil // interrupted
		}
		page, err := fetchEvents(*agent, *kind, *thread, cursor, 0) // follow mode
		if err != nil {
			fmt.Fprintln(o.errw, "watch: poll failed, retrying:", err)
			continue
		}
		if page.MaxID < cursor {
			fmt.Fprintf(o.errw, "watch: journal reset (max id %d < cursor %d) — DB replaced? following from the new tail\n", page.MaxID, cursor)
			cursor = page.MaxID
			continue
		}
		for _, e := range page.Events { // follow mode is oldest-first
			printEventLine(out, e)
		}
		cursor = page.MaxID
	}
	return nil
}

// signalWait returns the production wait: sleep d, but return false
// immediately on SIGINT/SIGTERM so Ctrl-C never waits out an interval.
func signalWait() func(time.Duration) bool {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	return func(d time.Duration) bool {
		select {
		case <-sig:
			return false
		case <-time.After(d):
			return true
		}
	}
}
```

`watchOpts` as in Interfaces. Dispatch gains `case "watch": return cmdWatch(args[1:], out, watchOpts{})`. Usage line in `cmd/muster/main.go` becomes `usage: muster <serve|debug|mcp|agents|inbox|send|tasks|events|watch|nudge|register|deregister|gc|hook|label> [args]`. Note the tabwriter used by `eventHeader`/`printEventLine` must flush per line in watch (streaming) — have both helpers write through a plain `fmt.Fprintf` with fixed-width columns OR flush the tabwriter after every row; pick per-line flush.

- [ ] **Step 3: Run package tests, then `just verify`. PASS.**

- [ ] **Step 4: Commit** — `git commit -am "cli: muster watch — poll-follow journal tail with cursor discipline"`

---

### Task 8: Docs, VERSION, ship

**Files:**
- Modify: `README.md`, `site/index.html` (tools/CLI mentions), `VERSION` → `0.5.0`
- Test: `just verify` + live smoke

- [ ] **Step 1: README.** CLI block gains `muster watch` line (`# follow the bus live — every message, task, wake and read as it happens`); the events paragraph in *Notifications & nudging* mentions watch and the subject column; gc paragraph mentions `--events-keep 720h` default.

- [ ] **Step 2: site/index.html.** In the tools section's plain-language chips / CLI mentions, add watch alongside events where the mailbox render is described (one sentence, defined terms, full sentences — match the page's copy standards; no new sections).

- [ ] **Step 3: VERSION** — `printf '0.5.0\n' > VERSION`.

- [ ] **Step 4: `just verify`; live smoke** — build to a temp path, run `muster watch --backlog 5` in one shell while `muster send`ing from another; confirm interleaved send→notify lines appear within an interval, Ctrl-C exits instantly.

- [ ] **Step 5: Commit** — `git commit -am "docs+version: muster watch ships as 0.5.0"`. PR `feat/watch → dev`, then promote dev → main (release v0.5.0 auto-cuts; sign darwin assets with `contrib/release-sign.sh v0.5.0`).
