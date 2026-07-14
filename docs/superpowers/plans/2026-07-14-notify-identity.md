# Mailbox (`@muster_inbox`) — Implementation Plan (muster Go)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

> **RESCOPED 2026-07-14.** The launch-wrapper + `tmuxenv` MUSTER_* fallback that this
> branch originally carried are **dropped**: Codex's interactive TUI *does* run hooks
> (the earlier "no hooks" finding was a false negative — see project memory / PR #10),
> so SessionStart-hook registration works for both agents and the wrapper is
> unnecessary. Hooks + Codex nudge-submit + self-resolving inbox are owned by
> `feat/codex-nudge-submit`. **This branch now = the mailbox only.**

**Goal:** Give muster a distinct, persistent unread-mail signal. The store tracks
unread state; the daemon sets `@muster_inbox=<unread count>` on the recipient's tmux
session on activity and clears it when the agent reads its inbox. The dotfiles
render `📬<count>` (applied separately by the controller — render only, no wrapper).

**Why it matters beyond the operator's 📬:** `@muster_inbox` is the signal the
self-resolving `Stop` hook (on `feat/codex-nudge-submit`) reads at turn-end to decide
whether to auto-drain the inbox. This branch produces that signal.

**Architecture:** Additive. The daemon core stays tmux-agnostic — notify only via the
injected `wake.Notifier`.

## Global Constraints

- cgo-free; `just verify` green (gofmt, golangci-lint **0 issues** — run the linter per task, not just `go test`), `-race`, build. `TMPDIR=/tmp` on macOS.
- Exported symbols get doc comments (revive); ignored params `_`; `switch` over long if/else (gocritic); `context.TODO()` not nil ctx (staticcheck).
- `internal/store` must NOT import `internal/tmuxenv`; daemon touches tmux only via `wake.Notifier`.
- Tests inject seams (fake `wake.Notifier`), never real tmux; no `t.Parallel()` while mutating package vars; `mustertest.ShortHome()` for daemon sockets.
- `@muster_inbox` holds an integer count; on count `0`/clear it is **unset** (not `"0"`), so the dotfiles `#{?@muster_inbox,…}` conditional reads falsy (never `📬0`).
- **Rebase note:** once PR #10 (`feat/codex-nudge-submit`) merges to `dev`, rebuild this branch on the updated `dev` before the final review (it touches `nudge.go`; this branch touches `store`/`wake`/`daemon`/`main` — no overlap, but stay current).

---

### Task 1: store — `last_read_at`, `UnreadCount`, `MarkRead`

**Files:** Modify `internal/store/schema.sql`, `internal/store/store.go` (migration), `internal/store/agents.go`; Test `internal/store/threads_test.go` (or agents_test.go).

**Interfaces produced (consumed by Task 2):** `func (s *Store) UnreadCount(alias string) (int, error)`, `func (s *Store) MarkRead(alias string) error`.

- [ ] **Step 1: Write the failing test**

```go
func TestUnreadCountAndMarkRead(t *testing.T) {
	s := openTestStore(t) // existing helper
	if err := s.RegisterAgent(Agent{Alias: "a", Role: "r"}); err != nil { t.Fatal(err) }
	for _, b := range []string{"one", "two"} {
		if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "x", ToKind: "agent", ToTarget: "a"}, b); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.UnreadCount("a"); err != nil || n != 2 {
		t.Fatalf("unread before read = %d (%v), want 2", n, err)
	}
	if err := s.MarkRead("a"); err != nil { t.Fatal(err) }
	if n, _ := s.UnreadCount("a"); n != 0 {
		t.Fatalf("unread after MarkRead = %d, want 0", n)
	}
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "x", ToKind: "agent", ToTarget: "a"}, "three"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("a"); n != 1 {
		t.Fatalf("unread after new msg = %d, want 1", n)
	}
}
```
(Use the store package's existing temp-store helper; match its real name.)

- [ ] **Step 2: Run to verify fail** — `TMPDIR=/tmp go test ./internal/store/ -run TestUnreadCountAndMarkRead` → FAIL.

- [ ] **Step 3: Schema + migration**

`schema.sql` agents table — add after `label_manual`:
```sql
    last_read_at  INTEGER NOT NULL DEFAULT 0,
```
`store.go` `migrate()` — add to the `alters` slice:
```go
		`ALTER TABLE agents ADD COLUMN last_read_at INTEGER NOT NULL DEFAULT 0`,
```

- [ ] **Step 4: Implement `UnreadCount` + `MarkRead`** (in `agents.go`)

```go
// UnreadCount returns how many threads addressed to alias have activity newer
// than the agent's last inbox read. Recipient matching mirrors Inbox.
func (s *Store) UnreadCount(alias string) (int, error) {
	var n int
	err := s.db.QueryRow(`
SELECT COUNT(*) FROM threads
WHERE ((to_kind='agent'     AND to_target=?)
    OR (to_kind='role'      AND to_target != '' AND to_target=(SELECT role FROM agents WHERE alias=?))
    OR (to_kind='broadcast'))
  AND updated_at > COALESCE((SELECT last_read_at FROM agents WHERE alias=?), 0)`,
		alias, alias, alias).Scan(&n)
	return n, err
}

// MarkRead records that alias has read its inbox up to now.
func (s *Store) MarkRead(alias string) error {
	_, err := s.db.Exec(`UPDATE agents SET last_read_at=? WHERE alias=?`, clock.NowMillis(), alias)
	return err
}
```

- [ ] **Step 5: Run tests + lint** — `TMPDIR=/tmp go test ./internal/store/` PASS; `TMPDIR=/tmp golangci-lint run ./internal/store/` → 0 issues (standalone, check exit code).
- [ ] **Step 6: Commit** — `feat(store): unread-count via last_read_at (UnreadCount/MarkRead)`.

---

### Task 2: wake + daemon — count-bearing `@muster_inbox` notify, clear on read

**Files:** Modify `internal/wake/wake.go`, `internal/daemon/daemon.go`, `cmd/muster/main.go`; update every `wake.Notifier` implementer/fake; Tests in `internal/wake/` and `internal/daemon/`.

**Interfaces:** `Notifier.Notify` gains a `count int` param — all implementers + call sites update.

- [ ] **Step 1: Write/adjust failing tests**

Wake test (`internal/wake/wake_test.go`):
```go
func TestNotifySetsCountAndUnsetsOnZero(t *testing.T) {
	var cmds [][]string
	n := TmuxNotifier{Option: "@muster_inbox", Run: func(_ context.Context, args ...string) error {
		cmds = append(cmds, args); return nil
	}}
	_ = n.Notify("/s", "$1", 3)
	if cmds[0][4] != "@muster_inbox" || cmds[0][5] != "3" {
		t.Fatalf("set cmd = %v", cmds[0])
	}
	cmds = nil
	_ = n.Notify("/s", "$1", 0)
	if joined := strings.Join(cmds[0], " "); !strings.Contains(joined, "-u") || !strings.Contains(joined, "@muster_inbox") {
		t.Fatalf("zero should unset, got %v", cmds[0])
	}
}
```
Daemon: update the existing fake `Notifier` in `internal/daemon` tests to the new `Notify(sock, sess string, count int) error` signature, and assert `get_inbox` marks read (a subsequent `UnreadCount` is 0).

- [ ] **Step 2: Run to verify fail / compile error.**

- [ ] **Step 3: wake.go — count param**

```go
type Notifier interface {
	Notify(socketPath, sessionID string, count int) error
	Clear(socketPath, sessionID string) error
}

// Notify sets the option to count (unsetting it when count <= 0), then repaints
// the session's clients. Best-effort.
func (n TmuxNotifier) Notify(socketPath, sessionID string, count int) error {
	if count <= 0 {
		_ = n.Clear(socketPath, sessionID)
	} else if err := n.run("-S", socketPath, "set-option", "-t", sessionID, n.Option, strconv.Itoa(count)); err != nil {
		return err
	}
	_ = n.refreshSessionClients(socketPath, sessionID)
	return nil
}
```
Add `"strconv"` import.

- [ ] **Step 4: daemon.go — compute count, mark read**

`notifyForThread` recipient loop:
```go
	for alias := range recipients {
		a, ok := byAlias[alias]
		if !ok || a.SocketPath == "" || a.SessionID == "" {
			continue
		}
		count, err := d.s.UnreadCount(alias)
		if err != nil {
			continue
		}
		_ = d.n.Notify(a.SocketPath, a.SessionID, count)
	}
```
`get_inbox` case — mark read before clearing:
```go
	case "get_inbox":
		alias := str(a, "alias")
		threads, err := d.s.Inbox(alias)
		if err != nil {
			return fail(err)
		}
		_ = d.s.MarkRead(alias)
		if d.n != nil {
			if ag, ok, _ := d.s.GetAgent(alias); ok && ag.SocketPath != "" && ag.SessionID != "" {
				_ = d.n.Clear(ag.SocketPath, ag.SessionID)
			}
		}
		return ok(threads)
```

- [ ] **Step 5: main.go — option name** — `wake.NewTmuxNotifier("@claude_attn", …)` → `wake.NewTmuxNotifier("@muster_inbox", …)`.

- [ ] **Step 6: Fix all other `Notifier` implementers/call sites** (grep `Notify(`), then `TMPDIR=/tmp go build ./...`.

- [ ] **Step 7: Run tests + lint** — `TMPDIR=/tmp go test ./...` PASS; `TMPDIR=/tmp golangci-lint run ./...` → 0 issues.
- [ ] **Step 8: Commit** — `feat(daemon,wake): count-bearing @muster_inbox notify; clear on inbox read`.

---

## Final verification

`TMPDIR=/tmp GODEBUG=netdns=go just verify` green → whole-branch review → merge to `dev`.
**After merge:** rebuild `~/.local/bin/muster`, apply the dotfiles `📬` render (title + status-left, render only — no wrapper), restart the daemon, and confirm `@muster_inbox` lights on inbound and clears on inbox-read for a live agent. Coordinate with `feat/codex-nudge-submit` so the self-resolving Stop hook reads `@muster_inbox` as its unread signal.
