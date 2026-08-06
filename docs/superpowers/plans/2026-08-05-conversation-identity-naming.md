# Conversation-as-Identity Naming — muster Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A conversation's user-set name (the transcript `custom-title` record) becomes the one durable name, projected onto the bus label and tmux option at SessionStart; recycled-tmux-ID ghosts can never be attributed to a live session.

**Architecture:** Spec at `docs/superpowers/specs/2026-08-05-conversation-identity-naming-design.md`. Three strands: (1) a projection path — SessionStart hook reads the transcript's custom title and pushes it as a manual label to both tmux and the bus; (2) incarnation hardening — `session_created = 0` never *attributes* a row to a live session (liveness, unread math, alias grouping, label writes), while `DepartStaleSiblings`' deliberate sparing of 0-rows from *reaping* is untouched; (3) a `--no-inject` mode on `muster label` so the dotfiles statusline can promote a Claude-side rename without re-typing `/rename` into the pane.

**Tech Stack:** Go (cgo-free), SQLite via modernc.org/sqlite, tmux via `internal/tmuxenv`'s swappable `Run` var. Tests: standard `go test -race`; humancli daemon tests use `startCLITestDaemon`/`registerViaDaemon`/`listAgentsForTest` helpers already in the package.

## Global Constraints

- `just verify` (gofmt, golangci-lint, `go test -race`, build) must pass before every commit.
- cgo-free: no new cgo dependencies (`CGO_ENABLED=0` build must keep working).
- `internal/daemon` and `internal/store` stay tmux-agnostic — they never call tmux; all tmux access goes through `internal/tmuxenv` (client-side) or the injected `wake.Notifier` (daemon-side).
- stdout is sacred in mcp mode; hooks write human-facing lines to the `out` writer they're given, never `fmt.Println`.
- The daemon never types into panes: `internal/nudge` is the only send-keys path. Nothing in this plan adds send-keys.
- Work in a worktree off origin/dev: `ROOT=$(git rev-parse --show-toplevel); git -C "$ROOT" worktree add "$ROOT/.worktrees/feat-conversation-identity" -b feat/conversation-identity origin/dev` — never develop on the primary clone.
- Go doc comments in this repo may render the empty-string literal as `""` styled quotes — house style; don't "fix" existing ones.
- **Hosted-backend coordination (spec §9):** every store-signature change here (`SessionUnread`, `SessionAliasLineage`, `SetSessionLabel`) must keep its semantics expressed in the method's doc comment, because `internal/dynamostore` (on the local `feat/hosted-backend` branch) reimplements from those comments. Do not import or touch dynamostore in this plan.

---

### Task 1: Socket-aware tmux option writes in tmuxenv

Hooks run env-stripped ($TMUX unset), so the existing ambient `SetSessionOption`/`RefreshClient` can't serve the SessionStart projection. Add explicit-socket variants next to them.

**Files:**
- Modify: `internal/tmuxenv/tmuxenv.go` (after `UnsetSessionOption`, ~line 213)
- Test: `internal/tmuxenv/tmuxenv_test.go`

**Interfaces:**
- Produces: `func SetSessionOptionOn(socket, target, name, value string) error` and `func RefreshClientOn(socket string) error` — Task 6 calls both with the whereami-resolved socket + session ID.

- [ ] **Step 1: Write the failing test**

Follow the package's existing `Run`-stub pattern (capture args, restore in cleanup):

```go
func TestSetSessionOptionOnUsesExplicitSocket(t *testing.T) {
	var calls [][]string
	prev := Run
	Run = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "", nil
	}
	t.Cleanup(func() { Run = prev })

	if err := SetSessionOptionOn("/tmp/proj-x", "$3", "@claude_task", "nfl-3"); err != nil {
		t.Fatalf("SetSessionOptionOn: %v", err)
	}
	if err := RefreshClientOn("/tmp/proj-x"); err != nil {
		t.Fatalf("RefreshClientOn: %v", err)
	}
	want := [][]string{
		{"-S", "/tmp/proj-x", "set-option", "-t", "$3", "@claude_task", "nfl-3"},
		{"-S", "/tmp/proj-x", "refresh-client", "-S"},
	}
	if len(calls) != 2 || !reflect.DeepEqual(calls[0], want[0]) || !reflect.DeepEqual(calls[1], want[1]) {
		t.Fatalf("unexpected tmux calls: %v", calls)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tmuxenv/ -run TestSetSessionOptionOnUsesExplicitSocket -v`
Expected: FAIL — `undefined: SetSessionOptionOn`

- [ ] **Step 3: Implement**

```go
// SetSessionOptionOn sets a tmux user option on an explicit socket + target
// session — the env-stripped counterpart of SetSessionOption, for callers
// (the SessionStart projection) that resolved their coordinates via
// CaptureFromAncestry rather than the ambient environment.
func SetSessionOptionOn(socket, target, name, value string) error {
	_, err := Run("-S", socket, "set-option", "-t", target, name, value)
	return err
}

// RefreshClientOn repaints the attached clients of the server on socket —
// the env-stripped counterpart of RefreshClient. Best-effort, like its
// ambient sibling.
func RefreshClientOn(socket string) error {
	_, err := Run("-S", socket, "refresh-client", "-S")
	return err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tmuxenv/ -run TestSetSessionOptionOn -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tmuxenv/
git commit -m "feat(tmuxenv): socket-aware option write + refresh for env-stripped hooks"
```

---

### Task 2: Transcript custom-title capture in harnessenv

The transcript records a user-set name as a structured line `{"type":"custom-title","customTitle":"nfl-3","sessionId":"…"}`, re-emitted through the file (verified live 2026-08-05; the last occurrence is current). Claude hook payloads carry `transcript_path`. harnessenv is the canonical harness-capture module, so both land here.

**Files:**
- Modify: `internal/harnessenv/harnessenv.go` (`Capture` struct, `FromHookPayload`; add `CustomTitle`)
- Test: `internal/harnessenv/harnessenv_test.go`

**Interfaces:**
- Produces: `Capture.TranscriptPath string` (populated by `FromHookPayload` only — there is no env fallback for it) and `func CustomTitle(transcriptPath string) string` (last record wins; "" on missing file, unreadable file, or no record). Task 6 consumes both.

- [ ] **Step 1: Write the failing tests**

```go
func TestFromHookPayloadCapturesTranscriptPath(t *testing.T) {
	c := FromHookPayload([]byte(`{"session_id":"u1","cwd":"/w","transcript_path":"/tmp/t.jsonl"}`))
	if c.TranscriptPath != "/tmp/t.jsonl" {
		t.Fatalf("TranscriptPath = %q, want /tmp/t.jsonl", c.TranscriptPath)
	}
}

func TestCustomTitleLastRecordWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	lines := []string{
		`{"type":"custom-title","customTitle":"old-name","sessionId":"u1"}`,
		`{"type":"user","message":{"role":"user","content":"body mentioning custom-title"}}`,
		`{"type":"custom-title","customTitle":"nfl-3","sessionId":"u1"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := CustomTitle(path); got != "nfl-3" {
		t.Fatalf("CustomTitle = %q, want nfl-3", got)
	}
}

func TestCustomTitleAbsentOrUnreadable(t *testing.T) {
	if got := CustomTitle(""); got != "" {
		t.Fatalf("empty path: got %q", got)
	}
	if got := CustomTitle(filepath.Join(t.TempDir(), "missing.jsonl")); got != "" {
		t.Fatalf("missing file: got %q", got)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "no-title.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := CustomTitle(path); got != "" {
		t.Fatalf("no record: got %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/harnessenv/ -v`
Expected: FAIL — `unknown field TranscriptPath` / `undefined: CustomTitle`

- [ ] **Step 3: Implement**

Add to `Capture`:

```go
	// TranscriptPath is the harness conversation's transcript file, when the
	// hook payload provided one (Claude Code sends transcript_path in every
	// hook payload; Codex sends none). Payload-only — the process
	// environment has no equivalent, so FromEnv leaves it empty.
	TranscriptPath string
```

In `FromHookPayload`, extend the inline struct and copy the field:

```go
	var p struct {
		SessionID      string `json:"session_id"`
		CWD            string `json:"cwd"`
		TranscriptPath string `json:"transcript_path"`
	}
	// ... existing unmarshal + fallbacks ...
	if p.TranscriptPath != "" {
		c.TranscriptPath = p.TranscriptPath
	}
```

New function (new imports: `bufio`):

```go
// CustomTitle returns the conversation's user-set name: the customTitle of
// the LAST {"type":"custom-title"} record in the transcript at path. The
// record is written by an explicit naming gesture (/rename, `claude --name`,
// or muster's own prefix-T injection) and re-emitted through the file, so
// its presence is proof of intent — the signal the statusline's merged
// session_name field cannot provide (spec §2, verified 2026-08-05). Returns
// "" for an empty path, unreadable file, or a transcript with no record:
// callers treat "" as "no user-set name", never as an error — a hook must
// not fail on transcript shape.
func CustomTitle(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	var title string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // transcript lines can be huge (tool results)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"custom-title"`)) {
			continue // cheap pre-filter; the unmarshal below is the authority
		}
		var rec struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.Type == "custom-title" && rec.CustomTitle != "" {
			title = rec.CustomTitle
		}
	}
	return title
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/harnessenv/ -v`
Expected: PASS (all, including pre-existing)

- [ ] **Step 5: Commit**

```bash
git add internal/harnessenv/
git commit -m "feat(harnessenv): transcript_path capture + CustomTitle transcript reader"
```

---

### Task 3: `session_created = 0` never reads alive

`tmuxenv.IsSessionAlive` currently spares legacy rows: `created == 0 || out == …`. That sparing is what let a tombstoned pre-v0.8.0 row ride a recycled tmux `$1` into a live session's badges (the 2026-08-05 nfl-3 incident). Flip it: a row that cannot prove its incarnation never reads alive. Every liveness consumer (roster live-dot, resume reclaim gate, SessionEnd sweep, label sync, station fetch, gc) flows through this one function, so the flip is the whole change — plus a pin that `muster gc` now retires such rows (this IS the spec §5.1 legacy sweep: tombstones are revivable-with-history, so a mis-swept row self-heals on its next register).

**Files:**
- Modify: `internal/tmuxenv/tmuxenv.go:96-105` (`IsSessionAlive`)
- Test: `internal/tmuxenv/tmuxenv_test.go`, `internal/humancli/identity_test.go`

**Interfaces:**
- Consumes: nothing new. Signature unchanged: `IsSessionAlive(socket, sessionID string, created int64) bool`.
- Produces: new semantics — `created == 0` returns false even when the session exists. Tasks 4–6 assume these semantics.

- [ ] **Step 1: Write the failing tests**

In `internal/tmuxenv/tmuxenv_test.go` (Run-stub pattern as in Task 1):

```go
func TestIsSessionAliveZeroCreatedNeverMatches(t *testing.T) {
	prev := Run
	Run = func(args ...string) (string, error) { return "1784000000", nil } // session exists
	t.Cleanup(func() { Run = prev })

	if IsSessionAlive("/tmp/s", "$1", 0) {
		t.Fatal("created=0 must never attribute a live session (spec §5.1: unprovable incarnation)")
	}
	if !IsSessionAlive("/tmp/s", "$1", 1784000000) {
		t.Fatal("matching non-zero created must read alive")
	}
	if IsSessionAlive("/tmp/s", "$1", 1700000000) {
		t.Fatal("mismatched created must read dead")
	}
}
```

In `internal/humancli/identity_test.go`, next to `TestGCTombstonesOnlyDeadAgents` (reuse its harness exactly — `startCLITestDaemon`, `registerViaDaemon`, `listAgentsForTest`, the `args[6] == "#{session_created}"` probe stub):

```go
// TestGCSweepsLegacyZeroCreatedRows pins the spec §5.1 legacy sweep: a row
// that cannot prove its incarnation (session_created = 0, the pre-v0.8.0
// population) is tombstoned by plain `muster gc` EVEN IF a session with its
// recycled ID is currently live — attribution requires proof. The tombstone
// keeps history and revives on the row's next register, so a genuinely live
// pre-upgrade session self-heals.
func TestGCSweepsLegacyZeroCreatedRows(t *testing.T) {
	sock := startCLITestDaemon(t)
	registerViaDaemon(t, sock, "legacy", "/s", "$1") // registerViaDaemon stores session_created=0
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if len(args) >= 7 && args[6] == "#{session_created}" {
			return "1784000000", nil // the recycled $1 IS live — sweep must still fire
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdGC(nil, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "tombstoned legacy") {
		t.Fatalf("expected legacy row tombstoned, got %q", buf.String())
	}
}
```

(If `registerViaDaemon` in this package takes a created argument, pass `0` explicitly; read its definition first.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tmuxenv/ ./internal/humancli/ -run 'ZeroCreated|SweepsLegacy' -v`
Expected: both FAIL (created=0 currently reads alive; gc currently spares the row)

- [ ] **Step 3: Implement the flip**

Replace `IsSessionAlive`'s return and rewrite its doc comment to record the new rule and its date:

```go
// IsSessionAlive reports whether the tmux session a registration was
// captured from still exists on the socket AS THE SAME INCARNATION. tmux
// recycles session IDs across server restarts, so existence alone proves
// nothing: the stored session_created must equal the live session's. Rows
// with created == 0 (pre-v0.8.0 registrations that never captured it) can
// never prove their incarnation and therefore NEVER read alive — the
// 2026-08-05 rule (conversation-identity spec §5.1) replacing the old
// spare-legacy fallback, after a tombstoned 0-row rode a recycled $1 into a
// live session's badge. Reaping stays separate: DepartStaleSiblings still
// deliberately spares 0-rows (attribution and tombstoning are distinct
// decisions); operator-run `muster gc` is the sweep that retires them, and
// a tombstone revives with history intact on its next register.
func IsSessionAlive(socket, sessionID string, created int64) bool {
	if socket == "" || sessionID == "" || created == 0 {
		return false
	}
	out := query(socket, sessionID, "#{session_created}")
	if out == "" {
		return false // session gone (or tmux unreachable): dead either way
	}
	return out == strconv.FormatInt(created, 10)
}
```

Then make gc's tombstone line distinguish the legacy case so the new test can assert it. In `cmdGC` (internal/humancli/identity.go, the non-purge branch), replace the single Fprintf with:

```go
			reason := "dead session"
			if a.SessionCreated == 0 {
				reason = "legacy row: no session_created, unprovable incarnation"
			}
			if _, err := fmt.Fprintf(out, "tombstoned %s (%s)\n", a.Alias, reason); err != nil {
				return err
			}
```

and in the new test assert `strings.Contains(buf.String(), "tombstoned legacy (legacy row")`. (Adjust the Step 1 assertion to this exact string.)

- [ ] **Step 4: Run the full affected packages**

Run: `go test ./internal/tmuxenv/ ./internal/humancli/ ./internal/render/ -race`
Expected: PASS. If any pre-existing test pinned the old `created == 0 → alive` fallback, update that test's expectation and cite spec §5.1 in a comment — the old behavior is deliberately gone, not accidentally broken.

- [ ] **Step 5: Commit**

```bash
git add internal/tmuxenv/ internal/humancli/
git commit -m "fix(tmuxenv): session_created=0 never attributes a live session; gc names the legacy sweep"
```

---

### Task 4: Incarnation dimension on session-level queries (SessionUnread, SessionAliasLineage)

The store's session-level queries key on the bare `(socket_path, session_id)` tuple, so a legacy row on a recycled ID joined the live session's unread math and alias list (the "two aliases, two unread counts" confusion). Add a `sessionCreated` dimension: for tmux tuples (non-empty socket) only rows whose `session_created` matches seed the lineage walk; the walk itself still follows `superseded_by` to old tuples (mail follows the name — unchanged). Paneless tuples (empty socket) skip the check — harness UUIDs are never recycled.

**Files:**
- Modify: `internal/store/agents.go` (`SessionUnread` ~line 334, `SessionAliasLineage` ~line 357)
- Modify: `internal/daemon/daemon.go` (storeAPI interface line ~50; `setSessionBadge` ~288; `reconcileBadge` ~311 and its call sites at ~254, 256, 748, 777, 818; `session_unread` op ~659; `session_aliases` op ~637; the direct `SessionUnread` call at ~819)
- Modify: `internal/humancli/hook.go` (`sessionAliasesForHook` line 583, `sessionUnreadForHook` line 599, and their callers in the Stop/SessionEnd paths)
- Test: `internal/store/agents_test.go`, `internal/daemon/daemon_test.go`

**Interfaces:**
- Consumes: Task 3's semantics (attribution requires proof) as the rule being extended into tmux-agnostic store code.
- Produces:
  - `func (s *Store) SessionUnread(socketPath, sessionID string, sessionCreated int64) (total, action int, err error)`
  - `func (s *Store) SessionAliasLineage(socketPath, sessionID string, sessionCreated int64) ([]string, error)`
  - daemon ops `session_unread` / `session_aliases` accept a `session_created` arg (i64; 0 = paneless caller)
  - humancli helpers `sessionAliasesForHook(socketPath, sessionID string, sessionCreated int64) []string` and `sessionUnreadForHook(socketPath, sessionID string, sessionCreated int64) (total, action int, ok bool)`
  - Task 6 and the dotfiles plan rely on the op accepting `session_created`.

- [ ] **Step 1: Write the failing store test**

In `internal/store/agents_test.go`:

```go
// TestSessionUnreadRequiresIncarnationMatch pins spec §5.1 at the store
// layer: a tmux-tuple row only seeds the session's unread/alias math when
// its session_created matches the caller's live value; 0 never matches.
// Paneless tuples (empty socket) are exempt — harness UUIDs don't recycle.
func TestSessionUnreadRequiresIncarnationMatch(t *testing.T) {
	s := newTestStore(t)
	// current incarnation
	mustRegister(t, s, store.Agent{Alias: "current", SocketPath: "/s", SessionID: "$1", SessionCreated: 200})
	// legacy ghost on the recycled ID
	mustRegister(t, s, store.Agent{Alias: "ghost", SocketPath: "/s", SessionID: "$1", SessionCreated: 0})
	// mail for each
	mustSend(t, s, "peer", "current", "for the live one")
	mustSend(t, s, "peer", "ghost", "for the ghost")

	total, _, err := s.SessionUnread("/s", "$1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("live incarnation must see ONLY its own unread, got %d", total)
	}
	aliases, err := s.SessionAliasLineage("/s", "$1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 1 || aliases[0] != "current" {
		t.Fatalf("lineage must not include the ghost, got %v", aliases)
	}
	// paneless: created is irrelevant
	mustRegister(t, s, store.Agent{Alias: "bg", SocketPath: "", SessionID: "uuid-9", SessionCreated: 0})
	mustSend(t, s, "peer", "bg", "paneless mail")
	total, _, err = s.SessionUnread("", "uuid-9", 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("paneless tuple must be exempt from the incarnation check, got %d", total)
	}
}
```

Use the package's existing register/send helpers — read a neighboring `SessionUnread` test first and mirror its setup helpers exactly (they exist; do not invent `mustRegister`/`mustSend` if the file already names them differently — substitute the file's real helpers, keeping the scenario identical).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/store/ -run IncarnationMatch -v`
Expected: FAIL — too many arguments to `SessionUnread` (compile error proves the signature work is real)

- [ ] **Step 3: Implement the store change**

In both queries, the change is the base case of the `WITH RECURSIVE sess` CTE only (the recursive step is untouched — lineage rows keep their old tuples on purpose). For `SessionUnread`:

```sql
WITH RECURSIVE sess AS (
  SELECT alias, last_read_entry_id, superseded_by FROM agents
  WHERE socket_path = ?1 AND session_id = ?2 AND ?2 != ''
    AND (?1 = '' OR (session_created = ?3 AND ?3 != 0))
  UNION
  SELECT a.alias, a.last_read_entry_id, a.superseded_by
  FROM agents a JOIN sess ON a.superseded_by = sess.alias
)
```

with `socketPath, sessionID, sessionCreated` bound. Apply the identical `AND (?1 = '' OR (session_created = ?3 AND ?3 != 0))` clause to `SessionAliasLineage`'s base case. Extend both doc comments with one paragraph (this is the contract dynamostore reimplements from — spec §9):

```
// sessionCreated scopes the BASE case to one tmux-session incarnation:
// with a non-empty socketPath, only rows whose session_created equals it
// seed the walk, and 0 seeds nothing (attribution requires proof — spec
// §5.1, 2026-08-05). An empty socketPath (the paneless tuple) skips the
// check: harness UUIDs are never recycled. The recursive lineage step is
// deliberately unscoped — superseded rows sit on old tuples forever.
```

- [ ] **Step 4: Thread the parameter through the daemon**

All compile errors from the signature change are the worklist; the value to pass at each site:

- storeAPI interface (daemon.go:50): add `sessionCreated int64` to `SessionUnread`.
- `setSessionBadge(socketPath, sessionID string, sessionCreated int64)` and `reconcileBadge(socketPath, sessionID string, sessionCreated int64)`: every existing caller has an agent row or parsed args in scope — pass `old.SessionCreated` / `newAgent.SessionCreated` / `ag.SessionCreated` respectively (sites ~254, 256, 748, 777, 818, 819).
- `session_unread` op (~659) and `session_aliases` op (~637): read `i64(a, "session_created")` and pass through. Absent arg → 0 → a tmux caller gets zeros (correct: it supplied no proof) and a paneless caller is unaffected.

- [ ] **Step 5: Thread through humancli hook helpers**

`sessionAliasesForHook` / `sessionUnreadForHook` gain `sessionCreated int64`, forwarded as `"session_created"` in their `callData` args. Their callers in the Stop/SessionEnd paths all hold a `tmuxenv.Capture` (pass `c.SessionCreated`) or a paneless harness capture (pass `0`). Fix every compile error the same way; no call site lacks the value.

- [ ] **Step 6: Daemon-level regression test**

In `internal/daemon/daemon_test.go`, using the file's existing dispatch-test scaffolding (read one `session_unread`/badge test first and reuse its store + daemon constructor and its request helper — the package has both):

```go
// TestSessionUnreadOpRequiresCreated pins the op contract: a tmux tuple
// queried without session_created (or with a stale one) gets zeros — the
// recycled-ID ghost can no longer inflate a live session's badge.
func TestSessionUnreadOpRequiresCreated(t *testing.T) {
	d, s := newTestDaemon(t) // substitute the package's real constructor helper
	mustRegister(t, s, store.Agent{Alias: "current", SocketPath: "/s", SessionID: "$1", SessionCreated: 200})
	mustRegister(t, s, store.Agent{Alias: "ghost", SocketPath: "/s", SessionID: "$1", SessionCreated: 0})
	mustSend(t, s, "peer", "current", "live mail")
	mustSend(t, s, "peer", "ghost", "ghost mail")

	resp := dispatchForTest(t, d, "session_unread", map[string]any{
		"socket_path": "/s", "session_id": "$1", "session_created": int64(200),
	})
	if total := i64FromResp(t, resp, "total"); total != 1 {
		t.Fatalf("with proof: total = %d, want 1 (only the live incarnation's mail)", total)
	}
	resp = dispatchForTest(t, d, "session_unread", map[string]any{
		"socket_path": "/s", "session_id": "$1", // no session_created: no proof
	})
	if total := i64FromResp(t, resp, "total"); total != 0 {
		t.Fatalf("without proof: total = %d, want 0", total)
	}
}
```

As in the store test, `newTestDaemon`/`mustRegister`/`mustSend`/`dispatchForTest`/`i64FromResp` stand in for whatever this package's existing tests actually name their helpers — substitute the real names, keep the scenario byte-for-byte.

- [ ] **Step 7: Run the full gate**

Run: `just verify`
Expected: PASS. Any other test broken by the signature is part of this task — fix it by passing the value its scenario implies (a live capture's created, a row's stored created, or 0 for paneless).

- [ ] **Step 8: Commit**

```bash
git add internal/store/ internal/daemon/ internal/humancli/
git commit -m "fix(store,daemon): session-level queries scope to one tmux incarnation"
```

---

### Task 5: Incarnation on set_label + `muster label --no-inject`

Two small label-path changes that Task 6 and the dotfiles statusline need: (a) `SetSessionLabel` must not stamp a label onto rows of a different (or unprovable) incarnation; (b) `muster label` needs a mode that skips the `/rename` injection, because the statusline promotes a name that ALREADY came from `/rename` — re-typing it would loop text into a live pane.

**Files:**
- Modify: `internal/store/agents.go:123-136` (`SetSessionLabel`)
- Modify: `internal/daemon/daemon.go:751-758` (`set_label` op)
- Modify: `internal/humancli/label.go` (flag, `cmdLabel`, `syncLabelToBus`)
- Test: `internal/store/agents_test.go`, `internal/humancli/label_test.go`

**Interfaces:**
- Consumes: `tmuxenv.Capture.SessionCreated` (already exists).
- Produces:
  - `func (s *Store) SetSessionLabel(socketPath, sessionID string, sessionCreated int64, label string, manual bool) (int64, error)`
  - `set_label` op accepts `session_created` (i64)
  - `muster label --no-inject <name>` — everything `muster label` does except `syncAgentName`
  - `syncLabelToBus(out io.Writer, label string, manual bool, socket, sessionID string, sessionCreated int64)`
  - Task 6 calls the op directly with these args; the dotfiles plan invokes `muster label --no-inject`.

- [ ] **Step 1: Write the failing store test**

```go
// TestSetSessionLabelScopedToIncarnation: a label write only lands on rows
// of the proven incarnation — never on a recycled-ID ghost (created
// mismatch) or an unprovable legacy row (created 0).
func TestSetSessionLabelScopedToIncarnation(t *testing.T) {
	s := newTestStore(t)
	mustRegister(t, s, store.Agent{Alias: "current", SocketPath: "/s", SessionID: "$1", SessionCreated: 200})
	mustRegister(t, s, store.Agent{Alias: "ghost", SocketPath: "/s", SessionID: "$1", SessionCreated: 0})

	n, err := s.SetSessionLabel("/s", "$1", 200, "nfl-3", true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected exactly the current row labeled, got %d", n)
	}
	ghost := mustGetAgent(t, s, "ghost")
	if ghost.Label != "" || ghost.LabelManual {
		t.Fatalf("ghost must be untouched, got label=%q manual=%v", ghost.Label, ghost.LabelManual)
	}
}
```

(Substitute the file's real register/get helpers, as in Task 4.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/store/ -run SetSessionLabelScoped -v`
Expected: FAIL — wrong argument count (compile error)

- [ ] **Step 3: Implement store + op**

`SetSessionLabel` body (same clause shape as Task 4; doc comment gains the same incarnation paragraph):

```go
func (s *Store) SetSessionLabel(socketPath, sessionID string, sessionCreated int64, label string, manual bool) (int64, error) {
	if socketPath == "" || sessionID == "" {
		return 0, nil
	}
	res, err := s.db.Exec(`
UPDATE agents SET label=?, label_manual=?
WHERE socket_path=? AND session_id=? AND departed=0
  AND session_created=? AND session_created != 0`,
		label, manual, socketPath, sessionID, sessionCreated)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
```

`set_label` op: pass `i64(a, "session_created")` in position. (An old caller omitting it now updates 0 rows and the CLI's existing "bus label sync failed / refreshes on next register" warning path already communicates a 0-update as silence — acceptable single-binary skew.)

- [ ] **Step 4: Write the failing CLI test for --no-inject**

In `internal/humancli/label_test.go`, mirror `TestLabelSetsOptionsAndRefreshes` (Run-stub, `t.Setenv("TMUX", …)`) but invoke `cmdLabel([]string{"--no-inject", "backend"}, &buf)` and assert: the two `set-option` calls and `refresh-client` happen exactly as in the plain test, and **no** captured call contains `"send-keys"` (that is `syncAgentName`'s injection fingerprint — it types via the nudge path). Also assert the existing plain-`cmdLabel` test still passes unchanged.

- [ ] **Step 5: Implement the flag**

In `newLabelFlagsWithVals` add `noInject := fs.Bool("no-inject", false, "skip typing /rename into the live agent pane (for callers whose name ALREADY came from the harness side, e.g. the statusline promoting a /rename)")`, return it, and in `cmdLabel` gate the `syncAgentName(out, name, socket, sessionID)` call on `!*noInject`. `syncLabelToBus` gains `sessionCreated int64` forwarded as `"session_created"`; `cmdLabel` passes the ambient capture's created (extend the `tmuxenv.SocketFromEnv()` / `CurrentSessionID()` block with the capture that carries `SessionCreated` — `tmuxenv` already exposes it on `Capture`; use the same capture call `hookCapture` uses ambient-side, i.e. `tmuxenv.CaptureEnv()`). Update `label`'s registry synopsis/help text with the new flag.

- [ ] **Step 6: Run both packages**

Run: `go test ./internal/store/ ./internal/humancli/ ./internal/daemon/ -race`
Expected: PASS (fix any call-site compile errors by passing the capture's created, as in Task 4)

- [ ] **Step 7: Commit**

```bash
git add internal/store/ internal/daemon/ internal/humancli/
git commit -m "feat(label): incarnation-scoped set_label + --no-inject for harness-originated names"
```

---

### Task 6: The SessionStart projection

The payoff: at SessionStart (fresh AND resume, pane sessions only), after registration/reclaim, read the conversation's custom title and project it — tmux option pair via Task 1, bus label via the `set_label` op with Task 5's signature — then print one line into session context. A same-project manual-label collision elsewhere is warned about, never stolen from (the write is tuple-scoped by construction; the warning is for the human).

**Files:**
- Modify: `internal/humancli/hook.go` (`cmdHook` SessionStart branch ~lines 42-57; new `hookProjectName`)
- Test: `internal/humancli/hook_test.go`

**Interfaces:**
- Consumes: `harnessenv.CustomTitle`, `Capture.TranscriptPath` (Task 2); `tmuxenv.SetSessionOptionOn`, `RefreshClientOn` (Task 1); `set_label` op with `session_created` (Task 5); `tmuxenv.LabelOption()` (exists).
- Produces: `func hookProjectName(c tmuxenv.Capture, title string, out io.Writer)` — internal; the dotfiles plan's operator-acceptance step relies on the printed line `muster: session name %q → tmux label + bus (manual)`.

- [ ] **Step 1: Write the failing tests**

In `internal/humancli/hook_test.go`, following the file's existing SessionStart test setup (daemon via `startCLITestDaemon`-equivalent for hooks, Run-stub for tmux — read one existing `hookSessionStart`/`hookRegisterPane` test first and reuse its scaffolding):

```go
// TestHookProjectNameProjectsCustomTitle: a SessionStart whose transcript
// carries a custom-title lands the name on (a) the tmux option pair via
// socket-aware writes and (b) the bus label, manual, on this incarnation.
func TestHookProjectNameProjectsCustomTitle(t *testing.T) {
	// fixture transcript
	dir := t.TempDir()
	tp := filepath.Join(dir, "t.jsonl")
	os.WriteFile(tp, []byte(`{"type":"custom-title","customTitle":"nfl-3","sessionId":"u1"}`+"\n"), 0o600)

	// capture tmux writes
	var calls [][]string
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	sock := startCLITestDaemon(t)
	registerViaDaemonCreated(t, sock, "muster-9", "/s", "$1", 200) // see note below

	var buf bytes.Buffer
	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", SessionCreated: 200, PaneID: "%5"}
	hookProjectName(c, harnessenv.CustomTitle(tp), &buf)

	// tmux half: option + manual flag + refresh, all socket-aware
	wantOpt := []string{"-S", "/s", "set-option", "-t", "$1", tmuxenv.LabelOption(), "nfl-3"}
	wantMan := []string{"-S", "/s", "set-option", "-t", "$1", tmuxenv.LabelOption() + "_manual", "1"}
	if !containsCall(calls, wantOpt) || !containsCall(calls, wantMan) {
		t.Fatalf("missing socket-aware option writes, got %v", calls)
	}
	// bus half
	ag := getAgentForTest(t, sock, "muster-9")
	if ag.Label != "nfl-3" || !ag.LabelManual {
		t.Fatalf("bus label = (%q, manual=%v), want (nfl-3, true)", ag.Label, ag.LabelManual)
	}
	if !strings.Contains(buf.String(), `session name "nfl-3"`) {
		t.Fatalf("expected context line, got %q", buf.String())
	}
}

// TestHookProjectNameNoTitleNoOp: no custom-title → no writes, no output
// (spec: never demote, never clear; a fresh unnamed session is untouched).
func TestHookProjectNameNoTitleNoOp(t *testing.T) {
	var calls [][]string
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", SessionCreated: 200, PaneID: "%5"}
	hookProjectName(c, "", &buf)
	for _, call := range calls {
		for _, arg := range call {
			if arg == "set-option" {
				t.Fatalf("empty title must write nothing, got %v", calls)
			}
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("empty title must print nothing, got %q", buf.String())
	}
}

// TestHookProjectNameWarnsOnCollision: another live agent in the SAME
// project already holds the name as a manual label → the projection still
// writes its own tuple (tuple-scoped, it cannot steal) but prints a warning
// naming the holder, so the resolver's coming ambiguity error isn't a
// surprise.
func TestHookProjectNameWarnsOnCollision(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "t.jsonl")
	os.WriteFile(tp, []byte(`{"type":"custom-title","customTitle":"nfl-3","sessionId":"u1"}`+"\n"), 0o600)

	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) { return "", nil }
	t.Cleanup(func() { tmuxenv.Run = prev })

	sock := startCLITestDaemon(t)
	// mine, and a same-project holder on a DIFFERENT tuple
	registerViaDaemonCreated(t, sock, "muster-9", "/s", "$1", 200)
	registerViaDaemonCreated(t, sock, "holder", "/s", "$2", 300)
	// both rows must carry the same project for the warning to fire: use
	// whatever mechanism registerViaDaemonCreated exposes (its register args
	// include project); then pin the holder's label via the set_label op:
	callDataForTest(t, sock, "set_label", map[string]any{
		"socket_path": "/s", "session_id": "$2", "session_created": int64(300),
		"label": "nfl-3", "label_manual": true,
	})

	var buf bytes.Buffer
	c := tmuxenv.Capture{SocketPath: "/s", SessionID: "$1", SessionCreated: 200, PaneID: "%5"}
	hookProjectName(c, harnessenv.CustomTitle(tp), &buf)
	if !strings.Contains(buf.String(), "also held by") || !strings.Contains(buf.String(), "holder") {
		t.Fatalf("expected collision warning naming the holder, got %q", buf.String())
	}
}
```

(`callDataForTest` — a helper that dials the test daemon's socket and issues
one op — may already exist under another name in this package's daemon-backed
tests; reuse the existing one. Same for the register helper: if the package's
existing `registerViaDaemon` can't carry `session_created`/`project`, extend
it or add `registerViaDaemonCreated` beside it rather than duplicating dial
logic.)
```

Write `containsCall`, `registerViaDaemonCreated`, and `getAgentForTest` as tiny local helpers if the package lacks them (check first — `listAgentsForTest` exists and can back `getAgentForTest`; a created-carrying register helper may already exist in become/hook tests).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/humancli/ -run HookProjectName -v`
Expected: FAIL — `undefined: hookProjectName`

- [ ] **Step 3: Implement hookProjectName**

In `internal/humancli/hook.go`:

```go
// hookProjectName projects the conversation's user-set name (the transcript
// custom-title — see harnessenv.CustomTitle) onto every naming surface at
// SessionStart: the tmux option pair (socket-aware — hooks run
// env-stripped) and the stored bus label (manual, incarnation-scoped via
// set_label). This is the spec's conversation-as-identity payoff: resume
// nfl-3 anywhere and "send nfl-3" routes with zero gestures. Best-effort
// throughout — a failed write degrades to pre-projection behavior, never a
// wrong name. Empty title = no-op: never demote, never clear. A same-project
// manual holder elsewhere is warned about, never overwritten: the set_label
// write is tuple-scoped by construction, so stealing is impossible and the
// resolver's ambiguity error stays the enforcement (spec §4).
func hookProjectName(c tmuxenv.Capture, title string, out io.Writer) {
	if title == "" || c.SocketPath == "" || c.SessionID == "" {
		return
	}
	opt := tmuxenv.LabelOption()
	if err := tmuxenv.SetSessionOptionOn(c.SocketPath, c.SessionID, opt, title); err != nil {
		return // tmux unreachable: leave every surface as-is
	}
	_ = tmuxenv.SetSessionOptionOn(c.SocketPath, c.SessionID, opt+"_manual", "1")
	_ = tmuxenv.RefreshClientOn(c.SocketPath)
	if _, err := callData("set_label", map[string]any{
		"socket_path": c.SocketPath, "session_id": c.SessionID,
		"session_created": c.SessionCreated,
		"label":           title, "label_manual": true,
	}); err != nil {
		fmt.Fprintf(out, "muster: session name %q set in tmux; bus sync failed (%v) — refreshes on next register\n", title, err)
		return
	}
	fmt.Fprintf(out, "muster: session name %q → tmux label + bus (manual)\n", title)
	warnLabelCollision(title, c, out)
}

// warnLabelCollision surfaces (never resolves) a same-project manual-label
// holder on a different session: the human decides who renames.
func warnLabelCollision(title string, c tmuxenv.Capture, out io.Writer) {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return
	}
	var agents []agentRow
	if json.Unmarshal(raw, &agents) != nil {
		return
	}
	var myProject string
	for _, a := range agents {
		if a.SocketPath == c.SocketPath && a.SessionID == c.SessionID && a.SessionCreated == c.SessionCreated && !a.Departed {
			myProject = a.Project
			break
		}
	}
	for _, a := range agents {
		if a.Departed || !a.LabelManual || a.Label != title || a.Project != myProject {
			continue
		}
		if a.SocketPath == c.SocketPath && a.SessionID == c.SessionID {
			continue // my own tuple
		}
		fmt.Fprintf(out, "muster: note — label %q is also held by %s; sends to the bare label will error as ambiguous until one of you renames\n", title, a.Alias)
	}
}
```

(`agentRow` already exists in this package — cmdGC unmarshals into it; confirm its field set covers SocketPath/SessionID/SessionCreated/Project/Label/LabelManual/Departed and extend its struct tags if a field is missing, matching `internal/humancli/humancli.go`'s AgentView json tags.)

- [ ] **Step 4: Wire into cmdHook's SessionStart branch**

Restructure the branch so BOTH the resume and fresh paths reach the projection (currently the resume path `return nil`s early):

```go
	case "SessionStart":
		c := hookCapture()
		h := harnessenv.FromHookPayload(payload)
		var start struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(payload, &start)
		if c.SocketPath != "" && c.PaneID != "" {
			handled := false
			if start.Source == "resume" {
				handled = hookSessionStartResume(c, h, model, out)
			}
			if !handled && hookMayClaimIdentity(c) {
				hookRegisterPane(c, h, model)
			}
			hookProjectName(c, harnessenv.CustomTitle(h.TranscriptPath), out)
		} else {
			hookSessionStartPaneless(h, model)
		}
```

- [ ] **Step 5: Run the hook suite**

Run: `go test ./internal/humancli/ -race`
Expected: PASS, including all pre-existing SessionStart/resume tests (the restructure must not change their observable behavior — if one fails, the restructure is wrong, not the test)

- [ ] **Step 6: Full gate + commit**

Run: `just verify`
Expected: PASS

```bash
git add internal/humancli/
git commit -m "feat(hook): SessionStart projects the conversation's custom-title onto tmux + bus"
```

---

### Task 7: Documentation — the naming contract

**Files:**
- Modify: `CLAUDE.md` (new short section after "Architecture")
- Modify: `.claude/skills/muster-coordination/SKILL.md` (addressing section)
- Modify: `README.md` (labels/addressing section, wherever labels are currently explained)

**Interfaces:** none — prose only.

- [ ] **Step 1: CLAUDE.md contract section**

Add (adjusting placement to read naturally after the Architecture block):

```markdown
## The naming contract

The tmux option pair `@claude_task` / `@claude_task_manual` is the neutral
meeting point between muster and the operator's dotfiles (spec:
docs/superpowers/specs/2026-08-05-conversation-identity-naming-design.md).
Intentional gestures (prefix T, `muster label`, the SessionStart projection
of a transcript custom-title) set both; automatic syncs write only the label
and defer to the flag; readers trust the pair. The conversation's transcript
custom-title is the one durable name — tmux and the bus are projections.
Attribution requires proof: `session_created = 0` never matches a live
session (tmuxenv.IsSessionAlive), while DepartStaleSiblings still spares
those rows from reaping — attribution and tombstoning are distinct decisions.
```

- [ ] **Step 2: Skill + README**

In `.claude/skills/muster-coordination/SKILL.md`, find the addressing/labels passage and add two sentences: a resumed conversation re-asserts its custom-title as its manual label automatically at SessionStart, so peers address the *name* (`proj:name` or bare in-project) without any operator gesture; `/rename` inside Claude is now a first-class naming gesture (the statusline promotes it), not display-only. In `README.md`, update the labels explanation to match — replace any claim that only `muster label`/prefix T makes a label addressable.

- [ ] **Step 3: Verify + commit**

Run: `just verify` (gofmt/lint unaffected by prose, but the gate is the rule)

```bash
git add CLAUDE.md .claude/skills/muster-coordination/SKILL.md README.md
git commit -m "docs: the naming contract — custom-title is the durable name"
```

---

## Companion plan

The dotfiles half (statusline promotion via `muster label --no-inject`, prefix-T shim muster-less fallback) is a separate plan in the dotfiles repo: `~/dotfiles/docs/superpowers/plans/2026-08-05-naming-contract.md`. It depends on Task 5's `--no-inject` flag being in the installed `muster` binary — build/install from this branch (or wait for the release) before executing that plan's statusline task.

## Operator acceptance (after both plans, the merge gate)

Restart the tmux server (or laptop), resume a custom-titled conversation in a fresh tmux session, and verify with zero gestures: the tab title is the name, `muster agents` shows it as a manual label on the reclaimed alias, and a peer's `muster send <name>` routes to it. Then prefix-T a session and confirm `/rename` still lands in the pane (the non-regression the operator called out).
