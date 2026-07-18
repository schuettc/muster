# tmux Registered-Agent Badge (`@muster_agent`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The daemon pushes each tmux session's registered-alias list into a `@muster_agent` tmux option on every registration change, and the operator's status bar renders it as a dimmed `📬 <alias>` badge — ambient proof of registration plus the exact copy/pasteable name to message.

**Architecture:** Extend `internal/wake` (the daemon's only tmux contact for options) with a `SetAgents` surface on the existing `Notifier` interface; extend the daemon's existing `reconcileBadge` identity-change hook (already called on register old+new tuple, deregister, purge) to also recompute and push the session's live alias list. Display is a two-line `.tmux.conf` change in the separate dotfiles repo.

**Tech Stack:** Go (muster repo, worktree `~/GitHub/schuettc/muster-wt-agent-badge`, branch `feat/tmux-agent-badge` off `dev`), tmux format strings (dotfiles repo `~/Users/courtschuett/dotfiles`).

**Spec:** `docs/superpowers/specs/2026-07-18-tmux-agent-badge-design.md` (this worktree).

## Global Constraints

- All muster work happens in the worktree `~/GitHub/schuettc/muster-wt-agent-badge` — NEVER the primary clone `~/GitHub/schuettc/muster` (it stays on `dev` untouched).
- Gate before every commit: `just verify` (gofmt, golangci-lint, `go test -race`, build), run from the worktree root.
- Daemon and store stay tmux-agnostic: tmux only via `internal/wake` / `internal/tmuxenv` (repo CLAUDE.md hard rule).
- Option name `@muster_agent`; default lives in `internal/wake` as an overridable struct field (knobs, not constants).
- All wake pushes are best-effort: errors ignored by callers, never fail a bus op.
- Departed (tombstoned) agents are excluded from the badge alias list.
- Do NOT change the existing `session_aliases` daemon op (it intentionally includes departed agents for the Stop-hook drain; out of scope).
- The `Notifier` interface gains one method; its only implementations are `wake.TmuxNotifier` and the test fakes in `internal/daemon/wake_wiring_test.go` (`fakeNotifier`, plus `blockingNotifier` which embeds it) — verified by grep 2026-07-18.

---

### Task 1: `wake.SetAgents` — the tmux option surface

**Files:**
- Modify: `internal/wake/wake.go`
- Test: `internal/wake/wake_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `Notifier` interface method `SetAgents(socketPath, sessionID string, aliases []string) error`; `TmuxNotifier` field `AgentOption string` (empty → default `"@muster_agent"`). Empty `aliases` unsets the option. Task 2's daemon code and fakes rely on exactly this signature.

- [ ] **Step 1: Write the failing tests**

Append to `internal/wake/wake_test.go`:

```go
func TestSetAgentsSetsCommaJoinedOptionAndRefreshes(t *testing.T) {
	var calls [][]string
	n := TmuxNotifier{Option: "@muster_inbox", Timeout: time.Second, Run: func(_ context.Context, args ...string) error {
		calls = append(calls, args)
		return nil
	}}
	if err := n.SetAgents("/sock", "$3", []string{"backend", "api"}); err != nil {
		t.Fatalf("SetAgents: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("no tmux calls")
	}
	set := strings.Join(calls[0], " ")
	if !strings.Contains(set, "-S /sock") || !strings.Contains(set, "set-option") || !strings.Contains(set, "-t $3") || !strings.Contains(set, "@muster_agent backend,api") {
		t.Fatalf("first call not a socket-aware @muster_agent set: %v", calls[0])
	}
	for _, c := range calls {
		if strings.Contains(strings.Join(c, " "), "send-keys") {
			t.Fatalf("SetAgents must NEVER send-keys, got: %v", c)
		}
	}
}

func TestSetAgentsEmptyUnsetsOption(t *testing.T) {
	var calls [][]string
	n := TmuxNotifier{Run: func(_ context.Context, args ...string) error {
		calls = append(calls, args)
		return nil
	}}
	if err := n.SetAgents("/sock", "$3", nil); err != nil {
		t.Fatalf("SetAgents(empty): %v", err)
	}
	got := strings.Join(calls[0], " ")
	if !strings.Contains(got, "-u") || !strings.Contains(got, "@muster_agent") || !strings.Contains(got, "-t $3") {
		t.Fatalf("empty aliases must unset @muster_agent: %v", calls[0])
	}
}

func TestSetAgentsHonorsAgentOptionOverride(t *testing.T) {
	var calls [][]string
	n := TmuxNotifier{AgentOption: "@custom_agent", Run: func(_ context.Context, args ...string) error {
		calls = append(calls, args)
		return nil
	}}
	_ = n.SetAgents("/sock", "$3", []string{"x"})
	if got := strings.Join(calls[0], " "); !strings.Contains(got, "@custom_agent x") {
		t.Fatalf("AgentOption override ignored: %v", calls[0])
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd ~/GitHub/schuettc/muster-wt-agent-badge && go test ./internal/wake/ -run TestSetAgents -v`
Expected: FAIL to compile — `n.SetAgents undefined` / `unknown field AgentOption`.

- [ ] **Step 3: Implement**

In `internal/wake/wake.go`:

Add to the `Notifier` interface (after `Clear`):

```go
	// SetAgents writes the session's registered-alias list into the agent
	// badge option (comma-joined), unsetting it when the list is empty — the
	// operator's ambient "registered as X" indicator. Best-effort like
	// Notify/Clear.
	SetAgents(socketPath, sessionID string, aliases []string) error
```

Add the `AgentOption` field to `TmuxNotifier` (after `Option`):

```go
	AgentOption string // agent-badge option; "" → DefaultAgentOption
```

Add near the top of the file:

```go
// DefaultAgentOption is the tmux user option carrying a session's registered
// alias list when TmuxNotifier.AgentOption is unset.
const DefaultAgentOption = "@muster_agent"
```

Add the method (after `Clear`):

```go
// SetAgents sets the agent-badge option to the comma-joined alias list
// (unsetting it when the list is empty), then repaints the session's clients.
// Aliases are joined verbatim — callers pass them sorted and deduplicated.
func (n TmuxNotifier) SetAgents(socketPath, sessionID string, aliases []string) error {
	opt := n.AgentOption
	if opt == "" {
		opt = DefaultAgentOption
	}
	var err error
	if len(aliases) == 0 {
		err = n.run("-S", socketPath, "set-option", "-t", sessionID, "-u", opt)
	} else {
		err = n.run("-S", socketPath, "set-option", "-t", sessionID, opt, strings.Join(aliases, ","))
	}
	if err != nil {
		return err
	}
	_ = n.refreshSessionClients(socketPath, sessionID)
	return nil
}
```

Add `"strings"` to the import block.

- [ ] **Step 4: Run the wake tests**

Run: `cd ~/GitHub/schuettc/muster-wt-agent-badge && go test ./internal/wake/ -v`
Expected: all PASS (new tests plus the four existing ones).

Note: `go build ./...` will now FAIL in `internal/daemon` — the test fakes don't implement `SetAgents` yet. That is expected and fixed in Task 2; do not run `just verify` or commit yet. (Compile-breaking interface growth is why Tasks 1 and 2 share one commit boundary at the end of Task 2.)

### Task 2: Daemon pushes the alias list on every identity change

**Files:**
- Modify: `internal/daemon/daemon.go` (the `reconcileBadge` block around line 264 and a new helper next to the `session_aliases` op)
- Test: `internal/daemon/wake_wiring_test.go`

**Interfaces:**
- Consumes: `Notifier.SetAgents(socketPath, sessionID string, aliases []string) error` from Task 1.
- Produces: unexported daemon helpers `sessionAliasesFor` and `pushSessionAgents`; `fakeNotifier` gains a recorded `agentSets []agentSet` log that Task 2's tests read. No cross-repo consumers.

- [ ] **Step 1: Make the fakes implement the grown interface**

In `internal/daemon/wake_wiring_test.go`, add after the `notifierCall` type:

```go
// agentSet records one SetAgents push: which session, with which alias list
// (nil/empty = unset).
type agentSet struct {
	session string
	aliases []string
}
```

Add the field `agentSets []agentSet` to `fakeNotifier` (after `log`), and the method after `Clear`:

```go
func (f *fakeNotifier) SetAgents(_, sessionID string, aliases []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agentSets = append(f.agentSets, agentSet{session: sessionID, aliases: append([]string(nil), aliases...)})
	return nil
}
```

Add the snapshot helper after `snapLog`:

```go
func (f *fakeNotifier) snapAgentSets() []agentSet {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]agentSet, len(f.agentSets))
	copy(out, f.agentSets)
	return out
}
```

(`blockingNotifier` embeds `fakeNotifier`, so it inherits the method.)

- [ ] **Step 2: Verify everything compiles again**

Run: `cd ~/GitHub/schuettc/muster-wt-agent-badge && go build ./... && go vet ./internal/daemon/`
Expected: clean — the interface is satisfied everywhere again.

- [ ] **Step 3: Write the failing wiring tests**

Append to `internal/daemon/wake_wiring_test.go`:

```go
// lastAgentSetFor returns the most recent SetAgents push for session, or nil.
func lastAgentSetFor(sets []agentSet, session string) *agentSet {
	for i := len(sets) - 1; i >= 0; i-- {
		if sets[i].session == session {
			return &sets[i]
		}
	}
	return nil
}

// TestRegisterPushesAgentBadge: registering pushes the session's sorted alias
// list; a sibling alias on the same tuple re-pushes the combined list.
func TestRegisterPushesAgentBadge(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "solo", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	got := lastAgentSetFor(n.snapAgentSets(), "$9")
	if got == nil || !slices.Equal(got.aliases, []string{"solo"}) {
		t.Fatalf("register must push [solo] to $9, got %+v", got)
	}
	call(t, sock, "register_agent", map[string]any{"alias": "chosen", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	got = lastAgentSetFor(n.snapAgentSets(), "$9")
	if got == nil || !slices.Equal(got.aliases, []string{"chosen", "solo"}) {
		t.Fatalf("sibling register must push sorted [chosen solo] to $9, got %+v", got)
	}
}

// TestDeregisterUnsetsAgentBadgeWhenLastAliasLeaves: tombstoning the last
// alias pushes an empty list (unset); a surviving sibling keeps the badge
// with the remainder.
func TestDeregisterUnsetsAgentBadgeWhenLastAliasLeaves(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "solo", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	call(t, sock, "register_agent", map[string]any{"alias": "twin", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	call(t, sock, "deregister_agent", map[string]any{"alias": "twin"})
	got := lastAgentSetFor(n.snapAgentSets(), "$9")
	if got == nil || !slices.Equal(got.aliases, []string{"solo"}) {
		t.Fatalf("deregister of one sibling must push remainder [solo], got %+v", got)
	}
	call(t, sock, "deregister_agent", map[string]any{"alias": "solo"})
	got = lastAgentSetFor(n.snapAgentSets(), "$9")
	if got == nil || len(got.aliases) != 0 {
		t.Fatalf("deregister of the last alias must push empty (unset), got %+v", got)
	}
}

// TestPurgeAgentUpdatesAgentBadge: hard-delete reconciles the badge exactly
// like deregister.
func TestPurgeAgentUpdatesAgentBadge(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "gone", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$4"})
	call(t, sock, "purge_agent", map[string]any{"alias": "gone"})
	got := lastAgentSetFor(n.snapAgentSets(), "$4")
	if got == nil || len(got.aliases) != 0 {
		t.Fatalf("purge of the last alias must push empty (unset), got %+v", got)
	}
}

// TestReRegisterMovesAgentBadgeBetweenSessions: an alias moving to a new
// tuple updates BOTH sessions' badges — the old one loses it, the new one
// gains it.
func TestReRegisterMovesAgentBadgeBetweenSessions(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "roamer", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$1"})
	call(t, sock, "register_agent", map[string]any{"alias": "roamer", "role": "peer", "model_type": "claude", "socket_path": "/s", "session_id": "$9"})
	sets := n.snapAgentSets()
	if got := lastAgentSetFor(sets, "$1"); got == nil || len(got.aliases) != 0 {
		t.Fatalf("old session $1 must end empty after the move, got %+v", got)
	}
	if got := lastAgentSetFor(sets, "$9"); got == nil || !slices.Equal(got.aliases, []string{"roamer"}) {
		t.Fatalf("new session $9 must end with [roamer], got %+v", got)
	}
}

// TestRegisterWithoutTmuxTuplePushesNoAgentBadge: no tuple → nothing to push.
func TestRegisterWithoutTmuxTuplePushesNoAgentBadge(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "headless", "role": "peer", "model_type": "claude"})
	if sets := n.snapAgentSets(); len(sets) != 0 {
		t.Fatalf("tuple-less register must push no agent badge, got %+v", sets)
	}
}
```

- [ ] **Step 4: Run tests to verify they fail**

Run: `cd ~/GitHub/schuettc/muster-wt-agent-badge && go test ./internal/daemon/ -run 'AgentBadge|MovesAgentBadge' -v`
Expected: FAIL — `lastAgentSetFor(...)` finds no pushes (`got <nil>`); the daemon never calls `SetAgents` yet.

- [ ] **Step 5: Implement the daemon push**

In `internal/daemon/daemon.go`, replace the existing `reconcileBadge` function with:

```go
// reconcileBadge is setSessionBadge for identity-change call sites
// (register/deregister/purge) that don't have a thread/journal-row shape to
// produce: best-effort, silent on an empty tuple or a nil notifier (there is
// no tmux badge to reconcile in either case). Identity changes are also the
// only moments a session's alias list can change, so this additionally
// pushes the agent badge (@muster_agent) — the operator's ambient
// "registered as X" indicator.
func (d *Daemon) reconcileBadge(socketPath, sessionID string) {
	if d.n == nil || socketPath == "" || sessionID == "" {
		return
	}
	_, _ = d.setSessionBadge(socketPath, sessionID)
	d.pushSessionAgents(socketPath, sessionID)
}

// sessionAliasesFor returns the sorted, deduplicated LIVE alias list for a
// session tuple — departed (tombstoned) agents excluded, since the agent
// badge advertises who is currently addressable there. Distinct from the
// session_aliases op, which includes departed aliases on purpose (their
// unread mail still needs draining).
func (d *Daemon) sessionAliasesFor(socketPath, sessionID string) ([]string, error) {
	agents, err := d.s.ListAgents()
	if err != nil {
		return nil, err
	}
	aliases := []string{}
	for _, ag := range agents {
		if ag.SocketPath == socketPath && ag.SessionID == sessionID && !ag.Departed {
			aliases = append(aliases, ag.Alias)
		}
	}
	sort.Strings(aliases)
	return compactStrings(aliases), nil
}

// pushSessionAgents recomputes and pushes the agent badge under the session's
// lock (same serialization contract as setSessionBadge: last recompute wins,
// no stale interleaved write). Best-effort: a store or tmux error leaves the
// previous badge in place — the roster stays authoritative.
func (d *Daemon) pushSessionAgents(socketPath, sessionID string) {
	mu := d.sessionLock(socketPath, sessionID)
	mu.Lock()
	defer mu.Unlock()
	aliases, err := d.sessionAliasesFor(socketPath, sessionID)
	if err != nil {
		return
	}
	_ = d.n.SetAgents(socketPath, sessionID, aliases)
}
```

No call-site changes needed: `handleRegisterAgent` (old + new tuple), `deregister_agent`, and `purge_agent` already call `reconcileBadge`.

- [ ] **Step 6: Run the new tests, then the full daemon package**

Run: `cd ~/GitHub/schuettc/muster-wt-agent-badge && go test ./internal/daemon/ -run 'AgentBadge|MovesAgentBadge' -v && go test -race ./internal/daemon/`
Expected: new tests PASS, full package PASS (existing badge/notify tests unaffected — `SetAgents` is a separate log the old assertions never read).

- [ ] **Step 7: Full gate and commit (Tasks 1+2 together)**

Run: `cd ~/GitHub/schuettc/muster-wt-agent-badge && just verify`
Expected: gofmt clean, lint clean, all tests PASS, build OK.

```bash
cd ~/GitHub/schuettc/muster-wt-agent-badge
git add internal/wake/wake.go internal/wake/wake_test.go internal/daemon/daemon.go internal/daemon/wake_wiring_test.go
git commit -m "feat(wake): @muster_agent badge — daemon pushes session alias list on identity changes"
```

### Task 3: Sandboxed end-to-end verify, VERSION bump, PR to dev

**Files:**
- Modify: `VERSION` (currently `0.6.0` → `0.7.0`; new feature = minor bump)

**Interfaces:**
- Consumes: the worktree-built binary with Tasks 1–2.
- Produces: a `feat/tmux-agent-badge → dev` PR. The live machine keeps its released binary; rollout happens when dev is promoted to main and the dotfiles installer rebuilds.

- [ ] **Step 1: Build a sandbox binary (never install over `~/.local/bin/muster`)**

```bash
cd ~/GitHub/schuettc/muster-wt-agent-badge
go build -o /private/tmp/claude-501/-Users-courtschuett-dotfiles/e74a97d0-811f-49fa-82d1-fc2197d88184/scratchpad/muster-badge ./cmd/muster
```

- [ ] **Step 2: End-to-end on a throwaway tmux server + isolated MUSTER_HOME**

```bash
SCRATCH=/private/tmp/claude-501/-Users-courtschuett-dotfiles/e74a97d0-811f-49fa-82d1-fc2197d88184/scratchpad
export MUSTER_HOME=$SCRATCH/mhome; mkdir -p "$MUSTER_HOME"
TSOCK=$SCRATCH/ttmux
tmux -S "$TSOCK" new-session -d -s sandbox
$SCRATCH/muster-badge serve >"$SCRATCH/serve.log" 2>&1 &
SERVE_PID=$!
sleep 1
# Register an agent whose tuple points at the sandbox session:
SID=$(tmux -S "$TSOCK" display-message -p -t sandbox '#{session_id}')
$SCRATCH/muster-badge debug call register_agent alias=backend role=peer model_type=claude socket_path="$TSOCK" session_id="$SID"
tmux -S "$TSOCK" show-options -t sandbox -v @muster_agent   # expect: backend
$SCRATCH/muster-badge debug call register_agent alias=api role=peer model_type=claude socket_path="$TSOCK" session_id="$SID"
tmux -S "$TSOCK" show-options -t sandbox -v @muster_agent   # expect: api,backend
$SCRATCH/muster-badge debug call deregister_agent alias=api
tmux -S "$TSOCK" show-options -t sandbox -v @muster_agent   # expect: backend
$SCRATCH/muster-badge debug call deregister_agent alias=backend
tmux -S "$TSOCK" show-options -t sandbox -qv @muster_agent  # expect: empty output (unset)
kill $SERVE_PID; tmux -S "$TSOCK" kill-server; unset MUSTER_HOME
```

Expected outputs as annotated. If `debug call` syntax differs (check `muster debug --help`), adapt the arg form but keep the op/args identical.

- [ ] **Step 3: Bump VERSION and commit**

Set `VERSION` to `0.7.0`.

```bash
cd ~/GitHub/schuettc/muster-wt-agent-badge
git add VERSION
git commit -m "chore: bump VERSION to 0.7.0 (@muster_agent badge)"
```

- [ ] **Step 4: Push and open the PR to dev**

```bash
cd ~/GitHub/schuettc/muster-wt-agent-badge
git push -u origin feat/tmux-agent-badge
gh pr create --base dev --title "feat: @muster_agent tmux badge — ambient registered-alias indicator" --body "$(cat <<'EOF'
Daemon pushes each session's live alias list into the @muster_agent tmux
option on every identity change (register old+new tuple, deregister, purge),
via a new wake.SetAgents surface. Empty list unsets. Departed agents
excluded. Spec: docs/superpowers/specs/2026-07-18-tmux-agent-badge-design.md.

Operator display lands separately in dotfiles (.tmux.conf).
EOF
)"
```

Expected: PR created; CI runs `just verify`. Trust CI, not just the local gate — read the job log on failure.

### Task 4: Dotfiles display — dimmed `📬 alias` in status bar and title

**Files:**
- Modify: `/Users/courtschuett/dotfiles/.tmux.conf:54` (set-titles-string) and `/Users/courtschuett/dotfiles/.tmux.conf:152` (status-left)

**Interfaces:**
- Consumes: the `@muster_agent` session option (Task 2's daemon behavior). Renders fine before the muster release too — the option is simply absent, so the segment is empty.
- Produces: nothing downstream.

- [ ] **Step 1: Edit the two format strings**

Line 54, current:

```
set -g set-titles-string '#{?@claude_attn,🔔 ,}#{?@muster_inbox,📬#{@muster_inbox} ,}#S#{?@claude_task, · #{@claude_task},}'
```

becomes (unread badge wins the slot; otherwise the registered alias):

```
set -g set-titles-string '#{?@claude_attn,🔔 ,}#{?@muster_inbox,📬#{@muster_inbox} ,#{?@muster_agent,📬#{@muster_agent} ,}}#S#{?@claude_task, · #{@claude_task},}'
```

Line 152, current:

```
set -g status-left  "#(~/dotfiles/bin/tmux-attention.sh)#(~/dotfiles/bin/tmux-session-color.sh '#S')#{?@muster_inbox,#[fg=#1e1e2e#,bg=#89dceb#,bold] 📬#{@muster_inbox} #[default] ,}#{?@claude_task,#[fg=#b4befe]#{@claude_task} #[default],}"
```

becomes (dimmed segment in Catppuccin Mocha overlay0 `#6c7086`, no background block):

```
set -g status-left  "#(~/dotfiles/bin/tmux-attention.sh)#(~/dotfiles/bin/tmux-session-color.sh '#S')#{?@muster_inbox,#[fg=#1e1e2e#,bg=#89dceb#,bold] 📬#{@muster_inbox} #[default] ,#{?@muster_agent,#[fg=#6c7086]📬 #{@muster_agent} #[default],}}#{?@claude_task,#[fg=#b4befe]#{@claude_task} #[default],}"
```

Also update the comment block above line 54 (lines 48–53) to mention the third state — append one line:

```
# When there's no unread badge but the session has a registered muster agent,
# show its alias(es) via @muster_agent (dimmed in the status bar) — the exact
# name to message it on the bus.
```

- [ ] **Step 2: Validate the format strings parse and render**

```bash
tmux source-file ~/dotfiles/.tmux.conf
tmux set-option @muster_agent "backend,api"
tmux display-message -p '#{T:status-left}' | head -c 200; echo
tmux set-option -u @muster_agent
```

Expected: `source-file` silent (no parse error); with the option set, the rendered status-left contains `📬 backend,api`; after unset it doesn't. Also eyeball the live status bar during the set/unset — the dimmed badge should appear and vanish.

- [ ] **Step 3: Commit (dotfiles repo)**

```bash
cd ~/dotfiles
git add .tmux.conf
git commit -m "feat(tmux): dimmed 📬 registered-agent badge from @muster_agent"
```

Note: until the muster PR is released to main and the dotfiles muster package rebuilds the live binary, the badge simply never appears — the display change is safe to land first.

---

## Self-Review Notes

- Spec coverage: wake surface (Task 1), register/deregister/purge push + move-between-sessions + non-tmux skip + departed exclusion (Task 2), best-effort semantics (implementation comments + no error propagation), testing section (Tasks 1–2 tests mirror wake_test.go / wake_wiring_test.go), display + title (Task 4), out-of-scope items untouched (session_aliases op explicitly frozen in Global Constraints).
- The one intentional deviation from bite-size: Tasks 1 and 2 share a commit because growing the `Notifier` interface breaks `internal/daemon` compilation until the fakes catch up — an interleaved commit would not build.
