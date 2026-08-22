# One Conversation, One Identity — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A conversation has exactly one live roster row (keyed by transcript path, then pane tuple), inbox reads move the watermark only for an alias the caller owns, and `become` can take a departed name back.

**Architecture:** `store.API` gains `TranscriptPath`, `FindConversation`, `StampHarness`, a `Become` that deletes a departed target, and a lineage base case that excludes non-chain tombstones — implemented in SQLite and DynamoDB with conformance tests. The daemon's `register_agent` adopts an existing conversation row instead of inserting a sibling; `get_inbox` takes a caller proof and peeks when unowned. Hooks reclaim by transcript first; MCP and CLI pass the proof. `tmuxenv` canonicalizes socket paths.

**Tech Stack:** Go 1.2x, `modernc.org/sqlite` (cgo-free), DynamoDB via `aws-sdk-go-v2` (lambda-tagged packages only), `just verify` / `just verify-dynamo`.

**Spec:** `docs/superpowers/specs/2026-08-21-one-conversation-one-identity-design.md`

## Global Constraints

- `just verify` must pass before every commit; `just verify-dynamo` (needs Docker: `just dynamo-up` if the recipe exists, see `justfile`) after any `internal/dynamostore` change.
- No AWS import outside `internal/dynamostore`, `internal/lambdamode`, `internal/deploy`, `cmd/muster-deploy`.
- `internal/daemon` and `internal/store` stay tmux-agnostic (stored data only; `filepath.EvalSymlinks` is filesystem, not tmux, and is allowed in the SQLite migration).
- stdout is sacred in `mcp` mode: diagnostics to stderr.
- Prose in docs: one line per paragraph, no hand-wrapping.
- Test helper for unix-socket temp paths on macOS: `mustertest.ShortHome()`.
- Every `store.API` change lands in BOTH backends plus a `storetest` conformance entry in the table at `internal/storetest/conformance.go` (~line 95–130).

---

### Task 1: `TranscriptPath` on the row + `StampHarness`

**Files:**
- Modify: `internal/store/models.go` (Agent struct, after `HarnessSessionID`)
- Modify: `internal/store/store.go` (`migrate` alters list)
- Modify: `internal/store/agents.go` (RegisterAgent/ListAgents/GetAgent/Become column lists; `SetHarnessSessionID` → `StampHarness`)
- Modify: `internal/store/api.go:111`
- Modify: `internal/dynamostore/agents.go` (RegisterAgent `set` map, `itemToAgent`, `SetHarnessSessionID` → `StampHarness`, Become clone list ~line 282–356)
- Modify: `internal/storetest/conformance.go` (`testSetHarnessSessionID*`)
- Modify: `internal/daemon/daemon.go:1008` (`stamp_harness_session` op)
- Modify: `internal/humancli/hook.go:722` (caller of the op)

**Interfaces:**
- Produces: `store.Agent.TranscriptPath string \`json:"transcript_path"\``; `store.API.StampHarness(alias, harnessSessionID, transcriptPath string) error` — empty argument leaves that field unchanged; unknown alias is a no-op.

- [ ] **Step 1: Write the failing conformance tests** (replace the two `SetHarnessSessionID` tests; keep the table names but point at the new functions)

```go
{"StampHarnessStampsBothFields", testStampHarness},
{"StampHarnessEmptyArgLeavesFieldAlone", testStampHarnessPartial},
{"StampHarnessUnknownAliasIsNoOp", testStampHarnessUnknown},
{"RegisterPersistsTranscriptPath", testRegisterTranscriptPath},
```

```go
func testStampHarness(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "backend", SocketPath: "/s", SessionID: "$1"})
	if err := s.StampHarness("backend", "uuid-1", "/t/a.jsonl"); err != nil {
		t.Fatalf("StampHarness: %v", err)
	}
	a, _, _ := s.GetAgent("backend")
	if a.HarnessSessionID != "uuid-1" || a.TranscriptPath != "/t/a.jsonl" {
		t.Fatalf("got harness=%q transcript=%q", a.HarnessSessionID, a.TranscriptPath)
	}
}

func testStampHarnessPartial(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "backend", SocketPath: "/s", SessionID: "$1", HarnessSessionID: "uuid-0", TranscriptPath: "/t/a.jsonl"})
	if err := s.StampHarness("backend", "uuid-1", ""); err != nil {
		t.Fatalf("StampHarness: %v", err)
	}
	a, _, _ := s.GetAgent("backend")
	if a.HarnessSessionID != "uuid-1" || a.TranscriptPath != "/t/a.jsonl" {
		t.Fatalf("empty transcript arg must not clear it: harness=%q transcript=%q", a.HarnessSessionID, a.TranscriptPath)
	}
	if err := s.StampHarness("backend", "", "/t/b.jsonl"); err != nil {
		t.Fatalf("StampHarness: %v", err)
	}
	a, _, _ = s.GetAgent("backend")
	if a.HarnessSessionID != "uuid-1" || a.TranscriptPath != "/t/b.jsonl" {
		t.Fatalf("empty harness arg must not clear it: harness=%q transcript=%q", a.HarnessSessionID, a.TranscriptPath)
	}
}

func testStampHarnessUnknown(t *testing.T, s store.API) {
	if err := s.StampHarness("ghost", "uuid-2", "/t/x.jsonl"); err != nil {
		t.Fatalf("unknown alias must be a no-op, got %v", err)
	}
	if _, ok, _ := s.GetAgent("ghost"); ok {
		t.Fatal("StampHarness must not create a row")
	}
}

func testRegisterTranscriptPath(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "a", SocketPath: "/s", SessionID: "$1", TranscriptPath: "/t/a.jsonl"})
	a, _, _ := s.GetAgent("a")
	if a.TranscriptPath != "/t/a.jsonl" {
		t.Fatalf("transcript_path not persisted: %q", a.TranscriptPath)
	}
	all, _ := s.ListAgents()
	if all[0].TranscriptPath != "/t/a.jsonl" {
		t.Fatal("ListAgents must carry transcript_path")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/ -run 'Conformance/(StampHarness|RegisterPersistsTranscriptPath)' 2>&1 | head`
Expected: compile error — `TranscriptPath`/`StampHarness` undefined.

- [ ] **Step 3: Implement — store model, schema, SQLite**

`models.go`, after `HarnessSessionID`:
```go
	// TranscriptPath is the harness conversation's transcript file — the
	// strongest identity key (spec 2026-08-21 §2): Claude Code never changes
	// it for a conversation, while the harness session ID can change under
	// /login. '' when the harness provides none (Codex, paneless).
	TranscriptPath string `json:"transcript_path"`
```

`store.go` migrate list, append: `` `ALTER TABLE agents ADD COLUMN transcript_path TEXT NOT NULL DEFAULT ''`, ``

`agents.go`: add `transcript_path` to the INSERT column list/VALUES (after `harness_session_id`), to the `ON CONFLICT` SET (`transcript_path=excluded.transcript_path`), to both SELECT lists + Scan (after `HarnessSessionID`), and to Become's INSERT/SELECT clone lists. Replace `SetHarnessSessionID` with:

```go
// StampHarness attaches the harness link to an existing row: the session
// UUID and the transcript path, each only when non-empty — a hook that
// knows one but not the other must not erase what another hook stamped.
// Identity, tuple, and read-state are untouched; unknown alias is a no-op.
func (s *Store) StampHarness(alias, harnessSessionID, transcriptPath string) error {
	_, err := s.db.Exec(`UPDATE agents SET
    harness_session_id = CASE WHEN ?2 = '' THEN harness_session_id ELSE ?2 END,
    transcript_path    = CASE WHEN ?3 = '' THEN transcript_path    ELSE ?3 END
WHERE alias = ?1`, alias, harnessSessionID, transcriptPath)
	return err
}
```

`api.go:111`: `StampHarness(alias, harnessSessionID, transcriptPath string) error` (delete `SetHarnessSessionID`).

- [ ] **Step 4: Implement — DynamoDB**

`RegisterAgent` set map: `"transcript_path": attrS(a.TranscriptPath),`. `itemToAgent`: `TranscriptPath: strAttr(item, "transcript_path"),`. Become's clone: copy `transcript_path` like `harness_session_id`. Replace `SetHarnessSessionID` with `StampHarness` building the SET expression only from non-empty args (return nil early if both empty) and keeping the existing `attribute_exists(pk)` condition so an unknown alias stays a no-op (match the existing function's error-swallowing of `ConditionalCheckFailedException`).

- [ ] **Step 5: Update callers**

`daemon.go` `stamp_harness_session`: `d.s.StampHarness(str(a, "alias"), str(a, "harness_session_id"), str(a, "transcript_path"))`. `hook.go` `stampHarnessLinks`: add `"transcript_path": h.TranscriptPath` to the op args. Fix any other compile errors (`grep -rn SetHarnessSessionID internal`).

- [ ] **Step 6: Run tests**

Run: `go test ./internal/store/ ./internal/daemon/ ./internal/humancli/ 2>&1 | tail -5` then `just verify-dynamo`
Expected: PASS.

- [ ] **Step 7: Commit** — `git commit -am "store: transcript_path on the row; StampHarness replaces SetHarnessSessionID"`

---

### Task 2: Register keeps `superseded_by`; lineage drops non-chain tombstones

**Files:**
- Modify: `internal/store/agents.go` (RegisterAgent ON CONFLICT; `SessionUnread` + `SessionAliasLineage` base case)
- Modify: `internal/dynamostore/agents.go` (RegisterAgent set map: drop the `superseded_by` reset; `sessionLineage` base filter)
- Modify: `internal/storetest/conformance.go` (`testRegisterResetsSupersededBy` → `testRegisterKeepsSupersededBy`; new lineage test)

- [ ] **Step 1: Flip the conformance test and add the lineage one**

```go
{"RegisterKeepsSupersededBy", testRegisterKeepsSupersededBy},
{"LineageExcludesTombstonesOfOtherConversations", testLineageExcludesForeignTombstones},
```

```go
func testRegisterKeepsSupersededBy(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "seed", SocketPath: "/s", SessionID: "$1"})
	if err := s.Become("seed", "claimed"); err != nil {
		t.Fatalf("Become: %v", err)
	}
	mustRegister(t, s, store.Agent{Alias: "seed", SocketPath: "/s", SessionID: "$1"})
	a, _, _ := s.GetAgent("seed")
	if a.SupersededBy != "claimed" {
		t.Fatalf("re-register must not forget the successor: got %q", a.SupersededBy)
	}
	if a.Departed {
		t.Fatal("re-registering still revives the tombstone")
	}
}

func testLineageExcludesForeignTombstones(t *testing.T, s store.API) {
	// A previous conversation's tombstone on the same tuple (no successor)…
	mustRegister(t, s, store.Agent{Alias: "old-conv", SocketPath: "/s", SessionID: "$1", SessionCreated: 5, PaneID: "%1"})
	if err := s.DepartAgent("old-conv"); err != nil {
		t.Fatal(err)
	}
	// …and the live conversation with a become-chain seed.
	mustRegister(t, s, store.Agent{Alias: "seed", SocketPath: "/s", SessionID: "$1", SessionCreated: 5, PaneID: "%1"})
	if err := s.Become("seed", "me"); err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionAliasLineage("", "/s", "$1", 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"me", "seed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage = %v, want %v (old-conv is another conversation's tombstone)", got, want)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/store/ -run 'Conformance/(RegisterKeeps|LineageExcludes)'` → FAIL.

- [ ] **Step 3: Implement**

SQLite `RegisterAgent`: remove `superseded_by=''` from the ON CONFLICT SET (keep `departed=0`); update the doc comment accordingly. In `SessionUnread` and `SessionAliasLineage` base case add `AND (departed = 0 OR superseded_by != '')` after the incarnation clause. DynamoDB: delete the `"superseded_by": attrS("")` line and its comment; in `sessionLineage`'s base-case loop skip rows where `a.Departed && a.SupersededBy == ""`.

- [ ] **Step 4: Run** — `go test ./internal/store/... ./internal/daemon/...` and `just verify-dynamo` → PASS (fix any daemon test that relied on the reset).

- [ ] **Step 5: Commit** — `git commit -am "store: re-register keeps superseded_by; lineage excludes other conversations' tombstones"`

---

### Task 3: `FindConversation`

**Files:**
- Modify: `internal/store/api.go` (new method after `SessionAliasLineage`)
- Modify: `internal/store/agents.go`
- Modify: `internal/dynamostore/agents.go`
- Modify: `internal/storetest/conformance.go`

**Interfaces:**
- Produces: `FindConversation(deviceID, transcriptPath, socketPath, sessionID string, sessionCreated int64, paneID string) (Agent, bool, error)`

- [ ] **Step 1: Conformance tests**

```go
{"FindConversationByTranscript", testFindConversationTranscript},
{"FindConversationByPaneTuple", testFindConversationPane},
{"FindConversationTranscriptBeatsPane", testFindConversationPrecedence},
{"FindConversationIgnoresDepartedAndOtherDevices", testFindConversationScope},
{"FindConversationRefusesUnprovenIncarnation", testFindConversationZeroCreated},
```

```go
func testFindConversationTranscript(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "a", DeviceID: "d", SocketPath: "/s", SessionID: "$1", SessionCreated: 5, PaneID: "%1", TranscriptPath: "/t/a.jsonl"})
	got, ok, err := s.FindConversation("d", "/t/a.jsonl", "/other", "$9", 1, "%9")
	if err != nil || !ok || got.Alias != "a" {
		t.Fatalf("by transcript: ok=%v alias=%q err=%v", ok, got.Alias, err)
	}
}

func testFindConversationPane(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "a", DeviceID: "d", SocketPath: "/s", SessionID: "$1", SessionCreated: 5, PaneID: "%1"})
	got, ok, _ := s.FindConversation("d", "", "/s", "$1", 5, "%1")
	if !ok || got.Alias != "a" {
		t.Fatalf("by pane: ok=%v alias=%q", ok, got.Alias)
	}
	if _, ok, _ := s.FindConversation("d", "", "/s", "$1", 5, "%2"); ok {
		t.Fatal("a different pane is a different conversation")
	}
	if _, ok, _ := s.FindConversation("d", "/t/none.jsonl", "/s", "$1", 5, "%1"); !ok {
		t.Fatal("an unknown transcript must still fall through to the pane match")
	}
}

func testFindConversationPrecedence(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "by-pane", DeviceID: "d", SocketPath: "/s", SessionID: "$1", SessionCreated: 5, PaneID: "%1"})
	mustRegister(t, s, store.Agent{Alias: "by-transcript", DeviceID: "d", SocketPath: "/old", SessionID: "$3", SessionCreated: 2, PaneID: "%7", TranscriptPath: "/t/c.jsonl"})
	got, ok, _ := s.FindConversation("d", "/t/c.jsonl", "/s", "$1", 5, "%1")
	if !ok || got.Alias != "by-transcript" {
		t.Fatalf("transcript must win: ok=%v alias=%q", ok, got.Alias)
	}
}

func testFindConversationScope(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "gone", DeviceID: "d", SocketPath: "/s", SessionID: "$1", SessionCreated: 5, PaneID: "%1", TranscriptPath: "/t/g.jsonl"})
	_ = s.DepartAgent("gone")
	if _, ok, _ := s.FindConversation("d", "/t/g.jsonl", "/s", "$1", 5, "%1"); ok {
		t.Fatal("departed rows are never a live conversation")
	}
	mustRegister(t, s, store.Agent{Alias: "elsewhere", DeviceID: "other", SocketPath: "/s", SessionID: "$1", SessionCreated: 5, PaneID: "%1", TranscriptPath: "/t/e.jsonl"})
	if _, ok, _ := s.FindConversation("d", "/t/e.jsonl", "/s", "$1", 5, "%1"); ok {
		t.Fatal("another device's row must not match")
	}
}

func testFindConversationZeroCreated(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "a", DeviceID: "d", SocketPath: "/s", SessionID: "$1", SessionCreated: 5, PaneID: "%1"})
	if _, ok, _ := s.FindConversation("d", "", "/s", "$1", 0, "%1"); ok {
		t.Fatal("session_created=0 is absence of proof: no pane match")
	}
	if _, ok, _ := s.FindConversation("d", "", "/s", "$1", 5, ""); ok {
		t.Fatal("empty pane id never matches")
	}
}
```

- [ ] **Step 2: Verify failure** — compile error.

- [ ] **Step 3: Implement SQLite**

```go
// FindConversation answers "which live row IS this conversation" (spec
// 2026-08-21 §2): by transcript path when the caller has one, else by the
// full live pane tuple. Harness session ID is deliberately not a key here —
// it can change under /login, which is the defect this op exists to close.
// A zero sessionCreated or empty paneID never pane-matches (absence of
// proof), mirroring every other tuple surface.
func (s *Store) FindConversation(deviceID, transcriptPath, socketPath, sessionID string, sessionCreated int64, paneID string) (Agent, bool, error) {
	if transcriptPath != "" {
		a, ok, err := s.scanOneAgent(`WHERE device_id=? AND transcript_path=? AND departed=0 ORDER BY last_seen DESC LIMIT 1`, deviceID, transcriptPath)
		if err != nil || ok {
			return a, ok, err
		}
	}
	if socketPath == "" || sessionID == "" || sessionCreated == 0 || paneID == "" {
		return Agent{}, false, nil
	}
	return s.scanOneAgent(`WHERE device_id=? AND socket_path=? AND session_id=? AND session_created=? AND pane_id=? AND departed=0 ORDER BY last_seen DESC LIMIT 1`,
		deviceID, socketPath, sessionID, sessionCreated, paneID)
}
```

Add `scanOneAgent(where string, args ...any) (Agent, bool, error)` that runs `SELECT <the full column list used by GetAgent> FROM agents ` + where and scans like `GetAgent`; refactor `GetAgent` to call it with `WHERE alias=?`.

- [ ] **Step 4: Implement DynamoDB** — `roster(ctx)` → `itemToAgent` each → first pass: transcript match (`!a.Departed && a.DeviceID==deviceID && a.TranscriptPath==transcriptPath`, transcript non-empty), pick highest `LastSeen`; second pass: `sameSession(a, deviceID, socketPath, sessionID, sessionCreated) && a.PaneID == paneID && paneID != "" && !a.Departed`, highest `LastSeen`. Document in the package comment that this is a roster scan like `DepartStaleSiblings`.

- [ ] **Step 5: Run** both gates → PASS. **Step 6: Commit** — `git commit -am "store: FindConversation — one live row per conversation, transcript then pane"`

---

### Task 4: `Become` reclaims a departed name

**Files:**
- Modify: `internal/store/agents.go` (Become), `internal/dynamostore/agents.go` (Become), `internal/storetest/conformance.go`, `internal/store/api.go` (doc on Become)

- [ ] **Step 1: Tests** (keep `BecomeRefusesAnExistingTarget` but make the target LIVE; add)

```go
{"BecomeReclaimsADepartedTarget", testBecomeReclaimsDeparted},
```

```go
func testBecomeReclaimsDeparted(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "old-name", SocketPath: "/s", SessionID: "$1", Label: "stale"})
	_ = s.DepartAgent("old-name")
	mustRegister(t, s, store.Agent{Alias: "seed", SocketPath: "/s", SessionID: "$2", Label: "fresh", HarnessSessionID: "u2"})
	if err := s.Become("seed", "old-name"); err != nil {
		t.Fatalf("Become over a departed target must succeed: %v", err)
	}
	a, ok, _ := s.GetAgent("old-name")
	if !ok || a.Departed || a.Label != "fresh" || a.HarnessSessionID != "u2" || a.SessionID != "$2" {
		t.Fatalf("reclaimed row must be the clone of seed, got %+v", a)
	}
	seed, _, _ := s.GetAgent("seed")
	if !seed.Departed || seed.SupersededBy != "old-name" {
		t.Fatalf("seed must be retired → old-name, got %+v", seed)
	}
}
```

Verify `testBecomeRefusesExistingTarget` registers a *live* target (it does today: a plain register). Leave it.

- [ ] **Step 2: Verify failure** — FAIL with `ErrBecomeToExists`.

- [ ] **Step 3: Implement** — SQLite: replace the COUNT check with `SELECT departed FROM agents WHERE alias=?`; `sql.ErrNoRows` → proceed; `departed=0` → `ErrBecomeToExists`; `departed=1` → `DELETE FROM agents WHERE alias=?` inside the tx, then proceed. DynamoDB: `rawAgentItem(to)`; if present and live → `ErrBecomeToExists`; if departed → include a `Delete` of `agentKey(to)` in the same `TransactWriteItems` ahead of the `Put` (the Put's `attribute_not_exists(pk)` condition then holds; if the transaction currently uses a conditional Put with `attribute_not_exists`, a Delete+Put of the same key in one transaction is NOT allowed by DynamoDB — so instead make the Put unconditional when reclaiming, guarded by a condition on the *seed* item: `attribute_exists(pk) AND departed = :false`... simplest correct form: two steps — `DeleteItem` with condition `departed = :true`, then the existing transaction; a race between the delete and the put can only fail the put, which the existing `ErrBecomeToExists` mapping already reports).

- [ ] **Step 4: Run** both gates → PASS. **Step 5: Commit** — `git commit -am "store: become may reclaim a departed name"`

---

### Task 5: Canonical socket paths

**Files:**
- Modify: `internal/tmuxenv/tmuxenv.go` (`SocketFromEnv` or `CaptureEnv` at ~line 265)
- Test: `internal/tmuxenv/tmuxenv_test.go`
- Modify: `internal/store/store.go` (`migrate`), Test: `internal/store/store_test.go`

- [ ] **Step 1: tmuxenv test**

```go
func TestCaptureEnvCanonicalizesSocketPath(t *testing.T) {
	home := mustertest.ShortHome(t)
	real := filepath.Join(home, "real")
	if err := os.MkdirAll(real, 0o755); err != nil { t.Fatal(err) }
	link := filepath.Join(home, "link")
	if err := os.Symlink(real, link); err != nil { t.Fatal(err) }
	sock := filepath.Join(link, "default")
	if err := os.WriteFile(sock, nil, 0o600); err != nil { t.Fatal(err) }
	t.Setenv("TMUX", sock+",123,0")
	t.Setenv("TMUX_PANE", "%1")
	c := CaptureEnv()
	if want := filepath.Join(real, "default"); c.SocketPath != want {
		t.Fatalf("SocketPath = %q, want canonical %q", c.SocketPath, want)
	}
}
```
(Adjust the `TMUX` env format to whatever `SocketFromEnv` parses — read it first.)

- [ ] **Step 2: Verify failure.** **Step 3: Implement** — in `SocketFromEnv` (the one canonical capture path): `if r, err := filepath.EvalSymlinks(p); err == nil { p = r }`.

- [ ] **Step 4: store migration test**

```go
func TestMigrateNormalizesSocketPaths(t *testing.T) {
	home := mustertest.ShortHome(t)
	real := filepath.Join(home, "real"); _ = os.MkdirAll(real, 0o755)
	link := filepath.Join(home, "link"); _ = os.Symlink(real, link)
	sock := filepath.Join(real, "s"); _ = os.WriteFile(sock, nil, 0o600)
	db := filepath.Join(home, "bus.db")
	s, _ := Open(db)
	_ = s.RegisterAgent(Agent{Alias: "a", SocketPath: filepath.Join(link, "s"), SessionID: "$1"})
	_ = s.RegisterAgent(Agent{Alias: "b", SocketPath: "/nonexistent/s", SessionID: "$1"})
	_ = s.Close()
	s, _ = Open(db) // migrate runs again
	a, _, _ := s.GetAgent("a"); b, _, _ := s.GetAgent("b")
	if a.SocketPath != sock { t.Fatalf("a = %q, want %q", a.SocketPath, sock) }
	if b.SocketPath != "/nonexistent/s" { t.Fatalf("unresolvable path must be left alone: %q", b.SocketPath) }
}
```

- [ ] **Step 5: Implement** — at the end of `migrate`: `SELECT DISTINCT socket_path FROM agents WHERE socket_path != ''`; for each, `EvalSymlinks`; if it differs, `UPDATE agents SET socket_path=? WHERE socket_path=?`. Idempotent by construction.

- [ ] **Step 6: Run** `go test ./internal/tmuxenv/ ./internal/store/` → PASS. **Step 7: Commit** — `git commit -am "tmuxenv+store: canonical socket paths (EvalSymlinks) at capture and on migrate"`

---

### Task 6: Daemon `register_agent` adopts the existing conversation

**Files:**
- Modify: `internal/daemon/daemon.go` (`handleRegisterAgent`, ~line 280–340)
- Test: `internal/daemon/daemon_test.go`

**Interfaces:**
- Consumes: `FindConversation`, `StampHarness` (Tasks 1, 3).
- Produces: `register_agent` args gain `transcript_path`; response gains `outcome: "adopted"` and always carries `alias` (the effective alias).

- [ ] **Step 1: Tests** (follow the package's existing `newTestDaemon`/`call` helpers — read one existing register test and mirror it)

```go
func TestRegisterAdoptsBySameTranscript(t *testing.T) {
	d := newTestDaemon(t)
	reg := func(alias, harness string) map[string]any {
		return call(t, d, "register_agent", map[string]any{
			"alias": alias, "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1",
			"harness_session_id": harness, "transcript_path": "/t/c.jsonl"})
	}
	reg("first", "uuid-A")
	resp := reg("second", "uuid-B") // same transcript, /login changed the harness id
	if resp["outcome"] != "adopted" || resp["alias"] != "first" {
		t.Fatalf("want adopted as first, got %v", resp)
	}
	if _, ok, _ := d.s.GetAgent("second"); ok {
		t.Fatal("no sibling row may be born")
	}
	a, _, _ := d.s.GetAgent("first")
	if a.HarnessSessionID != "uuid-B" {
		t.Fatalf("harness id must be re-stamped, got %q", a.HarnessSessionID)
	}
}

func TestRegisterAdoptsBySamePane(t *testing.T) {
	d := newTestDaemon(t)
	call(t, d, "register_agent", map[string]any{"alias": "hook-seed", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1", "transcript_path": "/t/c.jsonl"})
	resp := call(t, d, "register_agent", map[string]any{"alias": "mcp-name", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1", "harness_session_id": "uuid-old"})
	if resp["outcome"] != "adopted" || resp["alias"] != "hook-seed" {
		t.Fatalf("MCP register on an occupied pane must adopt, got %v", resp)
	}
}

func TestRegisterSameAliasStillRefreshes(t *testing.T) {
	d := newTestDaemon(t)
	args := map[string]any{"alias": "a", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1", "transcript_path": "/t/c.jsonl"}
	call(t, d, "register_agent", args)
	if resp := call(t, d, "register_agent", args); resp["outcome"] != "refreshed" {
		t.Fatalf("got %v", resp)
	}
}
```

- [ ] **Step 2: Verify failure.** **Step 3: Implement** — in `handleRegisterAgent`, after building `newAgent` (add `TranscriptPath: str(a, "transcript_path")`) and before the `if_absent` CAS: 

```go
	if conv, found, err := d.s.FindConversation(newAgent.DeviceID, newAgent.TranscriptPath, newAgent.SocketPath, newAgent.SessionID, newAgent.SessionCreated, newAgent.PaneID); err != nil {
		return fail(err)
	} else if found && conv.Alias != alias {
		// One conversation, one row (spec 2026-08-21 §3.2): the caller IS
		// the conversation this row already describes — re-stamp the
		// harness link and move the tuple, never insert a sibling.
		if err := d.s.StampHarness(conv.Alias, newAgent.HarnessSessionID, newAgent.TranscriptPath); err != nil {
			return fail(err)
		}
		moved := conv
		moved.SocketPath, moved.SessionID, moved.SessionCreated, moved.PaneID = newAgent.SocketPath, newAgent.SessionID, newAgent.SessionCreated, newAgent.PaneID
		moved.SessionName, moved.Project, moved.DeviceID, moved.DeviceName = newAgent.SessionName, newAgent.Project, newAgent.DeviceID, newAgent.DeviceName
		moved.HarnessSessionID, moved.TranscriptPath = coalesce(newAgent.HarnessSessionID, conv.HarnessSessionID), coalesce(newAgent.TranscriptPath, conv.TranscriptPath)
		if err := d.s.RegisterAgent(moved); err != nil {
			return fail(err)
		}
		d.reconcileBadge(conv.SocketPath, conv.SessionID, conv.SessionCreated)
		d.reconcileBadge(moved.SocketPath, moved.SessionID, moved.SessionCreated)
		unread, _ := d.s.UnreadCount(conv.Alias)
		d.logEvent(store.Event{Kind: "register", Agent: conv.Alias, Detail: "adopted (asked for " + alias + ")"})
		return ok(map[string]any{"outcome": "adopted", "alias": conv.Alias, "unread": unread})
	}
```
Add `func coalesce(a, b string) string { if a != "" { return a }; return b }` if the package lacks one. Make sure the normal-path response also includes `"alias": alias`. Keep `label`/`label_manual` from `conv` (labels belong to the conversation; the MCP caller's capture may be stale).

- [ ] **Step 4: Run** `go test ./internal/daemon/` → PASS. **Step 5: Commit** — `git commit -am "daemon: register_agent adopts the conversation's existing row"`

---

### Task 7: Daemon `get_inbox` — owned reads, peek otherwise

**Files:**
- Modify: `internal/daemon/daemon.go:860` (`get_inbox`)
- Test: `internal/daemon/daemon_test.go`

**Interfaces:**
- Produces: `get_inbox` args `caller_device_id, caller_socket_path, caller_session_id, caller_session_created, caller_pane_id, caller_harness_session_id` (all optional); response becomes `{"threads": [...], "marked_read": bool}`. **All clients of `get_inbox` change shape** (Tasks 9, 10, and `internal/humancli/paneless.go`/`hook.go` if they call it — `grep -rn '"get_inbox"' internal`).

- [ ] **Step 1: Tests**

```go
func TestGetInboxOwnedMarksRead(t *testing.T) {
	d := newTestDaemon(t)
	call(t, d, "register_agent", map[string]any{"alias": "me", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1"})
	call(t, d, "register_agent", map[string]any{"alias": "peer", "socket_path": "/s", "session_id": "$2", "session_created": 5, "pane_id": "%2"})
	call(t, d, "send_message", map[string]any{"from": "peer", "to": "me", "subject": "s", "body": "b"})
	resp := call(t, d, "get_inbox", map[string]any{"alias": "me", "caller_socket_path": "/s", "caller_session_id": "$1", "caller_session_created": 5, "caller_pane_id": "%1"})
	if resp["marked_read"] != true { t.Fatalf("owned read must mark: %v", resp) }
	if n, _ := d.s.UnreadCount("me"); n != 0 { t.Fatalf("unread after owned read = %d", n) }
}

func TestGetInboxUnownedIsAPeek(t *testing.T) {
	d := newTestDaemon(t)
	call(t, d, "register_agent", map[string]any{"alias": "me", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1"})
	call(t, d, "register_agent", map[string]any{"alias": "peer", "socket_path": "/s", "session_id": "$2", "session_created": 5, "pane_id": "%2"})
	call(t, d, "send_message", map[string]any{"from": "peer", "to": "me", "subject": "s", "body": "b"})
	for _, args := range []map[string]any{
		{"alias": "me"}, // no proof
		{"alias": "me", "caller_socket_path": "/s", "caller_session_id": "$2", "caller_session_created": 5, "caller_pane_id": "%2"}, // peer's pane
		{"alias": "me", "caller_socket_path": "/s", "caller_session_id": "$1", "caller_session_created": 0, "caller_pane_id": "%1"}, // unproven incarnation
	} {
		resp := call(t, d, "get_inbox", args)
		if resp["marked_read"] != false { t.Fatalf("%v: must be a peek", args) }
		if len(resp["threads"].([]any)) != 1 { t.Fatalf("%v: peek still returns the threads", args) }
		if n, _ := d.s.UnreadCount("me"); n != 1 { t.Fatalf("%v: watermark moved", args) }
	}
	evs, _ := d.s.Events(store.EventQuery{})
	if evs[len(evs)-1].Kind != "peek" { t.Fatalf("a peek must leave a journal artifact, last event %+v", evs[len(evs)-1]) }
}

func TestGetInboxChainSeedIsOwned(t *testing.T) {
	d := newTestDaemon(t)
	call(t, d, "register_agent", map[string]any{"alias": "seed", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1"})
	call(t, d, "become", map[string]any{"from": "seed", "to": "me"})
	resp := call(t, d, "get_inbox", map[string]any{"alias": "seed", "caller_socket_path": "/s", "caller_session_id": "$1", "caller_session_created": 5, "caller_pane_id": "%1"})
	if resp["marked_read"] != true { t.Fatal("a become-retired seed is still the caller's to drain") }
}

func TestGetInboxPanelessOwnedByHarnessID(t *testing.T) {
	d := newTestDaemon(t)
	call(t, d, "register_agent", map[string]any{"alias": "pl", "socket_path": "", "session_id": "uuid-1", "harness_session_id": "uuid-1"})
	resp := call(t, d, "get_inbox", map[string]any{"alias": "pl", "caller_harness_session_id": "uuid-1"})
	if resp["marked_read"] != true { t.Fatal("paneless ownership is the harness UUID") }
}
```
(Check the exact `send_message` arg names and the `Events` query type in the daemon tests before pasting.)

- [ ] **Step 2: Verify failure.** **Step 3: Implement**

```go
	case "get_inbox":
		alias, err := d.requireKnownAlias("alias", str(a, "alias"))
		if err != nil { return fail(err) }
		threads, err := d.s.Inbox(alias)
		if err != nil { return fail(err) }
		owned, err := d.callerOwns(alias, a)
		if err != nil { return fail(err) }
		if !owned {
			d.logEvent(store.Event{Kind: "peek", Agent: alias})
			return ok(map[string]any{"threads": threads, "marked_read": false})
		}
		if err := d.s.MarkRead(alias); err != nil { return fail(err) }
		// …existing badge + "read" event code unchanged…
		return ok(map[string]any{"threads": threads, "marked_read": true})
```

```go
// callerOwns decides whether the get_inbox caller may move alias's read
// watermark (spec 2026-08-21 §3.2): alias is in the lineage of the caller's
// proven tmux tuple, or — paneless — its row carries the caller's harness
// UUID. No proof, or a zero session_created, owns nothing.
func (d *Daemon) callerOwns(alias string, a map[string]any) (bool, error) {
	if hid := str(a, "caller_harness_session_id"); hid != "" {
		if ag, found, err := d.s.GetAgent(alias); err != nil {
			return false, err
		} else if found && ag.HarnessSessionID == hid {
			return true, nil
		}
	}
	sock, sess := str(a, "caller_socket_path"), str(a, "caller_session_id")
	if sess == "" {
		return false, nil
	}
	aliases, err := d.s.SessionAliasLineage(str(a, "caller_device_id"), sock, sess, i64(a, "caller_session_created"))
	if err != nil {
		return false, err
	}
	for _, x := range aliases {
		if x == alias {
			return true, nil
		}
	}
	return false, nil
}
```
Update `idem.go` if `get_inbox` appears in its tables (response shape changed). Update every in-repo caller of `get_inbox` to read `.threads` (`grep -rn '"get_inbox"' internal cmd`); station (`internal/station`?) included.

- [ ] **Step 4: Run** `go test ./...` → PASS. **Step 5: Commit** — `git commit -am "daemon: get_inbox moves the watermark only for an owned alias; unowned reads are journaled peeks"`

---

### Task 8: Hooks reclaim by transcript

**Files:**
- Modify: `internal/humancli/paneless.go:104` (`harnessOwnedRows` → `conversationRows`), `reviveRow`/`reclaimRow` (~150–185), `internal/humancli/hook.go` (`hookSessionStartResume` 254, seed register ~217, `stampHarnessLinks` 722)
- Test: `internal/humancli/hook_test.go` (mirror an existing SessionStart resume test — find `TestHookSessionStartResume` or similar)

- [ ] **Step 1: Test** — a roster row with `transcript_path=/t/c.jsonl, harness_session_id=uuid-A` on a dead tuple; SessionStart payload `{"session_id":"uuid-B","transcript_path":"/t/c.jsonl"}` in a live capture; assert the hook output contains `reconnected as '<alias>'` and the stubbed `register_agent` call carried `transcript_path: /t/c.jsonl` and `harness_session_id: uuid-B`. Use the package's existing `callData` stub pattern.

- [ ] **Step 2: Verify failure.** **Step 3: Implement** — `conversationRows(h harnessenv.Capture) []agentRow`: iterate `list_agents`; match `h.TranscriptPath != "" && ag.TranscriptPath == h.TranscriptPath`, else `ag.HarnessSessionID == h.SessionID || (ag.SocketPath == "" && ag.SessionID == h.SessionID)`. Pass `"transcript_path": h.TranscriptPath` (reclaim/revive: take `h` as a parameter; the seed register in `hook.go` ~217 too). `stampHarnessLinks`: skip condition becomes `ag.HarnessSessionID == h.SessionID && ag.TranscriptPath == h.TranscriptPath` (re-stamp when either differs), and send both fields. Update `hookSessionStartResume`/`hookSessionStartPaneless` call sites.

- [ ] **Step 4: Run** `go test ./internal/humancli/` → PASS. **Step 5: Commit** — `git commit -am "hook: reclaim the conversation by transcript; stamp transcript_path"`

---

### Task 9: MCP server — chain-following pane check, adopt message, inbox proof

**Files:**
- Modify: `internal/mcpserver/call.go:36` (`paneRegistration`), `tools_registry.go` (register handler), `tools_messages.go:95` (`getInboxHandler`, `GetInboxOut`)
- Test: `internal/mcpserver/*_test.go` (mirror existing handler tests with the `callDaemon` stub)

- [ ] **Step 1: Tests**
  - `paneRegistration` with roster `[{alias: seed, departed: true, superseded_by: me, pane %1}, {alias: me, pane %1}]` returns `me`; with `[{alias: ghost, departed: true, superseded_by: "", pane %1}]` returns `ok=false`.
  - register handler: stubbed daemon returns `{"outcome":"adopted","alias":"first"}` → `Detail` contains `already 'first' on this pane` and `become:true`.
  - `getInboxHandler`: stub records args; assert `caller_socket_path`/`caller_session_id`/`caller_session_created`/`caller_pane_id`/`caller_harness_session_id` present (set `TMUX`/`TMUX_PANE`/`CLAUDE_CODE_SESSION_ID` via `t.Setenv`); stub returns `{"threads":[],"marked_read":false}` → `Out.Detail` == `peek only — '<alias>' is not this session's; its unread state is unchanged`.

- [ ] **Step 2: Verify failure.** **Step 3: Implement** — `paneRegistration`: on a tuple match that is `Departed`, if `SupersededBy != ""` follow it (loop with a visited set, ≤ 8 hops) to the first non-departed row and return that; else continue. Register handler: after the daemon call, if `ack.Outcome == "adopted"` return `Detail: fmt.Sprintf("you are already '%s' on this pane — use that alias, or pass become:true to claim '%s' as this session's name", ack.Alias, in.Alias)`. `GetInboxOut` gains `MarkedRead bool \`json:"marked_read"\`` and `Detail string \`json:"detail,omitempty"\``; handler decodes `{threads, marked_read}` and passes proof from `tmuxenv.CaptureEnv()` + `harnessenv.FromEnv()` + `device.ID()` (whatever the register path uses for `device_id` — mirror it).

- [ ] **Step 4: Run** `go test ./internal/mcpserver/` → PASS. **Step 5: Commit** — `git commit -am "mcp: pane check follows become chains; adopt message; get_inbox carries caller proof"`

---

### Task 10: Human CLI — proof on `inbox`/`tasks`, peek line, become reclaim, help

**Files:**
- Modify: `internal/humancli/humancli.go:499–575` (`cmdInbox`, `cmdTasks`, `printThreads`), become command output, `internal/humancli/help.go` (or wherever `HelpFor` text lives — `grep -rn '"inbox"' internal/humancli/help*.go`)
- Test: `internal/humancli/humancli_test.go`

- [ ] **Step 1: Tests** — `TestInboxPeekPrintsNotice`: stub daemon returns `marked_read:false`; output ends with `peek only: 'x' is not this pane's alias — unread state unchanged`. `TestInboxOwnedPrintsNoNotice`. `TestBecomeReclaimMessage`: stub `become` returns `{"reclaimed": true}` → output contains `reclaimed departed name`. (Daemon `become` op: add `"reclaimed": bool` by checking `GetAgent(to)` departed-ness before calling `Become` — small addition in `daemon.go:1026`, with a daemon test.)

- [ ] **Step 2: Verify failure.** **Step 3: Implement** — `printThreads` takes the proof from `tmuxenv.CaptureEnv()` + `harnessenv.FromEnv()`, decodes `{threads, marked_read}`, prints the table, then the notice when `!marked_read`. Help text for `inbox`: one paragraph stating the owned-only rule and that a peek is journaled. Help for `become`: "A departed name may be reclaimed; a live one is refused."

- [ ] **Step 4: Run** `go test ./internal/humancli/ ./internal/daemon/` → PASS. **Step 5: Commit** — `git commit -am "cli: inbox/tasks prove ownership, peek otherwise; become reports a reclaim"`

---

### Task 11: Docs, skill, version, full gates

**Files:**
- Modify: `CLAUDE.md` ("The naming contract" paragraph: add one sentence — the transcript path is the durable identity key; harness session ID is an attribute; one live row per pane), `.claude/skills/muster-coordination/SKILL.md` (the wake-model bullet on the CLI fallback: `muster inbox` marks read only from your own pane, otherwise it is a peek), `docs/` user docs if `muster help` text is mirrored there (`grep -rln "muster inbox" docs README.md`), `VERSION` → `0.13.0`, `CHANGELOG.md` if present.

- [ ] **Step 1: Edit docs** (one line per paragraph). **Step 2: `just verify` and `just verify-dynamo`** → both PASS; paste the tail of each into the commit message body. **Step 3: Commit** — `git commit -am "docs+version: one conversation, one identity (0.13.0)"`.

- [ ] **Step 4: Live acceptance on this machine** (operator-gated surface — do not merge on green alone): build to `~/.local/bin/muster`, restart the daemon, then in the data-lake tmux session: `muster inbox personal-git-branch-handoff-flags/data-lake` from the `muster/debug` pane → prints the peek line and does NOT move its watermark (check `sqlite3 ~/.local/share/muster/bus.db "select last_read_entry_id from agents where alias like '%data-lake'"` before/after); `muster become bettor-help-workspace/data-lake` from the data-lake pane → "reclaimed departed name"; `muster agents` shows one live data-lake row.
