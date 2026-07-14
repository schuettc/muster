# Session Identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make muster's agent registry self-maintaining and human-legible — a plain `muster register`/`deregister` CLI (so shell hooks can auto-register), tmux-verified liveness, and agents surfaced/addressed by their project-scoped, manually-pinned tmux label.

**Architecture:** Additive. A new `internal/tmuxenv` package becomes the single canonical place for tmux interaction outside the daemon (capture, project derivation, liveness, label). The store gains `project`/`label`/`label_manual` columns; the daemon gains a `deregister_agent` op; the human CLI gains `register`/`deregister`/`gc`, a project-scoped target resolver reused by every target-taking command, and liveness/label enrichment in `agents`. The daemon core stays tmux-agnostic.

**Tech Stack:** Go 1.26, cgo-free, `modernc.org/sqlite`, official MCP go-sdk v1.6.1, `text/tabwriter`, stdlib `os/exec` for tmux.

## Global Constraints

- **cgo-free:** builds under `CGO_ENABLED=0`; no new non-pure-Go deps.
- **`just verify` is the gate:** gofmt, golangci-lint (errcheck, revive, gocritic, staticcheck), `go test -race`, build — all green, locally and in CI. Run as `just verify` (add `TMPDIR=/tmp` on macOS; prefix `GODEBUG=netdns=go` if module DNS flakes).
- **Exported symbols need doc comments** (revive); ignored params named `_`.
- **Daemon core stays tmux-agnostic** — tmux is touched ONLY through `internal/tmuxenv` (CLI layer) or the injected `wake.Notifier`. Do not import `tmuxenv` into `internal/daemon` or `internal/store`.
- **One canonical capture path** — after Task 2, no tmux env/query logic remains in `internal/mcpserver`; it delegates to `tmuxenv`. No duplicated tmux code anywhere.
- **Tests never shell to real tmux** — inject `tmuxenv.Run`. A test that sets `tmuxenv.Run` must restore it (`defer`) and must not call `t.Parallel()`.
- **Identity rules (verbatim):** alias precedence = explicit arg → `$MUSTER_ALIAS` → tmux session name. A label is addressable ONLY when manually pinned (`@claude_task_manual == "1"`). Cross-project qualifier is `:` (`proj:label`), never `/`. Bare-label resolution restricts to the caller's project and NEVER silently crosses a project boundary — ambiguity or cross-project-without-qualifier is a loud error listing candidates as `proj:label`.
- **Label option name** defaults to `@claude_task`, overridable via `$MUSTER_LABEL_OPTION`; its manual companion is `<option>_manual`.
- **Tests on macOS** use `internal/mustertest.ShortHome()` for any daemon/socket path (sun_path length limit); follow the existing `startTestDaemon` pattern.

---

### Task 1: `internal/tmuxenv` package

**Files:**
- Create: `internal/tmuxenv/tmuxenv.go`
- Test: `internal/tmuxenv/tmuxenv_test.go`

**Interfaces:**
- Consumes: nothing (stdlib only).
- Produces (consumed by Tasks 2, 5, 6):
  - `var Run func(args ...string) (string, error)` — executes `tmux <args>`; overridable in tests.
  - `type Capture struct { SocketPath, PaneID, SessionID, SessionName, Project, Label string; LabelManual bool }`
  - `func CaptureEnv() Capture`
  - `func SocketFromEnv() string`
  - `func ProjectFromSocket(socket string) string`
  - `func LabelOption() string`
  - `func IsSessionAlive(socket, sessionID string) bool`
  - `func SessionLabel(socket, target string) (label string, manual bool)`

- [ ] **Step 1: Write the failing tests**

`internal/tmuxenv/tmuxenv_test.go`:

```go
package tmuxenv

import (
	"fmt"
	"testing"
)

func withRun(t *testing.T, fn func(args ...string) (string, error)) {
	t.Helper()
	prev := Run
	Run = fn
	t.Cleanup(func() { Run = prev })
}

func TestProjectFromSocket(t *testing.T) {
	cases := map[string]string{
		"/private/tmp/tmux-501/proj-muster": "muster",
		"/tmp/tmux-0/proj-foo-bar":          "foo-bar",
		"/tmp/tmux-0/default":               "",
		"":                                  "",
	}
	for in, want := range cases {
		if got := ProjectFromSocket(in); got != want {
			t.Errorf("ProjectFromSocket(%q)=%q want %q", in, got, want)
		}
	}
}

func TestIsSessionAlive(t *testing.T) {
	withRun(t, func(args ...string) (string, error) { return "", nil })
	if !IsSessionAlive("/s", "$1") {
		t.Fatal("want alive when has-session exits 0")
	}
	withRun(t, func(args ...string) (string, error) { return "", fmt.Errorf("no such session") })
	if IsSessionAlive("/s", "$1") {
		t.Fatal("want dead when has-session errors")
	}
	if IsSessionAlive("", "$1") || IsSessionAlive("/s", "") {
		t.Fatal("empty socket/session must be dead")
	}
}

func TestSessionLabelManualVsAuto(t *testing.T) {
	withRun(t, func(args ...string) (string, error) { return "frontend\x1f1", nil })
	if l, m := SessionLabel("/s", "$1"); l != "frontend" || !m {
		t.Fatalf("manual: got (%q,%v)", l, m)
	}
	withRun(t, func(args ...string) (string, error) { return "some auto topic\x1f", nil })
	if l, m := SessionLabel("/s", "$1"); l != "some auto topic" || m {
		t.Fatalf("auto: got (%q,%v)", l, m)
	}
}

func TestCaptureEnvNoTmux(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	c := CaptureEnv()
	if c.SocketPath != "" || c.Project != "" || c.SessionID != "" {
		t.Fatalf("no-tmux capture should be empty, got %+v", c)
	}
}

func TestCaptureEnvPopulated(t *testing.T) {
	t.Setenv("TMUX", "/private/tmp/tmux-501/proj-muster,123,0")
	t.Setenv("TMUX_PANE", "%0")
	withRun(t, func(args ...string) (string, error) {
		last := args[len(args)-1]
		switch {
		case last == "#{session_id}":
			return "$7", nil
		case last == "#{session_name}":
			return "muster-2", nil
		default: // label format
			return "backend\x1f1", nil
		}
	})
	c := CaptureEnv()
	if c.Project != "muster" || c.SessionID != "$7" || c.SessionName != "muster-2" ||
		c.Label != "backend" || !c.LabelManual {
		t.Fatalf("capture=%+v", c)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `TMPDIR=/tmp go test ./internal/tmuxenv/`
Expected: FAIL (package/functions not defined).

- [ ] **Step 3: Write the implementation**

`internal/tmuxenv/tmuxenv.go`:

```go
// Package tmuxenv is muster's single point of contact with tmux from outside
// the daemon: capturing the current pane's identity, deriving the project from
// the per-project socket, checking session liveness, and reading the session
// label. All tmux execution goes through Run, which tests override.
package tmuxenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Run executes `tmux <args>` and returns trimmed stdout. Overridable in tests.
var Run = func(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Capture holds the identity fields for registering an agent from a tmux pane.
// Every field is empty (LabelManual false) when not running inside tmux.
type Capture struct {
	SocketPath  string
	PaneID      string
	SessionID   string
	SessionName string
	Project     string
	Label       string
	LabelManual bool
}

// SocketFromEnv returns the tmux socket path from $TMUX ("<socket>,<pid>,<idx>").
func SocketFromEnv() string {
	tmux := os.Getenv("TMUX")
	if tmux == "" {
		return ""
	}
	return strings.SplitN(tmux, ",", 2)[0]
}

// ProjectFromSocket derives the project name from a per-project socket path
// ("proj-<project>"). Returns "" for non-proj-managed sockets (e.g. "default").
func ProjectFromSocket(socket string) string {
	if socket == "" {
		return ""
	}
	base := filepath.Base(socket)
	if !strings.HasPrefix(base, "proj-") {
		return ""
	}
	return strings.TrimPrefix(base, "proj-")
}

// LabelOption returns the tmux session option holding the label, defaulting to
// "@claude_task", overridable via $MUSTER_LABEL_OPTION.
func LabelOption() string {
	if v := os.Getenv("MUSTER_LABEL_OPTION"); v != "" {
		return v
	}
	return "@claude_task"
}

func query(socket, target, format string) string {
	if socket == "" || target == "" {
		return ""
	}
	out, err := Run("-S", socket, "display-message", "-p", "-t", target, format)
	if err != nil {
		return ""
	}
	return out
}

// IsSessionAlive reports whether the tmux session still exists on the socket.
func IsSessionAlive(socket, sessionID string) bool {
	if socket == "" || sessionID == "" {
		return false
	}
	_, err := Run("-S", socket, "has-session", "-t", sessionID)
	return err == nil
}

// SessionLabel reads the label option and its manual flag for target (a pane or
// session) on socket. manual is true only when <option>_manual == "1".
func SessionLabel(socket, target string) (string, bool) {
	opt := LabelOption()
	raw := query(socket, target, "#{"+opt+"}\x1f#{"+opt+"_manual}")
	if raw == "" {
		return "", false
	}
	parts := strings.SplitN(raw, "\x1f", 2)
	label := parts[0]
	manual := len(parts) > 1 && parts[1] == "1"
	return label, manual
}

// CaptureEnv reads the current process's tmux environment into a Capture.
func CaptureEnv() Capture {
	socket := SocketFromEnv()
	pane := os.Getenv("TMUX_PANE")
	c := Capture{SocketPath: socket, PaneID: pane, Project: ProjectFromSocket(socket)}
	if socket == "" || pane == "" {
		return c
	}
	c.SessionID = query(socket, pane, "#{session_id}")
	c.SessionName = query(socket, pane, "#{session_name}")
	c.Label, c.LabelManual = SessionLabel(socket, pane)
	return c
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `TMPDIR=/tmp go test ./internal/tmuxenv/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tmuxenv/
git commit -m "feat(tmuxenv): canonical tmux capture, project, liveness, label"
```

---

### Task 2: Repoint mcpserver to tmuxenv (one canonical capture)

**Files:**
- Modify: `internal/mcpserver/tools_registry.go`
- Modify: `internal/mcpserver/tools_registry_test.go`

**Interfaces:**
- Consumes: `tmuxenv.CaptureEnv`, `tmuxenv.Run` (Task 1).
- Produces: `register_agent` daemon call now also sends `project`, `label`, `label_manual`.

- [ ] **Step 1: Update the test to inject `tmuxenv.Run`**

Replace the body of `internal/mcpserver/tools_registry_test.go` with a version that drives capture through `tmuxenv` and asserts the new fields flow into the daemon call. The existing test set `TMUX`/`TMUX_PANE` and overrode the old `tmuxQuery`; now override `tmuxenv.Run`:

```go
package mcpserver

import (
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

func TestRegisterAgentCapturesTmuxEnv(t *testing.T) {
	t.Setenv("TMUX", "/private/tmp/tmux-501/proj-muster,123,0")
	t.Setenv("TMUX_PANE", "%6")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		switch args[len(args)-1] {
		case "#{session_id}":
			return "$5", nil
		case "#{session_name}":
			return "muster-2", nil
		default:
			return "backend\x1f1", nil
		}
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var got map[string]any
	prevDaemon := callDaemon
	callDaemon = func(op string, args map[string]any) ([]byte, error) {
		got = args
		return []byte(`{}`), nil
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, _, err := registerAgentHandler(nil, nil, RegisterAgentIn{Alias: "backend", Role: "producer", ModelType: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if got["socket_path"] != "/private/tmp/tmux-501/proj-muster" || got["session_id"] != "$5" ||
		got["project"] != "muster" || got["label"] != "backend" || got["label_manual"] != true {
		t.Fatalf("captured args = %+v", got)
	}
}
```

> Note: this assumes `callDaemon` is a package-level `var` so it can be stubbed. If it is currently a function declaration, change it to `var callDaemon = func(...) (...) {...}` in this task (mechanical). Verify its real signature in `internal/mcpserver` and match it exactly in the stub.

- [ ] **Step 2: Run the test to verify it fails**

Run: `TMPDIR=/tmp go test ./internal/mcpserver/ -run TestRegisterAgentCapturesTmuxEnv`
Expected: FAIL (compile error / missing fields).

- [ ] **Step 3: Rewrite `tools_registry.go` capture to use tmuxenv**

Remove `tmuxSocketPath` and `tmuxQuery` entirely and drop the now-unused `os`/`os/exec`/`strings` imports (keep what's still used). Rewrite the handler:

```go
func registerAgentHandler(_ context.Context, _ *mcp.CallToolRequest, in RegisterAgentIn) (*mcp.CallToolResult, OKOut, error) {
	c := tmuxenv.CaptureEnv()
	sessionName := in.SessionName
	if sessionName == "" {
		sessionName = c.SessionName
	}
	_, err := callDaemon("register_agent", map[string]any{
		"alias":        in.Alias,
		"role":         in.Role,
		"model_type":   in.ModelType,
		"session_name": sessionName,
		"session_id":   c.SessionID,
		"socket_path":  c.SocketPath,
		"pane_id":      c.PaneID,
		"project":      c.Project,
		"label":        c.Label,
		"label_manual": c.LabelManual,
	})
	if err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true, Detail: "registered " + in.Alias}, nil
}
```

Add the import `"github.com/schuettc/muster/internal/tmuxenv"`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `TMPDIR=/tmp go test ./internal/mcpserver/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/
git commit -m "refactor(mcpserver): capture via tmuxenv; send project/label"
```

---

### Task 3: Store — project/label/label_manual columns, migration, DeleteAgent

**Files:**
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/store.go`
- Modify: `internal/store/models.go`
- Modify: `internal/store/agents.go`
- Test: `internal/store/agents_test.go` (add cases; create the file if absent)

**Interfaces:**
- Consumes: nothing new.
- Produces (Task 4): `store.Agent` fields `Project string`, `Label string`, `LabelManual bool`; `func (s *Store) DeleteAgent(alias string) error`.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/agents_test.go` (match the package's existing test-store constructor; use whatever helper other store tests use to open a temp DB, e.g. `openTestStore(t)`):

```go
func TestAgentLabelAndDelete(t *testing.T) {
	s := openTestStore(t) // existing helper in this package
	if err := s.RegisterAgent(Agent{
		Alias: "muster-2", Role: "peer", ModelType: "codex",
		SocketPath: "/tmp/tmux-0/proj-muster", SessionID: "$1",
		Project: "muster", Label: "frontend", LabelManual: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetAgent("muster-2")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Project != "muster" || got.Label != "frontend" || !got.LabelManual {
		t.Fatalf("round-trip=%+v", got)
	}

	// upsert refreshes label fields
	if err := s.RegisterAgent(Agent{Alias: "muster-2", Label: "backend", LabelManual: false}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetAgent("muster-2")
	if got.Label != "backend" || got.LabelManual {
		t.Fatalf("after upsert=%+v", got)
	}

	// delete removes the row, leaves the table usable
	if err := s.DeleteAgent("muster-2"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetAgent("muster-2"); ok {
		t.Fatal("agent should be gone after DeleteAgent")
	}
	if err := s.DeleteAgent("nonexistent"); err != nil {
		t.Fatalf("DeleteAgent of unknown alias must be a no-op, got %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `TMPDIR=/tmp go test ./internal/store/ -run TestAgentLabelAndDelete`
Expected: FAIL (fields/method undefined).

- [ ] **Step 3: Add the columns to fresh schema**

In `internal/store/schema.sql`, add three columns to the `agents` table (after `session_id`):

```sql
    session_id    TEXT NOT NULL DEFAULT '',
    project       TEXT NOT NULL DEFAULT '',
    label         TEXT NOT NULL DEFAULT '',
    label_manual  INTEGER NOT NULL DEFAULT 0,
    registered_at INTEGER NOT NULL,
    last_seen     INTEGER NOT NULL
```

- [ ] **Step 4: Add an idempotent migration to `store.go`**

In `internal/store/store.go`, after the `db.Exec(schemaSQL)` block in `Open`, call `migrate(db)`:

```go
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
```

Add:

```go
// migrate applies additive column migrations to pre-existing databases. Each
// ALTER is guarded so a re-run (column already present) is a no-op.
func migrate(db *sql.DB) error {
	alters := []string{
		`ALTER TABLE agents ADD COLUMN project TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN label TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN label_manual INTEGER NOT NULL DEFAULT 0`,
	}
	for _, ddl := range alters {
		if _, err := db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}
```

Add `"strings"` to `store.go` imports.

- [ ] **Step 5: Add the fields to the model**

In `internal/store/models.go`, extend `Agent` (place the three after `SessionID`):

```go
	SessionID    string `json:"session_id"`
	Project      string `json:"project"`
	Label        string `json:"label"`
	LabelManual  bool   `json:"label_manual"`
	RegisteredAt int64  `json:"registered_at"`
	LastSeen     int64  `json:"last_seen"`
```

- [ ] **Step 6: Update agents.go queries + add DeleteAgent**

In `internal/store/agents.go`, update `RegisterAgent`, `ListAgents`, `GetAgent` to include the new columns, and add `DeleteAgent`:

```go
func (s *Store) RegisterAgent(a Agent) error {
	now := clock.NowMillis()
	_, err := s.db.Exec(`
INSERT INTO agents (alias, role, model_type, socket_path, pane_id, session_name, session_id, project, label, label_manual, registered_at, last_seen)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(alias) DO UPDATE SET
    role=excluded.role,
    model_type=excluded.model_type,
    socket_path=excluded.socket_path,
    pane_id=excluded.pane_id,
    session_name=excluded.session_name,
    session_id=excluded.session_id,
    project=excluded.project,
    label=excluded.label,
    label_manual=excluded.label_manual,
    last_seen=excluded.last_seen`,
		a.Alias, a.Role, a.ModelType, a.SocketPath, a.PaneID, a.SessionName, a.SessionID,
		a.Project, a.Label, a.LabelManual, now, now)
	return err
}
```

`ListAgents` and `GetAgent`: add `project, label, label_manual` to the SELECT column list (after `session_id`) and to the `Scan` targets (`&a.Project, &a.Label, &a.LabelManual`). `database/sql` scans the INTEGER 0/1 into `*bool` via `driver.Bool`.

```go
// DeleteAgent removes an agent's registration by alias. Unknown alias is a
// no-op (no error). Message/task history is unaffected — threads store the
// alias as text, not a foreign key.
func (s *Store) DeleteAgent(alias string) error {
	_, err := s.db.Exec(`DELETE FROM agents WHERE alias=?`, alias)
	return err
}
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `TMPDIR=/tmp go test ./internal/store/`
Expected: PASS (new + existing).

- [ ] **Step 8: Commit**

```bash
git add internal/store/
git commit -m "feat(store): project/label/label_manual columns + DeleteAgent + migration"
```

---

### Task 4: Daemon — register_agent new args + deregister_agent op

**Files:**
- Modify: `internal/daemon/daemon.go`
- Test: `internal/daemon/daemon_test.go`

**Interfaces:**
- Consumes: `store.Agent` new fields, `store.DeleteAgent` (Task 3).
- Produces (Tasks 5, 6): daemon op `deregister_agent {alias}`; `register_agent` now persists `project`/`label`/`label_manual`.

- [ ] **Step 1: Write the failing test**

Add to `internal/daemon/daemon_test.go` (follow the existing `client.Call(sock, ...)` pattern used by the tests already in this file):

```go
func TestRegisterCapturesLabelAndDeregister(t *testing.T) {
	sock := startTestDaemon(t) // existing helper
	if _, err := client.Call(sock, proto.Request{Op: "register_agent", Args: map[string]any{
		"alias": "muster-2", "role": "peer", "model_type": "codex",
		"socket_path": "/s", "session_id": "$1",
		"project": "muster", "label": "frontend", "label_manual": true,
	}}); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Call(sock, proto.Request{Op: "get_agent", Args: map[string]any{"alias": "muster-2"}})
	if err != nil || !resp.OK {
		t.Fatalf("get_agent: %v %+v", err, resp)
	}
	// (decode resp.Data → agent; assert Project=="muster", Label=="frontend", LabelManual==true)

	if _, err := client.Call(sock, proto.Request{Op: "deregister_agent", Args: map[string]any{"alias": "muster-2"}}); err != nil {
		t.Fatal(err)
	}
	resp, _ = client.Call(sock, proto.Request{Op: "get_agent", Args: map[string]any{"alias": "muster-2"}})
	// assert resp.Data.found == false
}
```

Fill the decode/assert comments using the same JSON-decoding approach the other tests in this file already use for `get_agent` (`found`/`agent`).

- [ ] **Step 2: Run the test to verify it fails**

Run: `TMPDIR=/tmp go test ./internal/daemon/ -run TestRegisterCapturesLabelAndDeregister`
Expected: FAIL (project not persisted; unknown op `deregister_agent`).

- [ ] **Step 3: Add a bool arg helper**

In `internal/daemon/daemon.go`, next to the existing `str`/`i64` arg helpers, add:

```go
// boolArg reads a bool arg, accepting a JSON bool or the strings "true"/"1"
// (the debug CLI passes all args as strings).
func boolArg(a map[string]any, key string) bool {
	switch v := a[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	default:
		return false
	}
}
```

- [ ] **Step 4: Thread the new fields + add the op**

In the `register_agent` case, extend the `store.Agent` literal:

```go
	case "register_agent":
		err := d.s.RegisterAgent(store.Agent{
			Alias: str(a, "alias"), Role: str(a, "role"), ModelType: str(a, "model_type"),
			SocketPath: str(a, "socket_path"), PaneID: str(a, "pane_id"), SessionName: str(a, "session_name"),
			SessionID: str(a, "session_id"),
			Project:   str(a, "project"), Label: str(a, "label"), LabelManual: boolArg(a, "label_manual"),
		})
		if err != nil {
			return fail(err)
		}
		return ok(nil)
```

Add a new case (next to `get_agent`):

```go
	case "deregister_agent":
		if err := d.s.DeleteAgent(str(a, "alias")); err != nil {
			return fail(err)
		}
		return ok(nil)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `TMPDIR=/tmp go test ./internal/daemon/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/
git commit -m "feat(daemon): persist project/label; add deregister_agent op"
```

---

### Task 5: humancli — register / deregister / gc commands

**Files:**
- Modify: `internal/humancli/humancli.go` (extend `agentRow`, extend `Dispatch`)
- Create: `internal/humancli/identity.go` (the new commands)
- Modify: `cmd/muster/main.go` (dispatch + usage)
- Test: `internal/humancli/identity_test.go`

**Interfaces:**
- Consumes: `tmuxenv` (Task 1), daemon ops `register_agent`/`deregister_agent`/`list_agents` (Tasks 3–4), `callData` (existing).
- Produces (Task 6): extended `agentRow` with `SocketPath`, `SessionID`, `Project`, `Label`, `LabelManual`.

- [ ] **Step 1: Extend `agentRow`**

In `internal/humancli/humancli.go`, extend the `agentRow` struct so list_agents rows carry what liveness/resolution need:

```go
type agentRow struct {
	Alias       string `json:"alias"`
	Role        string `json:"role"`
	ModelType   string `json:"model_type"`
	SocketPath  string `json:"socket_path"`
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name"`
	Project     string `json:"project"`
	Label       string `json:"label"`
	LabelManual bool   `json:"label_manual"`
	LastSeen    int64  `json:"last_seen"`
}
```

- [ ] **Step 2: Write the failing test**

`internal/humancli/identity_test.go` — drive `register`/`deregister`/`gc` against a real temp daemon (reuse the humancli package's existing test daemon helper; if the package lacks one, start a `daemon.Serve` on a `mustertest.ShortHome()` socket exactly as other tests do) with `tmuxenv.Run` and env injected:

```go
func TestRegisterUsesAliasPrecedenceAndCaptures(t *testing.T) {
	sock := startCLITestDaemon(t) // package helper (create if absent, mirroring daemon tests)
	t.Setenv("TMUX", "/tmp/tmux-0/proj-muster,1,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("MUSTER_ALIAS", "")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		switch args[len(args)-1] {
		case "#{session_id}":
			return "$1", nil
		case "#{session_name}":
			return "muster-2", nil
		default:
			return "frontend\x1f1", nil
		}
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	// no positional alias, no $MUSTER_ALIAS → alias falls back to session name
	if err := cmdRegister([]string{"--model", "codex", "--role", "peer"}, &buf); err != nil {
		t.Fatal(err)
	}
	// verify via list_agents that alias == "muster-2", project=="muster", label=="frontend"
	agents := listAgentsForTest(t, sock)
	if len(agents) != 1 || agents[0].Alias != "muster-2" || agents[0].Project != "muster" || agents[0].Label != "frontend" || !agents[0].LabelManual {
		t.Fatalf("registered=%+v", agents)
	}

	// explicit positional alias wins over session name
	buf.Reset()
	if err := cmdRegister([]string{"backend", "--model", "codex"}, &buf); err != nil {
		t.Fatal(err)
	}
	// now "backend" also present
}

func TestGCReapsOnlyDeadAgents(t *testing.T) {
	sock := startCLITestDaemon(t)
	// register two agents directly via the daemon: one "alive", one "dead"
	registerViaDaemon(t, sock, "alive", "/s", "$ALIVE")
	registerViaDaemon(t, sock, "dead", "/s", "$DEAD")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		// has-session succeeds only for $ALIVE
		if len(args) >= 5 && args[1] == "has-session" && args[4] == "$ALIVE" {
			return "", nil
		}
		if len(args) >= 1 && args[1] == "has-session" {
			return "", fmt.Errorf("dead")
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var buf bytes.Buffer
	if err := cmdGC(&buf); err != nil {
		t.Fatal(err)
	}
	agents := listAgentsForTest(t, sock)
	if len(agents) != 1 || agents[0].Alias != "alive" {
		t.Fatalf("after gc=%+v (want only 'alive')", agents)
	}
}
```

> The exact positions in `args` for `has-session` are `-S <socket> has-session -t <session>` → indices `0=-S,1=socket,2=has-session,3=-t,4=session`. Adjust the test matcher to those indices. Provide `startCLITestDaemon`, `listAgentsForTest`, `registerViaDaemon` as small local helpers (or reuse existing ones) — keep them in this test file.

- [ ] **Step 3: Run the test to verify it fails**

Run: `TMPDIR=/tmp go test ./internal/humancli/ -run 'TestRegister|TestGC'`
Expected: FAIL (cmds undefined).

- [ ] **Step 4: Implement the commands**

`internal/humancli/identity.go`:

```go
package humancli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// cmdRegister registers the current tmux session as an agent. Alias precedence:
// explicit positional arg → $MUSTER_ALIAS → tmux session name.
func cmdRegister(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	role := fs.String("role", "", "this agent's role")
	model := fs.String("model", "claude", "model backing this agent: claude or codex")
	flagArgs, rest := splitFlagsAndPositional(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	c := tmuxenv.CaptureEnv()
	alias := ""
	switch {
	case len(rest) > 0:
		alias = rest[0]
	case os.Getenv("MUSTER_ALIAS") != "":
		alias = os.Getenv("MUSTER_ALIAS")
	default:
		alias = c.SessionName
	}
	if alias == "" {
		return fmt.Errorf("cannot determine alias: not in a named tmux session; pass one explicitly or set $MUSTER_ALIAS")
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": alias, "role": *role, "model_type": *model,
		"session_name": c.SessionName, "session_id": c.SessionID,
		"socket_path": c.SocketPath, "pane_id": c.PaneID,
		"project": c.Project, "label": c.Label, "label_manual": c.LabelManual,
	}); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "registered %s (project %q, model %s)\n", alias, c.Project, *model)
	return err
}

// registerBoolFlags: register has no bool flags, so splitFlagsAndPositional
// treats --role/--model as value flags (correct). Nothing to add here.

// cmdDeregister removes an agent's registration. Alias precedence mirrors
// register: explicit arg → $MUSTER_ALIAS → tmux session name.
func cmdDeregister(args []string, out io.Writer) error {
	alias := ""
	if len(args) > 0 {
		alias = args[0]
	} else if os.Getenv("MUSTER_ALIAS") != "" {
		alias = os.Getenv("MUSTER_ALIAS")
	} else {
		alias = tmuxenv.CaptureEnv().SessionName
	}
	if alias == "" {
		return fmt.Errorf("cannot determine alias to deregister")
	}
	if _, err := callData("deregister_agent", map[string]any{"alias": alias}); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "deregistered %s\n", alias)
	return err
}

// cmdGC deregisters every agent whose tmux session is no longer alive.
func cmdGC(out io.Writer) error {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return err
	}
	var agents []agentRow
	if err := json.Unmarshal(raw, &agents); err != nil {
		return err
	}
	reaped := 0
	for _, a := range agents {
		if tmuxenv.IsSessionAlive(a.SocketPath, a.SessionID) {
			continue
		}
		if _, err := callData("deregister_agent", map[string]any{"alias": a.Alias}); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "reaped %s (dead session)\n", a.Alias); err != nil {
			return err
		}
		reaped++
	}
	_, err = fmt.Fprintf(out, "gc: reaped %d\n", reaped)
	return err
}
```

Add `"encoding/json"` to this file's imports (used by `cmdGC`).

- [ ] **Step 5: Wire into `Dispatch` and `main.go`**

In `internal/humancli/humancli.go` `Dispatch`, add cases:

```go
	case "register":
		return cmdRegister(args[1:], out)
	case "deregister":
		return cmdDeregister(args[1:], out)
	case "gc":
		return cmdGC(out)
```

Update the `Dispatch` usage string to include `register|deregister|gc`.

In `cmd/muster/main.go`, add the three verbs to the humancli case and the usage banners:

```go
	case "agents", "inbox", "send", "tasks", "nudge", "register", "deregister", "gc":
		if err := humancli.Dispatch(os.Args[1:], os.Stdout); err != nil {
```

Update both `usage:` strings in `main.go` to list the new verbs.

- [ ] **Step 6: Run tests to verify they pass**

Run: `TMPDIR=/tmp go test ./internal/humancli/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/humancli/ cmd/muster/main.go
git commit -m "feat(cli): register/deregister/gc commands"
```

---

### Task 6: humancli — project-scoped resolver + agents enrichment + wiring

**Files:**
- Create: `internal/humancli/resolve.go`
- Modify: `internal/humancli/humancli.go` (`cmdAgents`, `cmdSend`, `cmdInbox`, `cmdTasks`, `cmdNudge`)
- Test: `internal/humancli/resolve_test.go`

**Interfaces:**
- Consumes: extended `agentRow` (Task 5), `tmuxenv` (Task 1).
- Produces: `ResolveTarget`, `enrichAgents`, used by all target-taking commands.

- [ ] **Step 1: Write the failing test**

`internal/humancli/resolve_test.go` — pure unit test of the resolver over in-memory enriched agents (no daemon needed):

```go
package humancli

import "testing"

func agents() []enrichedAgent {
	return []enrichedAgent{
		{agentRow: agentRow{Alias: "muster", Project: "muster"}, Live: true, EffLabel: "backend", EffManual: true},
		{agentRow: agentRow{Alias: "muster-2", Project: "muster"}, Live: true, EffLabel: "frontend", EffManual: true},
		{agentRow: agentRow{Alias: "timewalk", Project: "timewalk"}, Live: true, EffLabel: "frontend", EffManual: true},
		{agentRow: agentRow{Alias: "auto1", Project: "muster"}, Live: true, EffLabel: "some topic", EffManual: false},
	}
}

func TestResolveExactAliasWins(t *testing.T) {
	got, err := ResolveTarget(agents(), "timewalk", "muster")
	if err != nil || got != "timewalk" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestResolveBareLabelInCallerProject(t *testing.T) {
	got, err := ResolveTarget(agents(), "frontend", "muster")
	if err != nil || got != "muster-2" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestResolveBareLabelCrossProjectIsError(t *testing.T) {
	// caller in "scratch": "frontend" exists only in muster & timewalk → must error, not guess
	if _, err := ResolveTarget(agents(), "frontend", "scratch"); err == nil {
		t.Fatal("want error for cross-project bare label")
	}
}

func TestResolveQualified(t *testing.T) {
	got, err := ResolveTarget(agents(), "timewalk:frontend", "muster")
	if err != nil || got != "timewalk" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestResolveAutoTopicNotAddressable(t *testing.T) {
	if _, err := ResolveTarget(agents(), "some topic", "muster"); err == nil {
		t.Fatal("auto (non-manual) labels must not be addressable")
	}
}

func TestResolveUnknown(t *testing.T) {
	if _, err := ResolveTarget(agents(), "nope", "muster"); err == nil {
		t.Fatal("want unknown-agent error")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `TMPDIR=/tmp go test ./internal/humancli/ -run TestResolve`
Expected: FAIL (types/func undefined).

- [ ] **Step 3: Implement resolver + enrichment**

`internal/humancli/resolve.go`:

```go
package humancli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// enrichedAgent is an agentRow with live tmux-derived state overlaid.
type enrichedAgent struct {
	agentRow
	Live      bool
	EffLabel  string // live label if alive, else stored snapshot
	EffManual bool   // live manual flag if alive, else stored
}

// enrichAgents overlays liveness + live label onto each row. For a live
// session the label is re-read from tmux; for a dead one the stored snapshot
// stands in.
func enrichAgents(rows []agentRow) []enrichedAgent {
	out := make([]enrichedAgent, 0, len(rows))
	for _, a := range rows {
		e := enrichedAgent{agentRow: a, EffLabel: a.Label, EffManual: a.LabelManual}
		e.Live = tmuxenv.IsSessionAlive(a.SocketPath, a.SessionID)
		if e.Live {
			e.EffLabel, e.EffManual = tmuxenv.SessionLabel(a.SocketPath, a.SessionID)
		}
		out = append(out, e)
	}
	return out
}

// callerProject derives the calling shell's project from its own $TMUX socket.
func callerProject() string {
	return tmuxenv.ProjectFromSocket(tmuxenv.SocketFromEnv())
}

// ResolveTarget maps a user-supplied target to a unique agent alias, scoped to
// caller's project. Rules (in order): exact alias; qualified proj:label; bare
// addressable label within callerProject. Never silently crosses projects.
func ResolveTarget(agents []enrichedAgent, given, caller string) (string, error) {
	// 1. exact alias (globally unique)
	for _, a := range agents {
		if a.Alias == given {
			return a.Alias, nil
		}
	}
	// 2. qualified proj:label
	if proj, label, ok := strings.Cut(given, ":"); ok {
		var hits []string
		for _, a := range agents {
			if a.Project == proj && a.EffManual && a.EffLabel == label {
				hits = append(hits, a.Alias)
			}
		}
		return uniqueOrErr(hits, given)
	}
	// 3. bare label — restrict to caller's project
	var inProject, elsewhere []string
	for _, a := range agents {
		if a.EffManual && a.EffLabel == given {
			if a.Project == caller {
				inProject = append(inProject, a.Alias)
			} else {
				elsewhere = append(elsewhere, qualify(a.Project, given))
			}
		}
	}
	if len(inProject) > 0 {
		return uniqueOrErr(inProject, given)
	}
	if len(elsewhere) > 0 {
		sort.Strings(elsewhere)
		return "", fmt.Errorf("label %q is not in your project; qualify it: %s", given, strings.Join(elsewhere, ", "))
	}
	return "", fmt.Errorf("unknown agent %q", given)
}

func uniqueOrErr(hits []string, given string) (string, error) {
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("unknown agent %q", given)
	default:
		sort.Strings(hits)
		return "", fmt.Errorf("%q is ambiguous: %s", given, strings.Join(hits, ", "))
	}
}

func qualify(project, label string) string {
	if project == "" {
		return label
	}
	return project + ":" + label
}

// resolveVia lists agents, enriches them, and resolves given to an alias.
func resolveVia(given string) (string, error) {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return "", err
	}
	var rows []agentRow
	if err := jsonUnmarshal(raw, &rows); err != nil {
		return "", err
	}
	return ResolveTarget(enrichAgents(rows), given, callerProject())
}
```

> `jsonUnmarshal` here means `json.Unmarshal` — import `"encoding/json"` and call it directly (the alias is only to keep this snippet terse; write real `json.Unmarshal`).

- [ ] **Step 4: Rewrite `cmdAgents` to group + show LABEL/LIVE**

Replace `cmdAgents` in `humancli.go`:

```go
func cmdAgents(out io.Writer) error {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return err
	}
	var rows []agentRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return err
	}
	agents := enrichAgents(rows)
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].Project != agents[j].Project {
			return agents[i].Project < agents[j].Project
		}
		return agents[i].Alias < agents[j].Alias
	})
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "PROJECT\tALIAS\tLABEL\tMODEL\tLIVE"); err != nil {
		return err
	}
	for _, a := range agents {
		proj := a.Project
		if proj == "" {
			proj = "(none)"
		}
		label := a.EffLabel
		switch {
		case label == "":
			label = "—"
		case !a.EffManual:
			label = "(" + label + ")" // auto-topic: shown but not addressable
		}
		live := "✗"
		if a.Live {
			live = "●"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", proj, a.Alias, label, a.ModelType, live); err != nil {
			return err
		}
	}
	return tw.Flush()
}
```

Add `"sort"` to `humancli.go` imports.

- [ ] **Step 5: Route target-taking commands through the resolver**

- `cmdSend`: when `toKind == "agent"` (not role/broadcast), replace `toTarget = rest[0]` with a resolved alias:
  ```go
  toTarget = rest[0]
  if toKind == "agent" {
  	resolved, err := resolveVia(rest[0])
  	if err != nil {
  		return err
  	}
  	toTarget = resolved
  }
  ```
- `cmdInbox` / `cmdTasks`: resolve `args[0]` before `printThreads`:
  ```go
  alias, err := resolveVia(args[0])
  if err != nil {
  	return err
  }
  return printThreads(out, alias, <false|true>)
  ```
- `cmdNudge`: resolve `rest[0]` to `alias` before the `get_agent` call:
  ```go
  alias, err := resolveVia(rest[0])
  if err != nil {
  	return err
  }
  ```
  (replace the current `alias := rest[0]`).

Update the `usage:` strings for inbox/tasks/nudge/send to read `<alias|label|proj:label>`.

- [ ] **Step 6: Run the full package + verify**

Run: `TMPDIR=/tmp go test ./internal/humancli/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/humancli/
git commit -m "feat(cli): project-scoped target resolver; agents shows label+liveness"
```

---

### Task 7: Docs — README CLI + hook snippets

**Files:**
- Modify: `README.md`

**Interfaces:** none (docs).

- [ ] **Step 1: Update the CLI section**

In `README.md`, in the CLI section, document the new commands and addressing. Add:

```markdown
### Registering & liveness

Agents can self-register (so a shell hook can do it at session start):

    muster register [alias] --role <r> --model <claude|codex>
    muster deregister [alias]
    muster gc                 # reap agents whose tmux session is gone

`register` captures the tmux pane automatically. Alias precedence: explicit
arg → `$MUSTER_ALIAS` → tmux session name.

`muster agents` shows each agent's **project** (derived from the per-project
tmux socket) and its live **label** — the manually-pinned `@claude_task`
session option (`prefix T`). Auto-topics are shown parenthesized and are not
addressable.

### Addressing

Any command that takes a target — `send`, `nudge`, `inbox`, `tasks` — accepts:

- an **alias** (the tmux session name, globally unique): `muster nudge muster-2`
- a **label**, resolved within your current project: `muster send frontend "…"`
- a **qualified label** to cross projects: `muster send timewalk:frontend "…"`

A bare label never silently crosses projects; if it's ambiguous or only exists
elsewhere, muster errors and lists the `proj:label` candidates.
```

- [ ] **Step 2: Document the hooks**

Add a short subsection noting the SessionStart/SessionEnd hook wiring (Claude `settings.json`, Codex config) runs `muster register --model <t> || true` / `muster deregister || true`, and that the actual dotfiles wiring is maintained separately.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs(readme): register/deregister/gc + project-scoped addressing"
```

---

## Final verification

After all tasks: `TMPDIR=/tmp GODEBUG=netdns=go just verify` must pass (fmt, lint, `go test -race`, build). Then the whole-branch Opus review, then merge `feat/session-identity → dev` via PR.
