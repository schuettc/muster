# Notify + Identity Foundation — Implementation Plan (muster Go)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** The muster-side foundation for the mailbox + wrapper design: `tmuxenv` gains a `MUSTER_*` env fallback (so capture works without `$TMUX`), the store tracks unread state, and the daemon's notify carries an unread **count** in a new `@muster_inbox` option (cleared on inbox read). The dotfiles wrapper + `📬` render are applied separately (not in this plan).

**Architecture:** Additive, same layering as before. `internal/tmuxenv` stays the one capture path; the daemon core stays tmux-agnostic (notify only via the injected `wake.Notifier`).

**Tech Stack:** Go 1.26, cgo-free, `modernc.org/sqlite`.

## Global Constraints

- cgo-free; `just verify` green (gofmt, golangci-lint 0 issues, `go test -race`, build). Run the linter, not just `go test` — prefix macOS runs with `TMPDIR=/tmp`.
- Exported symbols get doc comments (revive); ignored params `_`; prefer `switch` over long if/else chains (gocritic); pass `context.TODO()` not nil Context (staticcheck).
- `internal/daemon` and `internal/store` must NOT import `internal/tmuxenv`.
- Tests inject seams (`tmuxenv.Run`, fake `wake.Notifier`), never real tmux; no `t.Parallel()` while mutating package vars/env; use `internal/mustertest.ShortHome()` for daemon sockets on macOS.
- The `@muster_inbox` option holds an integer count; on count `0`/clear it must be **unset** (not set to `"0"`), so the dotfiles `#{?@muster_inbox,…}` conditional reads falsy.

---

### Task 1: `internal/tmuxenv` — `MUSTER_*` env fallback

**Files:** Modify `internal/tmuxenv/tmuxenv.go`; Test `internal/tmuxenv/tmuxenv_test.go`.

**Interfaces:** Produces the same `Capture`/`CaptureEnv`/`SocketFromEnv` API; behavior extended to fall back to `MUSTER_SOCKET`/`MUSTER_PANE`/`MUSTER_SESSION_ID`/`MUSTER_SESSION_NAME`/`MUSTER_PROJECT` when the live tmux env is absent.

- [ ] **Step 1: Write the failing test** (append to `tmuxenv_test.go`)

```go
func TestCaptureEnvFallsBackToMusterVars(t *testing.T) {
	t.Setenv("TMUX", "")       // no live tmux
	t.Setenv("TMUX_PANE", "")
	t.Setenv("MUSTER_SOCKET", "/tmp/tmux-0/proj-bhw")
	t.Setenv("MUSTER_PANE", "%9")
	t.Setenv("MUSTER_SESSION_ID", "$3")
	t.Setenv("MUSTER_SESSION_NAME", "bhw-2")
	t.Setenv("MUSTER_PROJECT", "bhw")
	// tmux would be queried for label; stub it empty (no live session)
	prev := Run
	Run = func(args ...string) (string, error) { return "", nil }
	t.Cleanup(func() { Run = prev })

	c := CaptureEnv()
	if c.SocketPath != "/tmp/tmux-0/proj-bhw" || c.PaneID != "%9" ||
		c.SessionID != "$3" || c.SessionName != "bhw-2" || c.Project != "bhw" {
		t.Fatalf("fallback capture = %+v", c)
	}
}

func TestSocketFromEnvPrefersLiveTmux(t *testing.T) {
	t.Setenv("TMUX", "/live/proj-x,1,0")
	t.Setenv("MUSTER_SOCKET", "/fallback/proj-y")
	if got := SocketFromEnv(); got != "/live/proj-x" {
		t.Fatalf("want live socket, got %q", got)
	}
}
```

- [ ] **Step 2: Run to verify fail** — `TMPDIR=/tmp go test ./internal/tmuxenv/ -run 'FallsBack|PrefersLive'` → FAIL.

- [ ] **Step 3: Implement the fallback**

Replace `SocketFromEnv` and `CaptureEnv`:

```go
// SocketFromEnv returns the tmux socket path: the live $TMUX socket if present,
// else the wrapper-exported $MUSTER_SOCKET.
func SocketFromEnv() string {
	if tmux := os.Getenv("TMUX"); tmux != "" {
		return strings.SplitN(tmux, ",", 2)[0]
	}
	return os.Getenv("MUSTER_SOCKET")
}

// CaptureEnv reads identity from the live tmux env, falling back field-by-field
// to the wrapper-exported MUSTER_* vars (so capture works even where $TMUX is
// absent, e.g. an MCP subprocess).
func CaptureEnv() Capture {
	socket := SocketFromEnv()
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		pane = os.Getenv("MUSTER_PANE")
	}
	c := Capture{SocketPath: socket, PaneID: pane, Project: ProjectFromSocket(socket)}
	if c.Project == "" {
		c.Project = os.Getenv("MUSTER_PROJECT")
	}
	if socket != "" && pane != "" {
		c.SessionID = query(socket, pane, "#{session_id}")
		c.SessionName = query(socket, pane, "#{session_name}")
		c.Label, c.LabelManual = SessionLabel(socket, pane)
	}
	if c.SessionID == "" {
		c.SessionID = os.Getenv("MUSTER_SESSION_ID")
	}
	if c.SessionName == "" {
		c.SessionName = os.Getenv("MUSTER_SESSION_NAME")
	}
	return c
}
```

- [ ] **Step 4: Run to verify pass** — `TMPDIR=/tmp go test ./internal/tmuxenv/` → PASS (new + existing).
- [ ] **Step 5: Lint** — `TMPDIR=/tmp golangci-lint run ./internal/tmuxenv/` → 0 issues (run standalone; check exit code).
- [ ] **Step 6: Commit** — `feat(tmuxenv): fall back to MUSTER_* env when $TMUX absent`.

---

### Task 2: store — `last_read_at`, `UnreadCount`, `MarkRead`

**Files:** Modify `internal/store/schema.sql`, `internal/store/store.go` (migration), `internal/store/agents.go` or `threads.go`; Test `internal/store/threads_test.go` (or agents_test.go).

**Interfaces produced (consumed by Task 3):** `func (s *Store) UnreadCount(alias string) (int, error)`, `func (s *Store) MarkRead(alias string) error`.

- [ ] **Step 1: Write the failing test**

```go
func TestUnreadCountAndMarkRead(t *testing.T) {
	s := openTestStore(t) // existing helper
	must(t, s.RegisterAgent(Agent{Alias: "a", Role: "r"}))
	// two messages addressed to a
	_, err := s.CreateThread(Thread{Kind: "message", FromAgent: "x", ToKind: "agent", ToTarget: "a"}, "one")
	must(t, err)
	_, err = s.CreateThread(Thread{Kind: "message", FromAgent: "x", ToKind: "agent", ToTarget: "a"}, "two")
	must(t, err)

	n, err := s.UnreadCount("a")
	if err != nil || n != 2 {
		t.Fatalf("unread before read = %d (%v), want 2", n, err)
	}
	must(t, s.MarkRead("a"))
	n, err = s.UnreadCount("a")
	if err != nil || n != 0 {
		t.Fatalf("unread after MarkRead = %d (%v), want 0", n, err)
	}
	// a new message after read counts again
	_, err = s.CreateThread(Thread{Kind: "message", FromAgent: "x", ToKind: "agent", ToTarget: "a"}, "three")
	must(t, err)
	if n, _ := s.UnreadCount("a"); n != 1 {
		t.Fatalf("unread after new msg = %d, want 1", n)
	}
}
```
(Use the store package's existing temp-store + `must` helpers; if `must` doesn't exist, inline `if err != nil { t.Fatal(err) }`.)

- [ ] **Step 2: Run to verify fail** — FAIL (methods undefined).

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

- [ ] **Step 5: Run tests + lint** — `TMPDIR=/tmp go test ./internal/store/` PASS; `golangci-lint run ./internal/store/` 0 issues.
- [ ] **Step 6: Commit** — `feat(store): unread-count via last_read_at (UnreadCount/MarkRead)`.

---

### Task 3: wake + daemon — count-bearing `@muster_inbox` notify, clear on read

**Files:** Modify `internal/wake/wake.go`, `internal/daemon/daemon.go`, `cmd/muster/main.go`; update any `wake.Notifier` implementations/fakes; Tests in `internal/wake/` and `internal/daemon/`.

**Interfaces:** `Notifier.Notify` gains a `count int` param. All implementers + call sites update.

- [ ] **Step 1: Write/adjust failing tests**

Wake test (`internal/wake/wake_test.go`) — assert the option is set to the count and unset on 0:
```go
func TestNotifySetsCountAndUnsetsOnZero(t *testing.T) {
	var cmds [][]string
	n := TmuxNotifier{Option: "@muster_inbox", Run: func(_ context.Context, args ...string) error {
		cmds = append(cmds, args); return nil
	}}
	_ = n.Notify("/s", "$1", 3)
	// first cmd sets the option to "3"
	if got := cmds[0]; got[4] != "@muster_inbox" || got[5] != "3" {
		t.Fatalf("set cmd = %v", got)
	}
	cmds = nil
	_ = n.Notify("/s", "$1", 0)
	// count 0 → unset (-u), no value
	joined := strings.Join(cmds[0], " ")
	if !strings.Contains(joined, "-u") || !strings.Contains(joined, "@muster_inbox") {
		t.Fatalf("zero should unset, got %v", cmds[0])
	}
}
```
Daemon test — extend the wake-wiring test's fake Notifier to the new signature and assert `get_inbox` marks read (unread→0). Update the existing fake `Notifier` in `internal/daemon` tests to implement `Notify(sock, sess string, count int) error`.

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

In `notifyForThread`, the recipient loop becomes:
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
In the `get_inbox` case, mark read before clearing:
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

- [ ] **Step 5: main.go — option name**

Change `wake.NewTmuxNotifier("@claude_attn", 500*time.Millisecond)` → `wake.NewTmuxNotifier("@muster_inbox", 500*time.Millisecond)`.

- [ ] **Step 6: Update all other `Notifier` implementers/call sites** so the build passes (grep `Notify(` — any test fake, `mustertest`, etc.). Run `TMPDIR=/tmp go build ./...`.

- [ ] **Step 7: Run tests + lint** — `TMPDIR=/tmp go test ./...` PASS; `golangci-lint run ./...` 0 issues.
- [ ] **Step 8: Commit** — `feat(daemon,wake): count-bearing @muster_inbox notify; clear on inbox read`.

---

## Final verification

`TMPDIR=/tmp GODEBUG=netdns=go just verify` green. Then whole-branch review, then merge to `dev`. **After merge:** rebuild `~/.local/bin/muster`, apply the dotfiles wrapper + `📬` render (separate, done by the controller), restart the daemon, and live-re-test the two throwaway sessions.
