# Teammate Identity Refusal — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fleet-teammate sessions' hooks become no-ops (they can never claim a primary's bus identity), and `muster nudge` fails with an actionable message when a row's stored pane is dead.

**Architecture:** Spec at `docs/superpowers/specs/2026-08-06-teammate-identity-refusal-design.md`. Two strands: (1) `harnessenv.IsTeammate(transcriptPath)` — a bounded head-scan for a top-level `teamName` field — gates ALL of `muster hook` at entry; (2) a dead-pane guard in `cmdNudge` before the send-keys.

**Tech Stack:** Go, `internal/harnessenv` + `internal/humancli`; test conventions as in those packages (fixture transcripts via `os.WriteFile` in `t.TempDir()`, tmux via the swappable `tmuxenv.Run` var, daemon tests via the package's existing `startCLITestDaemon`/`registerAliveViaDaemon`/`listAgentsForTest` helpers).

## Global Constraints

- `just verify` green before every commit; cgo-free; no new dependencies.
- `internal/daemon` and `internal/store` untouched — this is hook-layer + CLI only.
- Hooks must never fail or block a session: the teammate gate returns nil silently; an unreadable transcript reads as NOT a teammate (fail-open).
- MCP `register_agent` and CLI `muster register` behavior untouched (explicit registration stays possible).
- Work in a worktree off origin/dev: `ROOT=$(git rev-parse --show-toplevel); git -C "$ROOT" worktree add "$ROOT/.worktrees/feat-teammate-refusal" -b feat/teammate-refusal origin/dev`.

---

### Task 1: harnessenv.IsTeammate

**Files:**
- Modify: `internal/harnessenv/harnessenv.go` (after `CustomTitle`)
- Test: `internal/harnessenv/harnessenv_test.go`

**Interfaces:**
- Produces: `func IsTeammate(transcriptPath string) bool` — true iff any record within the FIRST 30 lines has a non-empty top-level `teamName` string. False for empty path, unreadable file, no such field, or the field appearing only after line 30. Task 2 calls it at cmdHook entry.

- [ ] **Step 1: Write the failing tests**

```go
func TestIsTeammateDetectsMemberTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "member.jsonl")
	lines := []string{
		`{"type":"mode","mode":"normal","sessionId":"u1"}`,
		`{"type":"permission-mode","permissionMode":"auto","sessionId":"u1"}`,
		`{"parentUuid":null,"isSidechain":false,"teamName":"session-b41c21dd","agentName":"l5-mlb-measure","type":"user","message":{"role":"user","content":"go"}}`,
	}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	if !IsTeammate(path) {
		t.Fatal("teamName-bearing transcript must read as teammate")
	}
}

func TestIsTeammateFalseForPrimariesAndLeads(t *testing.T) {
	dir := t.TempDir()
	// a lead/primary: custom-title + agent-name records but NO teamName —
	// the spec's verified shape (an agent-name record alone must not match)
	path := filepath.Join(dir, "primary.jsonl")
	lines := []string{
		`{"type":"custom-title","customTitle":"nfl-3","sessionId":"u2"}`,
		`{"type":"agent-name","agentName":"nfl-3","sessionId":"u2"}`,
		`{"type":"user","message":{"role":"user","content":"body mentioning teamName in prose"}}`,
	}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	if IsTeammate(path) {
		t.Fatal("primary transcript must not read as teammate")
	}
}

func TestIsTeammateFailOpenAndBounded(t *testing.T) {
	if IsTeammate("") {
		t.Fatal("empty path must be false")
	}
	if IsTeammate(filepath.Join(t.TempDir(), "missing.jsonl")) {
		t.Fatal("missing file must be false")
	}
	// teamName appearing only AFTER line 30 does not match: the signal
	// sits in the first few lines by construction, and an unbounded scan
	// would false-positive on conversation text echoing transcripts.
	dir := t.TempDir()
	path := filepath.Join(dir, "late.jsonl")
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, `{"type":"user","message":{"role":"user","content":"x"}}`)
	}
	lines = append(lines, `{"teamName":"t","agentName":"a","type":"user"}`)
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	if IsTeammate(path) {
		t.Fatal("teamName beyond line 30 must not match (bounded scan)")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/harnessenv/ -run IsTeammate -v`
Expected: FAIL — `undefined: IsTeammate`

- [ ] **Step 3: Implement**

```go
// IsTeammate reports whether the transcript at path belongs to a fleet
// TEAMMATE session — a member spawned into a pane of some primary's tmux
// session. Members' transcripts carry a top-level teamName (with
// agentName) on their records from the first few lines; team LEADS and
// plain primaries never do (verified over 46 transcripts, 2026-08-06 —
// see the teammate-identity-refusal spec §2; a bare agent-name record
// does NOT discriminate, /rename'd primaries have those). The scan is
// bounded to the first 30 lines: the signal sits at the top by
// construction, and an unbounded scan would false-positive on
// conversation text that quotes transcripts. Empty path, unreadable
// file, or no match read as NOT a teammate — hooks fail open, they never
// block a session.
func IsTeammate(transcriptPath string) bool {
	if transcriptPath == "" {
		return false
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for i := 0; i < 30 && sc.Scan(); i++ {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"teamName"`)) {
			continue
		}
		var rec struct {
			TeamName string `json:"teamName"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.TeamName != "" {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/harnessenv/ -race`
Expected: PASS (all, including pre-existing)

- [ ] **Step 5: Commit**

```bash
git add internal/harnessenv/
git commit -m "feat(harnessenv): IsTeammate — bounded teamName scan identifies fleet members"
```

---

### Task 2: the hook gate

**Files:**
- Modify: `internal/humancli/hook.go` (`cmdHook`, right after the payload read at ~line 40)
- Test: `internal/humancli/hook_test.go`

**Interfaces:**
- Consumes: `harnessenv.IsTeammate` (Task 1), `harnessenv.FromHookPayload(...).TranscriptPath` (exists).
- Produces: the behavioral contract — every `muster hook <event>` invocation whose payload transcript identifies a teammate returns nil with no output and no daemon calls.

- [ ] **Step 1: Write the failing tests**

Mirror the file's existing daemon-backed SessionStart test scaffolding (Run-stub + `startCLITestDaemon` + fixture transcripts as in the Task-6 projection tests; reuse `registerAliveViaDaemon`/`listAgentsForTest`/`agentRowForTest`). Three tests:

```go
// TestHookTeammateSessionStartTouchesNothing is the 2026-08-06 incident
// as a regression test: a primary registered on pane %1 of $1; a
// teammate's SessionStart fires from pane %2 of the SAME session with a
// teamName-bearing transcript. The row must be byte-for-byte untouched
// (pane, harness id, label) and the roster must gain no alias.
func TestHookTeammateSessionStartTouchesNothing(t *testing.T) {
	// fixture: teammate transcript (teamName by line 3, as in Task 1's member fixture)
	// setup: startCLITestDaemon; register primary alias on ("/s", "$1", created 200, pane "%1")
	//        with a manual label via the set_label op
	// stub tmux Run: session-created probes answer "200"; capture all calls
	// env: point the hook's capture at pane %2 of the same session
	//      (t.Setenv TMUX + TMUX_PANE as the projection tests do)
	// act: cmdHook([]string{"SessionStart"}, strings.NewReader(payloadWithTranscript), &buf)
	//      where payload carries source:"startup" and the teammate transcript_path
	// assert: buf.Len() == 0; roster has exactly the one primary row;
	//         primary row's PaneID, HarnessSessionID, Label all unchanged
}

// TestHookTeammateSessionEndTombstonesNothing: same setup; cmdHook
// SessionEnd with the teammate transcript; primary row stays departed=0.

// TestHookTeammateStopEmitsNoWake: same setup with an unread message for
// the primary; cmdHook Stop with the teammate transcript; buf stays empty
// (no wake text) and the primary's unread count is not drained.
```

Write the bodies fully — the comments above name every assertion; expand them into real code against the package's actual helper names (read the Task-6 projection tests first; they contain the same setup shapes).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/humancli/ -run HookTeammate -v`
Expected: all three FAIL (today the teammate's SessionStart registers/claims, its Stop drains).

- [ ] **Step 3: Implement the gate**

In `cmdHook`, immediately after `payload, _ := io.ReadAll(...)`:

```go
	// A fleet TEAMMATE's hooks are no-ops (teammate-identity-refusal spec
	// §3): a teammate is a full Claude session in a pane of some
	// primary's tmux session, so without this gate it races the primary
	// for the session's bus identity at every register, resume-reclaim,
	// projection, tombstone sweep, and stamp — and the pane-ownership
	// guard only protects a primary while its pane claim is provable
	// (caught live 2026-08-06: a teammate stamped its pane + harness id
	// onto two primaries' rows). Explicit registration (MCP
	// register_agent, `muster register`) is deliberately untouched — a
	// teammate can still be GIVEN an identity on purpose; what is barred
	// is automatic capture. Codex/Cursor payloads carry no
	// transcript_path, so they never match.
	if harnessenv.IsTeammate(harnessenv.FromHookPayload(payload).TranscriptPath) {
		return nil
	}
```

- [ ] **Step 4: Run the suite**

Run: `go test ./internal/humancli/ -race`
Expected: PASS — the three new tests plus every pre-existing hook test (the gate must not affect payloads without teamName transcripts).

- [ ] **Step 5: Full gate + commit**

Run: `just verify`
Expected: PASS

```bash
git add internal/humancli/
git commit -m "fix(hook): teammate sessions' hooks are no-ops — fleet members never race for identity"
```

---

### Task 3: nudge dead-pane message

**Files:**
- Modify: `internal/humancli/humancli.go` (`cmdNudge`, before the "nudging …" print at ~line 518)
- Test: `internal/humancli/humancli_test.go` (or wherever the package's existing cmdNudge tests live — find with `grep -rn "cmdNudge" internal/humancli/*_test.go` and sit beside them)

**Interfaces:**
- Consumes: `tmuxenv.IsPaneAlive(socket, paneID)`, `tmuxenv.IsSessionAlive(socket, sessionID, created)` (both exist), the fetched agent row `ag` (has SocketPath/PaneID/SessionID/SessionCreated).
- Produces: an error instead of a doomed send-keys when the stored pane is dead but the session is alive.

- [ ] **Step 1: Write the failing test**

Mirror an existing cmdNudge test's daemon + Run-stub setup. Register an agent whose stored pane is `%99` with `session_created` 200; stub tmux so the `#{session_created}` probe answers `200` (session alive) but the `IsPaneAlive` probe for `%99` answers empty (pane gone). Invoke `cmdNudge` and assert: it returns an error, the error string contains `stored pane %99 is gone` and `heals at the session's next start`, and NO send-keys call appears in the captured tmux calls.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/humancli/ -run NudgeDeadPane -v`
Expected: FAIL (today it prints "nudging …" and attempts the send-keys, which errors opaquely)

- [ ] **Step 3: Implement**

In `cmdNudge`, after the agent row is fetched and before the "nudging …" Fprintf:

```go
	if ag.SocketPath != "" && !tmuxenv.IsPaneAlive(ag.SocketPath, ag.PaneID) &&
		tmuxenv.IsSessionAlive(ag.SocketPath, ag.SessionID, ag.SessionCreated) {
		return fmt.Errorf("nudge %s: stored pane %s is gone but its session is alive — the row heals at the session's next start/resume (or re-register from the live pane); refusing to type into a guessed pane", ag.Alias, ag.PaneID)
	}
```

(Do not auto-retarget: with teammate panes in the same session, typing into a guessed pane is worse than failing — spec §3.)

- [ ] **Step 4: Run the suite, full gate, commit**

Run: `go test ./internal/humancli/ -race && just verify`
Expected: PASS

```bash
git add internal/humancli/
git commit -m "fix(nudge): refuse a dead stored pane with the remedy, not a bare failure"
```

---

## Operator acceptance

Spawn any subagent teammate from a registered primary session (a normal `/loop` of daily work does this constantly), let it finish, then confirm: `muster agents` gained no alias, the primary row's pane/harness are unchanged (`sqlite3 ~/.local/share/muster/bus.db "select alias,pane_id,harness_session_id from agents where alias='<primary>'"`), and prefix m still nudges. For Task 3: point a row at a dead pane in a scratch MUSTER_HOME and confirm the new message.
