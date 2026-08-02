# `muster become` (Claim Your Name) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A session claims its real name (`muster become alias-routing` / MCP `register_agent(..., become:true)`): a new identity row is created under the claimed alias (read watermark carried over) and the tmux-seeded row retires.

**Architecture:** One daemon op (`become`) backed by one transactional store method; a CLI verb reusing the hook layer's env-else-walk capture; a `become` flag threaded through the MCP pane guard that currently refuses second aliases; two sentences of skill text. No wrapper, hook, label, or dotfiles changes. Spec: `docs/superpowers/specs/2026-08-01-become-claim-your-name-design.md`.

**Tech Stack:** Go, pure-Go SQLite, newline-JSON daemon protocol, MCP go-sdk.

## Global Constraints

- **Worktree from `origin/dev`** (primary clone lags): `git fetch origin && git worktree add ~/GitHub/worktrees/muster-become -b feat/become-claim-your-name origin/dev`. First commit copies this plan + the spec from the primary clone.
- **`just verify` before every commit**; cgo-free; `internal/daemon`/`internal/store` stay tmux/harness-agnostic.
- **stdout sacred in mcp mode**; hooks-never-block untouched (this feature adds no hook paths).
- House style: full-sentence rationale-carrying doc comments; test harnesses: `startWithNotifier`/`call` (daemon), `startTestDaemon`/`callData`/`hookRun`/`pinAncestryWalkAway` (humancli), `callDaemon` stub var (mcpserver).
- Wire-shape reference points on origin/dev: `store.Agent` fields incl. `HarnessSessionID` + `LastReadEntryID` (`internal/store/models.go`); register outcome response and `storeAPI` (`internal/daemon/daemon.go`); `hookCapture()` (`internal/humancli/hook.go` — unexported, same package as the new CLI verb, reuse directly); pane guard (`internal/mcpserver/tools_registry.go` + `validate.go`); registry pattern (`internal/humancli/registry.go`, goldens in `registry_test.go`).
- Exact strings (spec): journal event `Kind:"become", Agent:<to>, Detail:"<from> → <to>"`; daemon response Data `{"from","to","unread"}`; CLI output `you are now '<to>' (was '<from>') — N unread thread(s)`; MCP become Detail `you are now '<to>' (was '<from>'); N unread thread(s): call get_inbox with alias '<to>'`; refusal message gains `, or pass become:true to claim '<requested>' as this session's name`.

## Interfaces established by this plan

- `store.Become(from, to string) error` — transactional clone+retire; typed sentinel errors `store.ErrBecomeFromMissing`, `store.ErrBecomeToExists`.
- Daemon op `become {from, to}` → `ok(map{"from","to","unread"})`; guards map sentinels to loud messages; journal + badge reconcile.
- CLI `muster become <name> [--from <alias>]`.
- MCP `RegisterAgentIn.Become bool` (`json:"become,omitempty"`).

---

### Task 1: store.Become — transactional clone + retire

**Files:**
- Modify: `internal/store/agents.go`
- Test: `internal/store/agents_test.go` (append)

**Interfaces:**
- Produces: `Become(from, to string) error`; `ErrBecomeFromMissing`, `ErrBecomeToExists` (package-level `errors.New`).

- [ ] **Step 1: Write the failing test** (use this file's existing store-opening helper):

```go
// TestBecomeClonesIdentityAndRetiresSeed covers the become spec's core move:
// the claimed alias inherits the seed's full identity INCLUDING the read
// watermark (without it, all of history would flip unread), and the seed
// retires as a tombstone with its own history intact.
func TestBecomeClonesIdentityAndRetiresSeed(t *testing.T) {
	s := openTestStore(t) // match the helper name used by neighboring tests
	if err := s.RegisterAgent(Agent{
		Alias: "muster-2", Role: "peer", ModelType: "claude",
		SocketPath: "/s", SessionID: "$1", SessionCreated: 111, PaneID: "%1",
		SessionName: "muster-2", HarnessSessionID: "uuid-1",
		Project: "muster", Label: "durable-alias", LabelManual: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRead("muster-2"); err != nil { // establish a nonzero watermark
		t.Fatal(err)
	}
	seed, _, _ := s.GetAgent("muster-2")

	if err := s.Become("muster-2", "alias-routing"); err != nil {
		t.Fatal(err)
	}
	to, ok, err := s.GetAgent("alias-routing")
	if err != nil || !ok {
		t.Fatalf("claimed alias missing: %v %v", ok, err)
	}
	if to.SocketPath != "/s" || to.SessionID != "$1" || to.SessionCreated != 111 ||
		to.PaneID != "%1" || to.HarnessSessionID != "uuid-1" ||
		to.Project != "muster" || to.Label != "durable-alias" || !to.LabelManual ||
		to.Role != "peer" || to.ModelType != "claude" || to.Departed {
		t.Fatalf("clone dropped identity fields: %+v", to)
	}
	if to.LastReadEntryID != seed.LastReadEntryID {
		t.Fatalf("watermark not carried: got %d want %d", to.LastReadEntryID, seed.LastReadEntryID)
	}
	from, _, _ := s.GetAgent("muster-2")
	if !from.Departed {
		t.Fatalf("seed not retired: %+v", from)
	}
}

// TestBecomeGuards: missing from and existing to (live OR tombstoned) both
// fail with the typed sentinels — become never silently fuses identities.
func TestBecomeGuards(t *testing.T) {
	s := openTestStore(t)
	if err := s.Become("ghost", "x"); !errors.Is(err, ErrBecomeFromMissing) {
		t.Fatalf("missing from: got %v", err)
	}
	_ = s.RegisterAgent(Agent{Alias: "a"})
	_ = s.RegisterAgent(Agent{Alias: "b"})
	if err := s.Become("a", "b"); !errors.Is(err, ErrBecomeToExists) {
		t.Fatalf("live to: got %v", err)
	}
	_ = s.DepartAgent("b")
	if err := s.Become("a", "b"); !errors.Is(err, ErrBecomeToExists) {
		t.Fatalf("tombstoned to must ALSO refuse: got %v", err)
	}
	// Departed FROM is fine: a session may claim after gc tombstoned its seed.
	_ = s.DepartAgent("a")
	if err := s.Become("a", "c"); err != nil {
		t.Fatalf("departed from should still clone: %v", err)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/store/ -run TestBecome -race` — expect FAIL (undefined).

- [ ] **Step 3: Implement** in `internal/store/agents.go`:

```go
// ErrBecomeFromMissing / ErrBecomeToExists are become's guard sentinels —
// the daemon maps them to loud, hint-carrying wire errors.
var (
	ErrBecomeFromMissing = errors.New("become: from alias not found")
	ErrBecomeToExists    = errors.New("become: to alias already exists")
)

// Become claims a new name for an existing identity (spec:
// become-claim-your-name): inserts to as a CLONE of from — tuple, harness
// link, project, label, role, model, and the READ WATERMARK, without which
// the claimed identity would see all of history as unread — then retires
// from as a tombstone. to must not exist at all: a live row is someone
// else's identity and a tombstone is some other conversation's history;
// merging identities is exactly the confusion this feature exists to kill.
// from may already be departed (a claim after gc swept the seed). One
// transaction: a crash mid-become never leaves both rows live.
func (s *Store) Become(from, to string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agents WHERE alias=?`, to).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrBecomeToExists
	}
	now := clock.NowMillis()
	res, err := tx.Exec(`
INSERT INTO agents (alias, role, model_type, socket_path, pane_id, session_name, session_id, session_created, harness_session_id, project, label, label_manual, departed, registered_at, last_seen, last_read_entry_id, last_read_at)
SELECT ?, role, model_type, socket_path, pane_id, session_name, session_id, session_created, harness_session_id, project, label, label_manual, 0, ?, ?, last_read_entry_id, last_read_at
FROM agents WHERE alias=?`, to, now, now, from)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err != nil {
		return err
	} else if rows == 0 {
		return ErrBecomeFromMissing
	}
	if _, err := tx.Exec(`UPDATE agents SET departed=1 WHERE alias=?`, from); err != nil {
		return err
	}
	return tx.Commit()
}
```

(Check the INSERT column list against the CURRENT schema/RegisterAgent on the worktree — if `last_read_at` isn't in RegisterAgent's list it still exists as a column via migration; include it. Adjust to the real column set.)

- [ ] **Step 4: Run** the package — `go test ./internal/store/ -race` — expect PASS.
- [ ] **Step 5: Commit** — `feat(store): Become clones an identity under its claimed name and retires the seed`

---

### Task 2: Daemon op `become`

**Files:**
- Modify: `internal/daemon/daemon.go` (dispatch switch + storeAPI)
- Test: `internal/daemon/become_test.go` (new)

**Interfaces:**
- Consumes: Task 1's `Become` + sentinels; existing `UnreadCount`, `logEvent`, `reconcileBadge`.
- Produces: op `become {from, to}` → `ok(map[string]any{"from": from, "to": to, "unread": n})`.

- [ ] **Step 1: Failing test:**

```go
// TestBecomeOpClaimsAndReports: end-to-end over the wire — identity moves,
// seed retires, journal records the claim, unread rides the response so
// surfaces can say "you are now X — N unread".
func TestBecomeOpClaimsAndReports(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/s", "session_id": "$1", "harness_session_id": "uuid-1",
	})
	call(t, sock, "register_agent", map[string]any{"alias": "peer", "socket_path": "/s", "session_id": "$2"})
	call(t, sock, "send_message", map[string]any{
		"from": "peer", "to_kind": "agent", "to_target": "muster-2", "subject": "s", "body": "b",
	})

	resp := call(t, sock, "become", map[string]any{"from": "muster-2", "to": "alias-routing"})
	if !resp.OK {
		t.Fatalf("become: %+v", resp)
	}
	data, _ := resp.Data.(map[string]any)
	if data["to"] != "alias-routing" || data["from"] != "muster-2" {
		t.Fatalf("response = %+v", data)
	}
	if n, _ := data["unread"].(float64); n < 1 {
		t.Fatalf("unread = %v, want >= 1 (pre-claim mail concerns the claimed identity's session)", data["unread"])
	}

	ev := call(t, sock, "list_events", map[string]any{"agent": "alias-routing"})
	if !containsEventDetail(t, ev, "become", "muster-2 → alias-routing") {
		t.Fatalf("no become journal event: %+v", ev.Data)
	}

	resp = call(t, sock, "become", map[string]any{"from": "alias-routing", "to": "peer"})
	if resp.OK || !strings.Contains(resp.Error, "already has history") {
		t.Fatalf("existing-to guard: %+v", resp)
	}
}
```

(`containsEventDetail` exists in `register_outcome_test.go` — same package, reuse. NOTE the unread assertion: unread is computed for the TO alias via `UnreadCount(to)`; the message was addressed to the seed. If `UnreadCount("alias-routing")` doesn't count the seed-addressed thread — threadConcerns matches by alias text — the assertion will fail. In that case compute unread via `SessionUnread(tuple)` in the op instead: the spec's intent is "how much mail is waiting for this session", and SessionUnread is the canonical session-level number. Decide by what the first test run shows; document the choice in the op's comment.)

- [ ] **Step 2: Run** — expect FAIL (unknown op).
- [ ] **Step 3: Implement.** Add `Become(from, to string) error` to `storeAPI`. Dispatch case:

```go
	case "become":
		from, to := str(a, "from"), str(a, "to")
		if err := d.s.Become(from, to); err != nil {
			switch {
			case errors.Is(err, store.ErrBecomeFromMissing):
				return fail(fmt.Errorf("become: no such alias %q to become from; register first", from))
			case errors.Is(err, store.ErrBecomeToExists):
				return fail(fmt.Errorf("become: alias %q already has history; pick another name, or purge it with `muster gc --purge-agents`", to))
			}
			return fail(err)
		}
		d.logEvent(store.Event{Kind: "become", Agent: to, Detail: from + " → " + to})
		ag, _, err := d.s.GetAgent(to)
		if err != nil {
			return fail(err)
		}
		d.reconcileBadge(ag.SocketPath, ag.SessionID)
		unread, _, err := d.s.SessionUnread(ag.SocketPath, ag.SessionID)
		if err != nil {
			unread = 0 // best-effort: the claim already succeeded
		}
		return ok(map[string]any{"from": from, "to": to, "unread": unread})
```

(Adjust the unread source per Step 1's note; SessionUnread shown here as the likely-correct choice. For a paneless tuple SessionUnread still groups by ("", uuid) — fine.)

- [ ] **Step 4: Run** `go test ./internal/daemon/ -race` — expect PASS.
- [ ] **Step 5: Commit** — `feat(daemon): become op — claim a name, retire the seed, report unread`

---

### Task 3: CLI `muster become <name>`

**Files:**
- Create: `internal/humancli/become.go`
- Modify: `internal/humancli/registry.go` (+ goldens per its tests)
- Test: `internal/humancli/become_test.go` (new)

**Interfaces:**
- Consumes: `hookCapture()` (same package), `callData`, session_aliases op, Task 2's op.
- Produces: `muster become <name> [--from <alias>]`.

- [ ] **Step 1: Failing tests** (mirror `whereami_test.go`'s setup style; `pinAncestryWalkAway(t)` on env-empty tests):

```go
// TestBecomeClaimsSingleAliasSession: the happy path — one live alias on
// this session; become claims the new name and reports the trade.
func TestBecomeClaimsSingleAliasSession(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$1", "#{session_name}": "muster-2", "#{session_created}": "111",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })
	if _, err := callData("register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$1", "session_created": 111,
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Dispatch([]string{"become", "alias-routing"}, &buf); err != nil {
		t.Fatalf("become: %v", err)
	}
	if !strings.Contains(buf.String(), "you are now 'alias-routing' (was 'muster-2')") {
		t.Fatalf("output = %q", buf.String())
	}
	ag, ok := hookGetAgent("alias-routing")
	if !ok || ag.Departed || ag.SessionID != "$1" {
		t.Fatalf("claimed row = %+v (ok=%v)", ag, ok)
	}
}

// TestBecomeRequiresFromWhenSplit: two live aliases on one session — never
// guess which identity is being claimed over.
func TestBecomeRequiresFromWhenSplit(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_id}": "$1"})
	t.Cleanup(func() { tmuxenv.Run = prev })
	for _, a := range []string{"muster-2", "cost-audit"} {
		if _, err := callData("register_agent", map[string]any{
			"alias": a, "socket_path": "/tmp/sock", "session_id": "$1",
		}); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	err := Dispatch([]string{"become", "alias-routing"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "--from") {
		t.Fatalf("want --from requirement error, got %v (out %q)", err, buf.String())
	}
	if err := Dispatch([]string{"become", "alias-routing", "--from", "cost-audit"}, &buf); err != nil {
		t.Fatalf("explicit --from: %v", err)
	}
}
```

- [ ] **Step 2: Run** — expect FAIL (unknown command).
- [ ] **Step 3: Implement `cmdBecome`:** parse `--from` via the flag pattern `cmdRegister` uses; resolve identity with `hookCapture()`; list live aliases via `callData("session_aliases", ...)` filtered to non-departed via `hookGetAgent` (session_aliases includes departed on purpose — filter here); 0 live → `fmt.Errorf("nothing to become from on this session; register first")`; >1 live and no `--from` → `fmt.Errorf("this session has aliases %s; pass --from <alias>", list)`; call the op; print `you are now '<to>' (was '<from>') — N unread thread(s)`. Register in `registry.go` (help: "claim this session's real name: a new alias inherits this session's identity and inbox watermark, and the tmux-seeded alias retires — route traffic by a name the work deserves"). Fix registry goldens.
- [ ] **Step 4: Run** `go test ./internal/humancli/ -race` — expect PASS.
- [ ] **Step 5: Commit** — `feat(cli): muster become — claim this session's real name`

---

### Task 4: MCP — become flag on register_agent

**Files:**
- Modify: `internal/mcpserver/tools_registry.go`
- Test: `internal/mcpserver/tools_registry_test.go` (append)

**Interfaces:**
- Consumes: existing `paneRegistration` guard, `callDaemon` stub seam, Task 2's op.
- Produces: `RegisterAgentIn.Become bool `+"`json:\"become,omitempty\" jsonschema:\"claim this alias as the session's name: the current alias retires and its identity and read-state carry over\"`"+`.

- [ ] **Step 1: Failing tests** (mirror the existing `callDaemon`-stub tests):

```go
// TestRegisterAgentBecomeClaimsThroughPaneGuard: an already-registered pane
// calling register_agent with become:true issues the become op instead of
// the refusal, and the Detail reports the trade.
func TestRegisterAgentBecomeClaimsThroughPaneGuard(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%6")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) { return "", nil }
	t.Cleanup(func() { tmuxenv.Run = prev })

	var becomeArgs map[string]any
	prevDaemon := callDaemon
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		switch op {
		case "become":
			becomeArgs = args
			return []byte(`{"from":"muster-2","to":"alias-routing","unread":2}`), nil
		default: // paneRegistration's roster probe: this pane already owns muster-2
			return []byte(`[{"alias":"muster-2","socket_path":"/tmp/sock","session_id":"$1","pane_id":"%6"}]`), nil
		}
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, out, err := registerAgentHandler(context.TODO(), nil, RegisterAgentIn{
		Alias: "alias-routing", Role: "peer", ModelType: "claude", Become: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if becomeArgs["from"] != "muster-2" || becomeArgs["to"] != "alias-routing" {
		t.Fatalf("become args = %+v", becomeArgs)
	}
	if !strings.Contains(out.Detail, "you are now 'alias-routing' (was 'muster-2')") ||
		!strings.Contains(out.Detail, "2 unread") {
		t.Fatalf("Detail = %q", out.Detail)
	}
}

// TestRegisterAgentRefusalAdvertisesBecome: the become:false refusal now
// tells the agent how to claim instead of dead-ending.
func TestRegisterAgentRefusalAdvertisesBecome(t *testing.T) {
	// same stubs as above, Become:false
	// assert out.Detail contains "pass become:true to claim 'alias-routing'"
}
```

(Adapt the roster-probe stub to `paneRegistration`'s REAL call+shape on the worktree — read `validate.go` first; the existing idempotent-pane test shows the working stub. Fill the second test fully from the first's scaffolding.)

- [ ] **Step 2: Run** — expect FAIL.
- [ ] **Step 3: Implement:** add `Become` to `RegisterAgentIn`. In `registerAgentHandler`'s pane-guard branch: if `row.Alias != in.Alias` and `in.Become` → `callDaemon("become", {"from": row.Alias, "to": in.Alias})`, decode `{from,to,unread}`, Detail `fmt.Sprintf("you are now '%s' (was '%s'); %d unread thread(s): call get_inbox with alias '%s'", to, from, unread, to)`; on daemon error return it (loud — an existing-to guard message must reach the agent). If `in.Become` and the pane is NOT registered → fall through to the normal register (degrade gracefully). Update the refusal Detail: append `", or pass become:true to claim '" + in.Alias + "' as this session's name"`.
- [ ] **Step 4: Run** `go test ./internal/mcpserver/ -race` — expect PASS.
- [ ] **Step 5: Commit** — `feat(mcp): register_agent become:true claims the alias through the pane guard`

---

### Task 5: Integration tests + skill text

**Files:**
- Test: `internal/humancli/become_integration_test.go` (new)
- Modify: `.claude/skills/muster-coordination/SKILL.md`

- [ ] **Step 1: Failing integration tests:**

```go
// TestStopHookDrainsSeedStragglerAfterBecome: mail sent to the retired seed
// AFTER the claim still reaches the session's drain — session_aliases
// includes departed aliases on purpose, and become must not break that.
func TestStopHookDrainsSeedStragglerAfterBecome(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%1")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"@muster_inbox": "1", "#{session_id}": "$1", "#{session_name}": "muster-2",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })
	seed := func(op string, args map[string]any) {
		t.Helper()
		if _, err := callData(op, args); err != nil {
			t.Fatal(err)
		}
	}
	seed("register_agent", map[string]any{"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$1", "pane_id": "%1"})
	seed("become", map[string]any{"from": "muster-2", "to": "alias-routing"})
	seed("register_agent", map[string]any{"alias": "peer", "socket_path": "/tmp/sock", "session_id": "$9"})
	seed("send_message", map[string]any{"from": "peer", "to_kind": "agent", "to_target": "alias-routing", "subject": "s", "body": "b"})

	var buf bytes.Buffer
	if err := cmdHook([]string{"Stop"}, strings.NewReader(`{}`), &buf); err != nil {
		t.Fatal(err)
	}
	outStr := buf.String()
	if !strings.Contains(outStr, "alias-routing") {
		t.Fatalf("drain reason must name the claimed alias:\n%s", outStr)
	}
}

// TestResumeReclaimsClaimedName: the v0.8.0 resume chain composed with
// become — kill the tmux session, resume env-stripped in a new one, and the
// CLAIMED alias (not the seed) reconnects onto the new tuple.
func TestResumeReclaimsClaimedName(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{
		"#{session_id}": "$NEW", "#{session_name}": "muster-9", "#{session_created}": "222",
	})
	t.Cleanup(func() { tmuxenv.Run = prev })
	seed := func(op string, args map[string]any) {
		t.Helper()
		if _, err := callData(op, args); err != nil {
			t.Fatal(err)
		}
	}
	seed("register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/tmp/sock", "session_id": "$OLD",
		"session_created": 111, "harness_session_id": "uuid-42",
	})
	seed("become", map[string]any{"from": "muster-2", "to": "alias-routing"})

	var buf bytes.Buffer
	if err := cmdHook([]string{"SessionStart"}, strings.NewReader(`{"source":"resume","session_id":"uuid-42"}`), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "reconnected as 'alias-routing'") {
		t.Fatalf("resume summary:\n%s", buf.String())
	}
	ag, ok := hookGetAgent("alias-routing")
	if !ok || ag.Departed || ag.SessionID != "$NEW" {
		t.Fatalf("claimed row after resume = %+v (ok=%v)", ag, ok)
	}
	if seedRow, _ := hookGetAgent("muster-2"); !seedRow.Departed {
		t.Fatalf("seed must stay retired after resume, got %+v", seedRow)
	}
}
```

(The Stop test's stub map: extend with whatever formats the walked/env Stop path queries — mirror `TestHookStopUnreadEmitsBlockDecision`'s working map. IsSessionAlive for the resume test's $OLD tuple: the row is DEPARTED, so the collision predicate skips liveness — no extra stub needed.)

- [ ] **Step 2: Run** — expect FAIL until wired end-to-end (these may pass immediately if Tasks 1–4 are complete — that's fine; they're regression armor, note it in the report).
- [ ] **Step 3: Skill edit** — in the "Register at the start — and on resume" section of `.claude/skills/muster-coordination/SKILL.md`, append:

```markdown
Your seed alias (your tmux session's name) is a placeholder, not your
identity. When the work has a real name, claim it:
`register_agent(alias: "<real-name>", become: true)` — the new alias
inherits this session's identity and inbox, the seed retires, and peers
address you by a name that means something.
```

- [ ] **Step 4: Run** `just verify` — expect PASS.
- [ ] **Step 5: Commit** — `feat: become integration armor + skill teaches the claim`

---

### Task 6: Gate, PR, live check

- [ ] **Step 1:** `just verify` — green.
- [ ] **Step 2:** Push; PR to `dev`: `feat: muster become — claim your name`. Body links the spec, states the claim-and-retire model, the rejected rename-with-history alternative and why (operator: old stuff doesn't matter; written-reference promise holds post-claim), and the surfaces touched.
- [ ] **Step 3 (controller, post-merge):** scripted live check on an isolated MUSTER_HOME (same rig as v0.8.0's): seed a tmux session, become, verify roster shows only the claimed name, send mail to it, then the resume flow reclaims the claimed name. Operator's own `become` on a real session is the final feel-check.
- [ ] **Step 4:** VERSION bump rides the release train when the operator says so.

---

## Self-review notes (spec ↔ plan)

- Spec mechanism 1 (op: guards/clone/watermark/retire/journal/badges/response) → Tasks 1–2. The unread-source ambiguity (UnreadCount(to) vs SessionUnread) is called out in Task 2 Step 1 with the decision rule.
- Spec mechanism 2 (CLI, hookCapture reuse, --from on split) → Task 3.
- Spec mechanism 3 (MCP become flag, refusal advertises it, unregistered degrades to plain register) → Task 4.
- Spec mechanism 4 (skill text only; no wrapper/hook/dotfiles changes) → Task 5 Step 3; no task touches hooks or the wrapper.
- Spec interplay checks (straggler drain, resume-after-become) → Task 5 tests; SessionEnd/ghost-reap/gc need no new code (clone carries real session_created; departed seed is purgeable) — final review verifies by reading, not new tasks.
- Exact strings centralized in Global Constraints; all tasks reference them.
