# muster Milestone A — Foundation + Store + Daemon — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the spine of muster — a SQLite-backed store and a lazy-started unix-socket daemon — so agent/thread/task/kv state can be written and read over a socket, demoable via a debug CLI, with no MCP or tmux yet.

**Architecture:** One Go module, one binary. `internal/store` wraps SQLite (pure-Go driver, WAL) behind typed methods. `internal/daemon` serves newline-delimited JSON requests over a unix socket, dispatching each op to a store method. `internal/client` connects to the socket, lazily spawning the daemon if it's not running. `cmd/muster` dispatches subcommands (`serve`, `debug`).

**Tech Stack:** Go 1.26 · `modernc.org/sqlite` (pure-Go, cgo-free, so the binary stays statically linkable and `go install`-able) · stdlib `database/sql`, `net`, `encoding/json` · golangci-lint + just + lefthook for the quality gate.

## Global Constraints

- **Module path:** `github.com/schuettc/muster` (verbatim).
- **Go version floor:** `go 1.26`.
- **No cgo.** SQLite driver is `modernc.org/sqlite`. Never introduce `mattn/go-sqlite3`. The binary must build with `CGO_ENABLED=0`.
- **State locations** (resolved once, in `internal/paths`): data dir `~/.local/share/muster/`, db `~/.local/share/muster/bus.db`, socket `~/.local/share/muster/sock`. Honor `MUSTER_HOME` override for tests.
- **SQLite pragmas:** open with `_pragma=journal_mode(WAL)` and `_pragma=busy_timeout(5000)`; keep transactions short.
- **Timestamps:** Unix milliseconds (`int64`), via a single `nowMillis()` helper so tests can inject a clock.
- **The bus never calls a model.** Nothing in this milestone makes network calls.
- **Thread `kind`:** exactly `"message"` or `"task"`. **Task `status`:** one of `open`, `claimed`, `needs_info`, `blocked`, `completed`, `declined`, `cancelled`. Messages have `status = NULL`.
- **Addressing (`to_kind`):** exactly `"agent"`, `"role"`, or `"broadcast"`.

---

## File Structure

```
muster/
├── go.mod
├── go.sum
├── justfile                         # single source of truth for fmt/lint/test/build/verify
├── lefthook.yml                     # pre-commit → just fmt; pre-push → just verify
├── .golangci.yml
├── .github/workflows/ci.yml         # runs `just verify` on PRs to dev/main
├── cmd/muster/
│   └── main.go                      # subcommand dispatch: serve | debug
└── internal/
    ├── paths/paths.go               # resolve data dir / db / socket (MUSTER_HOME aware)
    ├── clock/clock.go               # nowMillis() + test override
    ├── store/
    │   ├── schema.sql               # embedded DDL
    │   ├── store.go                 # Store type: Open/Close, DB handle
    │   ├── models.go                # Agent, Thread, Entry, KVPair structs
    │   ├── agents.go                # RegisterAgent (upsert), ListAgents, TouchAgent
    │   ├── threads.go               # CreateThread, AppendEntry, GetThread, Inbox
    │   ├── tasks.go                 # TransitionTask, ClaimTask (atomic)
    │   └── kv.go                    # KVSet, KVGet
    ├── proto/proto.go               # Request/Response envelopes (shared by daemon + client)
    ├── daemon/daemon.go             # unix socket server + op dispatch
    └── client/client.go            # connect-or-spawn client
```

Files split by responsibility (one store concern per file) so each stays small and independently testable. `proto` is shared by `daemon` and `client` so the wire types can't drift.

---

### Task 1: Project scaffold + quality gate + CI

**Files:**
- Create: `go.mod`, `justfile`, `lefthook.yml`, `.golangci.yml`, `.github/workflows/ci.yml`, `cmd/muster/main.go`
- Test: none yet (this task's deliverable is "the skeleton builds and `just verify` is green")

**Interfaces:**
- Consumes: nothing (first task).
- Produces: a compiling `main` package with a subcommand switch that later tasks extend; `just verify` as the gate every later task runs.

- [ ] **Step 1: Initialize the module**

Run (from repo root `~/GitHub/schuettc/muster`, which already has `README.md` + `docs/`):
```bash
go mod init github.com/schuettc/muster
go get modernc.org/sqlite@latest
```
Expected: `go.mod` created with `go 1.26` and a `require modernc.org/sqlite ...` line; `go.sum` populated.

- [ ] **Step 2: Write the minimal entrypoint**

Create `cmd/muster/main.go`:
```go
// Command muster is the entrypoint for the muster coordination bus.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: muster <serve|debug> [args]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		fmt.Fprintln(os.Stderr, "serve: not implemented yet")
		os.Exit(1)
	case "debug":
		fmt.Fprintln(os.Stderr, "debug: not implemented yet")
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "muster: unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}
```

- [ ] **Step 3: Write the quality gate (justfile + lefthook + golangci + CI)**

Create `justfile`:
```make
# muster developer tasks — the SAME targets CI runs, so local and CI can't drift.
set shell := ["bash", "-uc"]

# Format code.
fmt:
    gofmt -w .

# Verify formatting is clean (used by verify/CI).
fmt-check:
    test -z "$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

# Static analysis.
lint:
    golangci-lint run ./...

# Tests (race detector on).
test:
    go test -race ./...

# Build the binary.
build:
    CGO_ENABLED=0 go build -o bin/muster ./cmd/muster

# Full gate — what pre-push and CI run.
verify: fmt-check lint test build
```

Create `lefthook.yml`:
```yaml
pre-commit:
  commands:
    fmt:
      run: just fmt && git add -u
pre-push:
  commands:
    verify:
      run: just verify
```

Create `.golangci.yml`:
```yaml
version: "2"
linters:
  enable:
    - errcheck
    - govet
    - ineffassign
    - staticcheck
    - unused
    - gocritic
    - revive
```

Create `.github/workflows/ci.yml`:
```yaml
name: CI
on:
  pull_request:
    branches: [dev, main]
  push:
    branches: [dev, main]
jobs:
  verify:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26"
      - uses: extractions/setup-just@v2
      - name: Install golangci-lint
        run: curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b "$(go env GOPATH)/bin" latest
      - name: Verify
        run: just verify
```

- [ ] **Step 4: Install hooks and run the gate**

Run:
```bash
brew install just lefthook golangci-lint
lefthook install
just verify
```
Expected: `fmt-check`, `lint`, `test` (no tests yet → "no test files" is OK, exit 0), and `build` all pass; `bin/muster` produced.

- [ ] **Step 5: Set up the branch model and commit**

Run:
```bash
git add -A
git commit -m "chore: Go scaffold, quality gate (just+lefthook+golangci), CI

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
git push origin main
# Create the long-lived integration branch for the feat → dev → main model.
git checkout -b dev && git push -u origin dev && git checkout main
# Protect dev and main so the CI 'verify' check is required.
gh api -X PUT repos/schuettc/muster/branches/main/protection \
  -f 'required_status_checks[strict]=true' -f 'required_status_checks[contexts][]=verify' \
  -F 'enforce_admins=true' -F 'required_pull_request_reviews=null' -F 'restrictions=null'
gh api -X PUT repos/schuettc/muster/branches/dev/protection \
  -f 'required_status_checks[strict]=true' -f 'required_status_checks[contexts][]=verify' \
  -F 'enforce_admins=false' -F 'required_pull_request_reviews=null' -F 'restrictions=null'
```
Expected: `main` and `dev` exist and are protected; CI runs on the next PR. (Feature work branches off `dev` as `feat/*`.)

---

### Task 2: Paths, clock, and SQLite open with schema

**Files:**
- Create: `internal/paths/paths.go`, `internal/clock/clock.go`, `internal/store/schema.sql`, `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces:
  - `paths.Home() string`, `paths.DBPath() string`, `paths.SocketPath() string` (honor `MUSTER_HOME`).
  - `clock.NowMillis() int64`; `clock.SetForTesting(fn func() int64)` and `clock.ResetForTesting()`.
  - `store.Open(dbPath string) (*Store, error)`; `(*Store).Close() error`; `(*Store).DB() *sql.DB`. `Open` applies the embedded schema idempotently.

- [ ] **Step 1: Write `paths` and `clock`**

Create `internal/paths/paths.go`:
```go
// Package paths resolves muster's on-disk locations.
package paths

import (
	"os"
	"path/filepath"
)

// Home is the muster data directory (~/.local/share/muster, or $MUSTER_HOME).
func Home() string {
	if h := os.Getenv("MUSTER_HOME"); h != "" {
		return h
	}
	base, err := os.UserHomeDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, ".local", "share", "muster")
}

// DBPath is the SQLite database path.
func DBPath() string { return filepath.Join(Home(), "bus.db") }

// SocketPath is the daemon's unix socket path.
func SocketPath() string { return filepath.Join(Home(), "sock") }
```

Create `internal/clock/clock.go`:
```go
// Package clock provides a test-overridable millisecond clock.
package clock

import (
	"sync"
	"time"
)

var (
	mu  sync.RWMutex
	now = func() int64 { return time.Now().UnixMilli() }
)

// NowMillis returns the current time in Unix milliseconds.
func NowMillis() int64 {
	mu.RLock()
	defer mu.RUnlock()
	return now()
}

// SetForTesting overrides the clock. Call ResetForTesting to restore.
func SetForTesting(fn func() int64) {
	mu.Lock()
	defer mu.Unlock()
	now = fn
}

// ResetForTesting restores the real clock.
func ResetForTesting() {
	mu.Lock()
	defer mu.Unlock()
	now = func() int64 { return time.Now().UnixMilli() }
}
```

- [ ] **Step 2: Write the schema**

Create `internal/store/schema.sql`:
```sql
CREATE TABLE IF NOT EXISTS agents (
    alias         TEXT PRIMARY KEY,
    role          TEXT NOT NULL DEFAULT '',
    model_type    TEXT NOT NULL DEFAULT '',
    socket_path   TEXT NOT NULL DEFAULT '',
    pane_id       TEXT NOT NULL DEFAULT '',
    session_name  TEXT NOT NULL DEFAULT '',
    registered_at INTEGER NOT NULL,
    last_seen     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS threads (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL,                 -- 'message' | 'task'
    from_agent TEXT NOT NULL,
    to_kind    TEXT NOT NULL,                 -- 'agent' | 'role' | 'broadcast'
    to_target  TEXT NOT NULL DEFAULT '',
    subject    TEXT NOT NULL DEFAULT '',
    ref        TEXT NOT NULL DEFAULT '',
    status     TEXT,                          -- NULL for messages
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_threads_recipient ON threads(to_kind, to_target);

CREATE TABLE IF NOT EXISTS entries (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_id     INTEGER NOT NULL REFERENCES threads(id),
    from_agent    TEXT NOT NULL,
    body          TEXT NOT NULL DEFAULT '',
    status_change TEXT,
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_entries_thread ON entries(thread_id);

CREATE TABLE IF NOT EXISTS kv (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);
```

- [ ] **Step 3: Write the failing test for `store.Open`**

Create `internal/store/store_test.go`:
```go
package store

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db := filepath.Join(t.TempDir(), "bus.db")
	s, err := Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenCreatesSchema(t *testing.T) {
	s := newTestStore(t)
	var n int
	err := s.DB().QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('agents','threads','entries','kv')`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 4 {
		t.Fatalf("expected 4 tables, got %d", n)
	}
}
```

- [ ] **Step 4: Run the test to verify it fails**

Run: `go test ./internal/store/ -run TestOpenCreatesSchema -v`
Expected: FAIL — `undefined: Open` (package doesn't compile).

- [ ] **Step 5: Write `store.Open`**

Create `internal/store/store.go`:
```go
// Package store is muster's SQLite persistence layer.
package store

import (
	_ "embed"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Store wraps the SQLite database.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the database at dbPath, enables WAL, and
// applies the schema idempotently.
func Open(dbPath string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1) // serialize writers; WAL still allows concurrent readers via separate conns later
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// DB exposes the underlying handle (tests + store methods).
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./internal/store/ -run TestOpenCreatesSchema -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/paths internal/clock internal/store go.mod go.sum
git commit -m "feat(store): paths, clock, SQLite open with embedded schema

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 3: Agent registry (upsert + list + touch)

**Files:**
- Create: `internal/store/models.go`, `internal/store/agents.go`
- Test: `internal/store/agents_test.go`

**Interfaces:**
- Consumes: `Store`, `clock.NowMillis`.
- Produces:
  - `type Agent struct { Alias, Role, ModelType, SocketPath, PaneID, SessionName string; RegisteredAt, LastSeen int64 }`
  - `(*Store) RegisterAgent(a Agent) error` — upsert by `Alias`; sets `RegisteredAt` on first insert, always bumps `LastSeen`.
  - `(*Store) ListAgents() ([]Agent, error)` — ordered by `Alias`.
  - `(*Store) TouchAgent(alias string) error` — bumps `LastSeen`; no error if absent.

- [ ] **Step 1: Write the model structs**

Create `internal/store/models.go`:
```go
package store

// Agent is a registered participant on the bus.
type Agent struct {
	Alias        string
	Role         string
	ModelType    string
	SocketPath   string
	PaneID       string
	SessionName  string
	RegisteredAt int64
	LastSeen     int64
}

// Thread is a conversation: a message (no status) or a task (status set).
type Thread struct {
	ID        int64
	Kind      string
	FromAgent string
	ToKind    string
	ToTarget  string
	Subject   string
	Ref       string
	Status    string // "" means NULL (message)
	CreatedAt int64
	UpdatedAt int64
}

// Entry is one append-only message within a thread.
type Entry struct {
	ID           int64
	ThreadID     int64
	FromAgent    string
	Body         string
	StatusChange string // "" means none
	CreatedAt    int64
}

// KVPair is a shared blackboard fact.
type KVPair struct {
	Key       string
	Value     string
	UpdatedBy string
	UpdatedAt int64
}
```

- [ ] **Step 2: Write the failing test**

Create `internal/store/agents_test.go`:
```go
package store

import "testing"

func TestRegisterAgentUpsertAndList(t *testing.T) {
	s := newTestStore(t)

	if err := s.RegisterAgent(Agent{Alias: "backend", Role: "producer", ModelType: "claude", SocketPath: "/s", PaneID: "%1", SessionName: "bhw"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Re-register (restart) with a new pane — upsert, not duplicate.
	if err := s.RegisterAgent(Agent{Alias: "backend", Role: "producer", ModelType: "claude", SocketPath: "/s2", PaneID: "%9", SessionName: "bhw"}); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	agents, err := s.ListAgents()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent after upsert, got %d", len(agents))
	}
	if agents[0].PaneID != "%9" || agents[0].SocketPath != "/s2" {
		t.Fatalf("upsert did not refresh tuple: %+v", agents[0])
	}
	if agents[0].RegisteredAt == 0 || agents[0].LastSeen == 0 {
		t.Fatalf("timestamps not set: %+v", agents[0])
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/store/ -run TestRegisterAgentUpsertAndList -v`
Expected: FAIL — `undefined: (*Store).RegisterAgent`.

- [ ] **Step 4: Implement agent methods**

Create `internal/store/agents.go`:
```go
package store

import "github.com/schuettc/muster/internal/clock"

// RegisterAgent upserts by Alias: inserts on first sight (stamping RegisteredAt),
// and on conflict refreshes the tuple + LastSeen while preserving RegisteredAt.
func (s *Store) RegisterAgent(a Agent) error {
	now := clock.NowMillis()
	_, err := s.db.Exec(`
INSERT INTO agents (alias, role, model_type, socket_path, pane_id, session_name, registered_at, last_seen)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(alias) DO UPDATE SET
    role=excluded.role,
    model_type=excluded.model_type,
    socket_path=excluded.socket_path,
    pane_id=excluded.pane_id,
    session_name=excluded.session_name,
    last_seen=excluded.last_seen`,
		a.Alias, a.Role, a.ModelType, a.SocketPath, a.PaneID, a.SessionName, now, now)
	return err
}

// ListAgents returns all agents ordered by alias.
func (s *Store) ListAgents() ([]Agent, error) {
	rows, err := s.db.Query(`
SELECT alias, role, model_type, socket_path, pane_id, session_name, registered_at, last_seen
FROM agents ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.Alias, &a.Role, &a.ModelType, &a.SocketPath, &a.PaneID, &a.SessionName, &a.RegisteredAt, &a.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TouchAgent bumps last_seen. No error if the agent is unknown.
func (s *Store) TouchAgent(alias string) error {
	_, err := s.db.Exec(`UPDATE agents SET last_seen=? WHERE alias=?`, clock.NowMillis(), alias)
	return err
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/store/ -run TestRegisterAgentUpsertAndList -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store
git commit -m "feat(store): agent registry with upsert, list, touch

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 4: Threads, entries, and inbox

**Files:**
- Create: `internal/store/threads.go`
- Test: `internal/store/threads_test.go`

**Interfaces:**
- Consumes: `Store`, `Agent`, `Thread`, `Entry`, `clock.NowMillis`.
- Produces:
  - `(*Store) CreateThread(t Thread, firstBody string) (int64, error)` — inserts the thread + its first entry (from `t.FromAgent`, body `firstBody`) in one transaction; sets `CreatedAt`/`UpdatedAt`; returns the new thread id. For `kind="task"`, caller sets `Status="open"`; for `kind="message"`, `Status=""`.
  - `(*Store) AppendEntry(threadID int64, fromAgent, body, statusChange string) (int64, error)` — inserts an entry, bumps the thread's `UpdatedAt`; returns entry id.
  - `(*Store) GetThread(id int64) (Thread, []Entry, error)` — thread + its entries ordered by id.
  - `(*Store) Inbox(alias string) ([]Thread, error)` — threads addressed to `alias` directly, to its role, or broadcast; ordered by `UpdatedAt` desc.

- [ ] **Step 1: Write the failing test**

Create `internal/store/threads_test.go`:
```go
package store

import "testing"

func TestCreateThreadAppendAndGet(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(Thread{
		Kind: "task", FromAgent: "backend", ToKind: "role", ToTarget: "reviewer",
		Subject: "Review feat/wagers", Ref: "repo=bhw branch=feat/wagers", Status: "open",
	}, "please review the rename")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.AppendEntry(id, "reviewer", "looks good, one nit", "claimed"); err != nil {
		t.Fatalf("append: %v", err)
	}
	th, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if th.Subject != "Review feat/wagers" || len(entries) != 2 {
		t.Fatalf("unexpected thread/entries: %+v / %d", th, len(entries))
	}
	if th.UpdatedAt < th.CreatedAt {
		t.Fatalf("updated_at should advance on append")
	}
}

func TestInboxMatchesAgentRoleAndBroadcast(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "rev1", Role: "reviewer", ModelType: "codex"}); err != nil {
		t.Fatal(err)
	}
	mk := func(toKind, toTarget string) {
		if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "backend", ToKind: toKind, ToTarget: toTarget}, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	mk("agent", "rev1")      // direct
	mk("role", "reviewer")   // by role
	mk("broadcast", "")      // to everyone
	mk("agent", "someoneelse") // not for rev1

	in, err := s.Inbox("rev1")
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(in) != 3 {
		t.Fatalf("expected 3 inbox threads for rev1, got %d", len(in))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run 'TestCreateThread|TestInbox' -v`
Expected: FAIL — `undefined: (*Store).CreateThread`.

- [ ] **Step 3: Implement threads/entries/inbox**

Create `internal/store/threads.go`:
```go
package store

import (
	"database/sql"

	"github.com/schuettc/muster/internal/clock"
)

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CreateThread inserts a thread and its first entry atomically.
func (s *Store) CreateThread(t Thread, firstBody string) (int64, error) {
	now := clock.NowMillis()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`
INSERT INTO threads (kind, from_agent, to_kind, to_target, subject, ref, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Kind, t.FromAgent, t.ToKind, t.ToTarget, t.Subject, t.Ref, nullable(t.Status), now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`
INSERT INTO entries (thread_id, from_agent, body, status_change, created_at)
VALUES (?, ?, ?, ?, ?)`, id, t.FromAgent, firstBody, nil, now); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// AppendEntry adds an entry and advances the thread's updated_at.
func (s *Store) AppendEntry(threadID int64, fromAgent, body, statusChange string) (int64, error) {
	now := clock.NowMillis()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`
INSERT INTO entries (thread_id, from_agent, body, status_change, created_at)
VALUES (?, ?, ?, ?, ?)`, threadID, fromAgent, body, nullable(statusChange), now)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE threads SET updated_at=? WHERE id=?`, now, threadID); err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func scanThread(row interface{ Scan(...any) error }) (Thread, error) {
	var t Thread
	var status sql.NullString
	err := row.Scan(&t.ID, &t.Kind, &t.FromAgent, &t.ToKind, &t.ToTarget, &t.Subject, &t.Ref, &status, &t.CreatedAt, &t.UpdatedAt)
	if status.Valid {
		t.Status = status.String
	}
	return t, err
}

const threadCols = `id, kind, from_agent, to_kind, to_target, subject, ref, status, created_at, updated_at`

// GetThread returns the thread and its entries (ordered by id).
func (s *Store) GetThread(id int64) (Thread, []Entry, error) {
	t, err := scanThread(s.db.QueryRow(`SELECT `+threadCols+` FROM threads WHERE id=?`, id))
	if err != nil {
		return Thread{}, nil, err
	}
	rows, err := s.db.Query(`SELECT id, thread_id, from_agent, body, status_change, created_at FROM entries WHERE thread_id=? ORDER BY id`, id)
	if err != nil {
		return Thread{}, nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		var e Entry
		var sc sql.NullString
		if err := rows.Scan(&e.ID, &e.ThreadID, &e.FromAgent, &e.Body, &sc, &e.CreatedAt); err != nil {
			return Thread{}, nil, err
		}
		if sc.Valid {
			e.StatusChange = sc.String
		}
		entries = append(entries, e)
	}
	return t, entries, rows.Err()
}

// Inbox returns threads addressed to alias directly, to alias's role, or broadcast.
func (s *Store) Inbox(alias string) ([]Thread, error) {
	rows, err := s.db.Query(`
SELECT `+threadCols+` FROM threads
WHERE (to_kind='agent'     AND to_target=?)
   OR (to_kind='role'      AND to_target=(SELECT role FROM agents WHERE alias=?))
   OR (to_kind='broadcast')
ORDER BY updated_at DESC`, alias, alias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestCreateThread|TestInbox' -v`
Expected: PASS (both).

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat(store): threads, entries, inbox (agent/role/broadcast routing)

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 5: Task transitions with atomic claim

**Files:**
- Create: `internal/store/tasks.go`
- Test: `internal/store/tasks_test.go`

**Interfaces:**
- Consumes: `Store`, `AppendEntry`, `clock.NowMillis`.
- Produces:
  - `var ErrNotClaimable = errors.New("task not claimable")`
  - `(*Store) ClaimTask(threadID int64, byAgent string) error` — atomically moves `open → claimed`; returns `ErrNotClaimable` if it wasn't `open`. Appends a `claimed` entry.
  - `(*Store) TransitionTask(threadID int64, byAgent, newStatus, note string) error` — validates `newStatus` is a known task state, updates `threads.status`, and appends an entry recording the change.
  - `var TaskStates = map[string]bool{...}` for validation.

- [ ] **Step 1: Write the failing test**

Create `internal/store/tasks_test.go`:
```go
package store

import (
	"errors"
	"testing"
)

func newTask(t *testing.T, s *Store) int64 {
	t.Helper()
	id, err := s.CreateThread(Thread{Kind: "task", FromAgent: "backend", ToKind: "role", ToTarget: "reviewer", Status: "open"}, "review please")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestClaimTaskIsAtomic(t *testing.T) {
	s := newTestStore(t)
	id := newTask(t, s)

	if err := s.ClaimTask(id, "rev1"); err != nil {
		t.Fatalf("first claim should succeed: %v", err)
	}
	if err := s.ClaimTask(id, "rev2"); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("second claim should be ErrNotClaimable, got %v", err)
	}
	th, _, _ := s.GetThread(id)
	if th.Status != "claimed" {
		t.Fatalf("status should be claimed, got %q", th.Status)
	}
}

func TestTransitionTaskValidatesAndRecords(t *testing.T) {
	s := newTestStore(t)
	id := newTask(t, s)
	_ = s.ClaimTask(id, "rev1")

	if err := s.TransitionTask(id, "rev1", "bogus", ""); err == nil {
		t.Fatalf("expected error for invalid status")
	}
	if err := s.TransitionTask(id, "rev1", "completed", "LGTM"); err != nil {
		t.Fatalf("valid transition: %v", err)
	}
	th, entries, _ := s.GetThread(id)
	if th.Status != "completed" {
		t.Fatalf("status should be completed, got %q", th.Status)
	}
	last := entries[len(entries)-1]
	if last.StatusChange != "completed" || last.Body != "LGTM" {
		t.Fatalf("transition not recorded as entry: %+v", last)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run 'TestClaimTask|TestTransitionTask' -v`
Expected: FAIL — `undefined: ErrNotClaimable`.

- [ ] **Step 3: Implement task methods**

Create `internal/store/tasks.go`:
```go
package store

import (
	"errors"
	"fmt"

	"github.com/schuettc/muster/internal/clock"
)

// ErrNotClaimable is returned when claiming a task that is not open.
var ErrNotClaimable = errors.New("task not claimable")

// TaskStates is the set of valid task statuses.
var TaskStates = map[string]bool{
	"open": true, "claimed": true, "needs_info": true, "blocked": true,
	"completed": true, "declined": true, "cancelled": true,
}

// ClaimTask atomically moves a task from open → claimed and records it.
func (s *Store) ClaimTask(threadID int64, byAgent string) error {
	now := clock.NowMillis()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`UPDATE threads SET status='claimed', updated_at=? WHERE id=? AND status='open'`, now, threadID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotClaimable
	}
	if _, err := tx.Exec(`INSERT INTO entries (thread_id, from_agent, body, status_change, created_at) VALUES (?, ?, '', 'claimed', ?)`, threadID, byAgent, now); err != nil {
		return err
	}
	return tx.Commit()
}

// TransitionTask sets a new (validated) status and records the change as an entry.
func (s *Store) TransitionTask(threadID int64, byAgent, newStatus, note string) error {
	if !TaskStates[newStatus] {
		return fmt.Errorf("invalid task status %q", newStatus)
	}
	now := clock.NowMillis()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`UPDATE threads SET status=?, updated_at=? WHERE id=?`, newStatus, now, threadID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO entries (thread_id, from_agent, body, status_change, created_at) VALUES (?, ?, ?, ?, ?)`, threadID, byAgent, note, newStatus, now); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestClaimTask|TestTransitionTask' -v`
Expected: PASS (both).

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat(store): task state machine with atomic claim

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 6: KV blackboard

**Files:**
- Create: `internal/store/kv.go`
- Test: `internal/store/kv_test.go`

**Interfaces:**
- Consumes: `Store`, `KVPair`, `clock.NowMillis`.
- Produces:
  - `(*Store) KVSet(key, value, updatedBy string) error` — upsert.
  - `(*Store) KVGet(key string) (KVPair, bool, error)` — second return is `false` if absent.

- [ ] **Step 1: Write the failing test**

Create `internal/store/kv_test.go`:
```go
package store

import "testing"

func TestKVSetGet(t *testing.T) {
	s := newTestStore(t)
	if _, ok, err := s.KVGet("api.base"); err != nil || ok {
		t.Fatalf("missing key should return ok=false, got ok=%v err=%v", ok, err)
	}
	if err := s.KVSet("api.base", "http://localhost:4000", "backend"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.KVSet("api.base", "http://localhost:4001", "backend"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	p, ok, err := s.KVGet("api.base")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if p.Value != "http://localhost:4001" || p.UpdatedBy != "backend" {
		t.Fatalf("unexpected value: %+v", p)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run TestKVSetGet -v`
Expected: FAIL — `undefined: (*Store).KVGet`.

- [ ] **Step 3: Implement KV**

Create `internal/store/kv.go`:
```go
package store

import (
	"database/sql"
	"errors"

	"github.com/schuettc/muster/internal/clock"
)

// KVSet upserts a shared fact.
func (s *Store) KVSet(key, value, updatedBy string) error {
	_, err := s.db.Exec(`
INSERT INTO kv (key, value, updated_by, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		key, value, updatedBy, clock.NowMillis())
	return err
}

// KVGet returns the pair for key; ok is false if the key is absent.
func (s *Store) KVGet(key string) (KVPair, bool, error) {
	var p KVPair
	err := s.db.QueryRow(`SELECT key, value, updated_by, updated_at FROM kv WHERE key=?`, key).
		Scan(&p.Key, &p.Value, &p.UpdatedBy, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return KVPair{}, false, nil
	}
	if err != nil {
		return KVPair{}, false, err
	}
	return p, true, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/store/ -run TestKVSetGet -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat(store): KV blackboard (upsert + get)

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 7: Daemon, protocol, lazy-start client, and debug CLI

This task ties the store to a unix socket and makes the whole milestone demoable. It ends with a working `muster debug` that round-trips through a real daemon.

**Files:**
- Create: `internal/proto/proto.go`, `internal/daemon/daemon.go`, `internal/client/client.go`
- Modify: `cmd/muster/main.go`
- Test: `internal/daemon/daemon_test.go`

**Interfaces:**
- Consumes: everything in `store`.
- Produces:
  - `proto.Request{ Op string; Args map[string]any }`, `proto.Response{ OK bool; Data any; Error string }`.
  - `daemon.Serve(socketPath string, s *store.Store) (*daemon.Daemon, error)` — listens, serves in a goroutine; `(*Daemon) Close() error`.
  - `client.Call(socketPath string, req proto.Request) (proto.Response, error)` — connects, spawning `muster serve` if the socket is dead.

- [ ] **Step 1: Write protocol envelopes**

Create `internal/proto/proto.go`:
```go
// Package proto defines the daemon wire protocol: newline-delimited JSON.
package proto

// Request is one operation call. Args are op-specific.
type Request struct {
	Op   string         `json:"op"`
	Args map[string]any `json:"args,omitempty"`
}

// Response is the daemon's reply. Exactly one of Data/Error is meaningful.
type Response struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}
```

- [ ] **Step 2: Write the failing daemon test**

Create `internal/daemon/daemon_test.go`:
```go
package daemon

import (
	"path/filepath"
	"testing"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

func TestDaemonRegisterAndList(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	sock := filepath.Join(dir, "sock")
	d, err := Serve(sock, s)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	// register_agent
	reg, err := client.Call(sock, proto.Request{Op: "register_agent", Args: map[string]any{
		"alias": "backend", "role": "producer", "model_type": "claude",
	}})
	if err != nil || !reg.OK {
		t.Fatalf("register: err=%v resp=%+v", err, reg)
	}

	// list_agents
	list, err := client.Call(sock, proto.Request{Op: "list_agents"})
	if err != nil || !list.OK {
		t.Fatalf("list: err=%v resp=%+v", err, list)
	}
	agents, ok := list.Data.([]any)
	if !ok || len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %T %v", list.Data, list.Data)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/daemon/ -run TestDaemonRegisterAndList -v`
Expected: FAIL — packages `daemon`/`client` don't exist.

- [ ] **Step 4: Implement the daemon**

Create `internal/daemon/daemon.go`:
```go
// Package daemon serves the muster store over a unix socket.
package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"os"

	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

// Daemon owns the listener and the store.
type Daemon struct {
	ln net.Listener
	s  *store.Store
}

// Serve binds socketPath (replacing any stale socket) and serves in a goroutine.
func Serve(socketPath string, s *store.Store) (*Daemon, error) {
	_ = os.Remove(socketPath) // clear a stale socket from a previous run
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	d := &Daemon{ln: ln, s: s}
	go d.acceptLoop()
	return d, nil
}

// Close stops accepting connections.
func (d *Daemon) Close() error { return d.ln.Close() }

func (d *Daemon) acceptLoop() {
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go d.handle(conn)
	}
}

func (d *Daemon) handle(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	enc := json.NewEncoder(conn)
	for sc.Scan() {
		var req proto.Request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			_ = enc.Encode(proto.Response{Error: "bad request: " + err.Error()})
			continue
		}
		_ = enc.Encode(d.dispatch(req))
	}
}
```

- [ ] **Step 5: Implement dispatch (ops → store methods)**

Append to `internal/daemon/daemon.go`:
```go
func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func i64(m map[string]any, k string) int64 {
	if v, ok := m[k].(float64); ok { // JSON numbers decode to float64
		return int64(v)
	}
	return 0
}

func ok(data any) proto.Response  { return proto.Response{OK: true, Data: data} }
func fail(err error) proto.Response { return proto.Response{Error: err.Error()} }

func (d *Daemon) dispatch(req proto.Request) proto.Response {
	a := req.Args
	switch req.Op {
	case "register_agent":
		err := d.s.RegisterAgent(store.Agent{
			Alias: str(a, "alias"), Role: str(a, "role"), ModelType: str(a, "model_type"),
			SocketPath: str(a, "socket_path"), PaneID: str(a, "pane_id"), SessionName: str(a, "session_name"),
		})
		if err != nil {
			return fail(err)
		}
		return ok(nil)
	case "list_agents":
		agents, err := d.s.ListAgents()
		if err != nil {
			return fail(err)
		}
		return ok(agents)
	case "send_message":
		id, err := d.s.CreateThread(store.Thread{
			Kind: "message", FromAgent: str(a, "from"), ToKind: str(a, "to_kind"),
			ToTarget: str(a, "to_target"), Subject: str(a, "subject"), Ref: str(a, "ref"),
		}, str(a, "body"))
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"thread_id": id})
	case "task_create":
		id, err := d.s.CreateThread(store.Thread{
			Kind: "task", FromAgent: str(a, "from"), ToKind: str(a, "to_kind"),
			ToTarget: str(a, "to_target"), Subject: str(a, "subject"), Ref: str(a, "ref"), Status: "open",
		}, str(a, "body"))
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"thread_id": id})
	case "task_claim":
		if err := d.s.ClaimTask(i64(a, "thread_id"), str(a, "by")); err != nil {
			return fail(err)
		}
		return ok(nil)
	case "task_transition":
		if err := d.s.TransitionTask(i64(a, "thread_id"), str(a, "by"), str(a, "status"), str(a, "note")); err != nil {
			return fail(err)
		}
		return ok(nil)
	case "reply":
		id, err := d.s.AppendEntry(i64(a, "thread_id"), str(a, "from"), str(a, "body"), "")
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"entry_id": id})
	case "get_inbox":
		threads, err := d.s.Inbox(str(a, "alias"))
		if err != nil {
			return fail(err)
		}
		return ok(threads)
	case "kv_set":
		if err := d.s.KVSet(str(a, "key"), str(a, "value"), str(a, "by")); err != nil {
			return fail(err)
		}
		return ok(nil)
	case "kv_get":
		p, found, err := d.s.KVGet(str(a, "key"))
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"found": found, "pair": p})
	default:
		return proto.Response{Error: "unknown op: " + req.Op}
	}
}
```

- [ ] **Step 6: Implement the lazy-start client**

Create `internal/client/client.go`:
```go
// Package client talks to the muster daemon, spawning it if needed.
package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/schuettc/muster/internal/proto"
)

// Call sends one request, dialing socketPath and spawning `muster serve` if the
// socket is dead. Returns the daemon's response.
func Call(socketPath string, req proto.Request) (proto.Response, error) {
	conn, err := dialOrSpawn(socketPath)
	if err != nil {
		return proto.Response{}, err
	}
	defer conn.Close()

	line, err := json.Marshal(req)
	if err != nil {
		return proto.Response{}, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return proto.Response{}, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if !sc.Scan() {
		return proto.Response{}, fmt.Errorf("no response from daemon")
	}
	var resp proto.Response
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return proto.Response{}, err
	}
	return resp, nil
}

func dialOrSpawn(socketPath string) (net.Conn, error) {
	if c, err := net.Dial("unix", socketPath); err == nil {
		return c, nil
	}
	// Socket dead: spawn the daemon and wait for it to bind.
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "serve")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	_ = cmd.Process.Release()
	for i := 0; i < 50; i++ { // up to ~5s
		if c, err := net.Dial("unix", socketPath); err == nil {
			return c, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon did not start within timeout")
}
```

- [ ] **Step 7: Run the daemon test to verify it passes**

Run: `go test ./internal/daemon/ -run TestDaemonRegisterAndList -v`
Expected: PASS. (This test constructs the daemon in-process, so the client dials an existing socket — spawn path not exercised here.)

- [ ] **Step 8: Wire `serve` and `debug` into main**

Replace `cmd/muster/main.go`:
```go
// Command muster is the entrypoint for the muster coordination bus.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: muster <serve|debug> [args]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		runServe()
	case "debug":
		runDebug(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "muster: unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}

func runServe() {
	if err := os.MkdirAll(paths.Home(), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		os.Exit(1)
	}
	s, err := store.Open(paths.DBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	defer s.Close()
	d, err := daemon.Serve(paths.SocketPath(), s)
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
	defer d.Close()
	fmt.Fprintln(os.Stderr, "muster daemon listening at", paths.SocketPath())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}

// runDebug sends a raw op with key=value string args. Example:
//   muster debug register_agent alias=backend role=producer
//   muster debug list_agents
func runDebug(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: muster debug <op> [key=value ...]")
		os.Exit(2)
	}
	req := proto.Request{Op: args[0], Args: map[string]any{}}
	for _, kv := range args[1:] {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				req.Args[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	resp, err := client.Call(paths.SocketPath(), req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "call:", err)
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(resp, "", "  ")
	fmt.Println(string(out))
	if !resp.OK {
		os.Exit(1)
	}
}
```

- [ ] **Step 9: Manual end-to-end smoke test (lazy-start path)**

Run:
```bash
just build
export MUSTER_HOME="$(mktemp -d)"
./bin/muster debug register_agent alias=backend role=producer model_type=claude
./bin/muster debug list_agents
./bin/muster debug task_create from=backend to_kind=role to_target=reviewer subject="Review feat/wagers" body="please review"
./bin/muster debug get_inbox alias=backend   # (backend won't see reviewer-role tasks; expect ok:true, data:[] or null)
```
Expected: the first `debug` call spawns the daemon (you'll see "muster daemon listening…" on stderr), `register_agent` returns `"ok": true`, `list_agents` shows one agent, `task_create` returns a `thread_id`. Clean up: `pkill -f 'muster serve'; rm -rf "$MUSTER_HOME"`.

- [ ] **Step 10: Full gate + commit**

Run: `just verify`
Expected: PASS (fmt-check, lint, test, build).
```bash
git add -A
git commit -m "feat(daemon): unix-socket daemon, lazy-start client, debug CLI

Milestone A complete: store + daemon demoable end to end via 'muster debug'.

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

## Milestone A Definition of Done

- `just verify` green (fmt, lint, race tests, build) locally and in CI.
- `muster serve` lazily starts, binds the socket, persists to `~/.local/share/muster/bus.db`.
- `muster debug <op>` round-trips every store op (register/list agents, send_message, task_create/claim/transition, reply, get_inbox, kv_set/get).
- Task claim is atomic (second claimant gets `ErrNotClaimable`).
- No MCP, no tmux yet — those are Milestones B and C.

## Self-Review notes (author)

- **Spec coverage:** registry + passive `last_seen` (Task 3, `TouchAgent` ready for the wake layer to call), unified thread model with message/task kinds (Task 4), richer task states + atomic claim (Task 5), KV extra (Task 6), lazy daemon over unix socket (Task 7). Deferred per spec: MCP mode (B), tmux wake (C), human CLI polish + dotfiles wiring (D). Unread-tracking intentionally deferred (inbox returns all recipient threads; presentation/read-cursor lands with the MCP layer).
- **No placeholders:** every step has runnable code/commands.
- **Type consistency:** `Store`, `Agent`, `Thread`, `Entry`, `KVPair`, `proto.Request/Response`, `daemon.Serve`, `client.Call` used consistently across tasks; op names here (`register_agent`, `list_agents`, `send_message`, `task_create`, `task_claim`, `task_transition`, `reply`, `get_inbox`, `kv_set`, `kv_get`) are the same strings the MCP layer will wrap in Milestone B.
