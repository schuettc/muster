# muster Milestone D — Human CLI — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Give a human operator a plain-shell way to observe and drive the bus — `muster agents`, `muster inbox <alias>`, `muster send ...`, `muster tasks <alias>` — so you can watch and steer cross-terminal coordination without being inside an agent session.

**Architecture:** A new `internal/humancli` package with a `Dispatch(args, out)` that routes subcommands, each calling the daemon via the existing `client.Call` (which lazily starts `muster serve`) and formatting human-readable output. `cmd/muster/main.go` gains cases delegating to it. No new daemon ops — everything reuses `list_agents`, `get_inbox`, `send_message`.

**Tech Stack:** Go 1.26 · stdlib `flag`, `text/tabwriter` · existing `internal/{client,proto,paths}`.

## Global Constraints

- **No new daemon ops, no new module deps.** Reuse `list_agents`, `get_inbox`, `send_message`.
- **Human CLI writes to stdout** (unlike `mcp` mode) — these are operator commands, not the MCP channel.
- **Wire JSON is snake_case** (the store models are json-tagged). Local row structs use snake_case `json` tags.
- **cgo-free**, `just verify` green. Lint strict (errcheck, revive doc comments + `_` for ignored params, gocritic, staticcheck).
- **macOS test note:** prefix tests/verify with `TMPDIR=/tmp` (unix-socket path length), `GODEBUG=netdns=go` for any go download.
- **Do not touch `runServe`/`runMCP`/`runDebug`** — only ADD new switch cases + new run functions, to keep this branch mergeable alongside Milestone C (which edits `runServe`).

---

## File Structure

```
internal/humancli/
├── humancli.go        # Dispatch + per-command funcs (agents, inbox, send, tasks) + row structs
└── humancli_test.go   # commands against a live in-process daemon
cmd/muster/main.go     # MODIFY: add cases "agents"/"inbox"/"send"/"tasks" → humancli.Dispatch; update usage
README.md              # MODIFY: add a "## CLI" section
```

---

### Task 1: humancli package + `agents` command + wiring

**Files:**
- Create: `internal/humancli/humancli.go`, `internal/humancli/humancli_test.go`
- Modify: `cmd/muster/main.go`

**Interfaces:**
- Produces:
  - `func Dispatch(args []string, out io.Writer) error` — `args[0]` is the subcommand; routes to the command funcs. Unknown subcommand → error.
  - `agentRow`/`threadRow` structs (snake_case tags) for decoding daemon responses.
  - `callData(op string, args map[string]any) (json.RawMessage, error)` helper (same shape as mcpserver's callDaemon, local to this package).

- [ ] **Step 1: Write the failing test**

Create `internal/humancli/humancli_test.go`:
```go
package humancli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/store"
)

// startTestDaemon boots a real in-process daemon on a temp socket.
func startTestDaemon(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := daemon.Serve(paths.SocketPath(), s, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
}

func TestAgentsCommandListsRegistered(t *testing.T) {
	startTestDaemon(t)
	// Register two agents directly via the daemon op (through Dispatch's helper).
	if _, err := callData("register_agent", map[string]any{"alias": "backend", "role": "producer", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Dispatch([]string{"agents"}, &buf); err != nil {
		t.Fatalf("agents: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "backend") || !strings.Contains(out, "consumer") || !strings.Contains(out, "producer") {
		t.Fatalf("agents output missing rows:\n%s", out)
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	if err := Dispatch([]string{"bogus"}, nil); err == nil {
		t.Fatalf("expected error for unknown subcommand")
	}
}
```
Note: `daemon.Serve` takes a third `wake.Waker` arg (nil here). If your branch's `daemon.Serve` still has the 2-arg form (Milestone C not yet merged into this branch), use `daemon.Serve(paths.SocketPath(), s)` instead — match whatever signature exists in THIS worktree's `internal/daemon/daemon.go`. Check it before writing the test.

- [ ] **Step 2: Run to verify it fails**

Run: `TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/humancli/ -v`
Expected: FAIL — package has no `Dispatch`/`callData`.

- [ ] **Step 3: Implement `humancli.go` (Dispatch + agents)**

Create `internal/humancli/humancli.go`:
```go
// Package humancli implements muster's operator subcommands (agents, inbox,
// send, tasks) that read/drive the bus from a plain shell.
package humancli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
)

type agentRow struct {
	Alias       string `json:"alias"`
	Role        string `json:"role"`
	ModelType   string `json:"model_type"`
	SessionName string `json:"session_name"`
	LastSeen    int64  `json:"last_seen"`
}

type threadRow struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	FromAgent string `json:"from_agent"`
	ToKind    string `json:"to_kind"`
	ToTarget  string `json:"to_target"`
	Subject   string `json:"subject"`
	Status    string `json:"status"`
}

// callData sends one op to the daemon and returns its Data as JSON, or an error
// if the transport failed or the daemon reported !OK.
func callData(op string, args map[string]any) (json.RawMessage, error) {
	resp, err := client.Call(paths.SocketPath(), proto.Request{Op: op, Args: args})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s: %s", op, resp.Error)
	}
	return json.Marshal(resp.Data)
}

// Dispatch routes an operator subcommand. args[0] is the subcommand name.
func Dispatch(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: muster <agents|inbox|send|tasks> [args]")
	}
	switch args[0] {
	case "agents":
		return cmdAgents(out)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func cmdAgents(out io.Writer) error {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return err
	}
	var agents []agentRow
	if err := json.Unmarshal(raw, &agents); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ALIAS\tROLE\tMODEL\tSESSION")
	for _, a := range agents {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Alias, a.Role, a.ModelType, a.SessionName)
	}
	return tw.Flush()
}
```

- [ ] **Step 4: Wire into main.go**

In `cmd/muster/main.go`: add `"github.com/schuettc/muster/internal/humancli"` to imports. Update the usage string to `usage: muster <serve|debug|mcp|agents|inbox|send|tasks> [args]`. Add ONE combined case to the switch (do not touch existing cases):
```go
	case "agents", "inbox", "send", "tasks":
		if err := humancli.Dispatch(os.Args[1:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "muster:", err)
			os.Exit(1)
		}
```

- [ ] **Step 5: Run to verify it passes**

Run: `TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/humancli/ -v`
Expected: PASS.

- [ ] **Step 6: `just verify` + commit**

Run: `TMPDIR=/tmp GODEBUG=netdns=go just verify` → green.
```bash
git add internal/humancli/ cmd/muster/main.go
git commit -m "feat(cli): humancli dispatch + agents command

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 2: `send` + `inbox` commands

**Files:**
- Modify: `internal/humancli/humancli.go`, `internal/humancli/humancli_test.go`

**Interfaces:**
- `cmdSend(args []string, out io.Writer) error` — `muster send <target> <body> [--from X] [--subject S] [--ref R] [--role] [--broadcast]`. Default `--from` = `human`. Default to_kind = `agent` (target = alias); `--role` makes target a role; `--broadcast` ignores target.
- `cmdInbox(args []string, out io.Writer) error` — `muster inbox <alias>`; prints the alias's threads.

- [ ] **Step 1: Write the failing test (append to humancli_test.go)**

```go
func TestSendThenInboxShowsMessage(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	var sendBuf bytes.Buffer
	if err := Dispatch([]string{"send", "consumer", "the API changed", "--from", "backend", "--subject", "heads up"}, &sendBuf); err != nil {
		t.Fatalf("send: %v", err)
	}
	var inboxBuf bytes.Buffer
	if err := Dispatch([]string{"inbox", "consumer"}, &inboxBuf); err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if !strings.Contains(inboxBuf.String(), "heads up") {
		t.Fatalf("inbox missing sent message:\n%s", inboxBuf.String())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/humancli/ -run TestSendThenInbox -v`
Expected: FAIL — `send`/`inbox` not routed.

- [ ] **Step 3: Implement send + inbox**

Add to `internal/humancli/humancli.go` — new imports `"flag"` and `"strings"`, new switch cases, and the funcs:
```go
	case "send":
		return cmdSend(args[1:], out)
	case "inbox":
		return cmdInbox(args[1:], out)
```
```go
func cmdSend(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "human", "sending agent alias")
	subject := fs.String("subject", "", "message subject")
	ref := fs.String("ref", "", "pointer to the work")
	role := fs.Bool("role", false, "treat target as a role")
	broadcast := fs.Bool("broadcast", false, "send to everyone")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	toKind, toTarget := "agent", ""
	switch {
	case *broadcast:
		toKind = "broadcast"
	case *role:
		toKind = "role"
	}
	var body string
	if *broadcast {
		if len(rest) < 1 {
			return fmt.Errorf("usage: muster send --broadcast <body>")
		}
		body = strings.Join(rest, " ")
	} else {
		if len(rest) < 2 {
			return fmt.Errorf("usage: muster send <target> <body> [--from X --subject S --ref R --role --broadcast]")
		}
		toTarget = rest[0]
		body = strings.Join(rest[1:], " ")
	}
	raw, err := callData("send_message", map[string]any{
		"from": *from, "to_kind": toKind, "to_target": toTarget,
		"subject": *subject, "ref": *ref, "body": body,
	})
	if err != nil {
		return err
	}
	var res struct {
		ThreadID int64 `json:"thread_id"`
	}
	_ = json.Unmarshal(raw, &res)
	fmt.Fprintf(out, "sent (thread %d)\n", res.ThreadID)
	return nil
}

func cmdInbox(args []string, out io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: muster inbox <alias>")
	}
	return printThreads(out, args[0], false)
}

// printThreads fetches an alias's inbox and prints it; if tasksOnly, only
// kind=task threads are shown.
func printThreads(out io.Writer, alias string, tasksOnly bool) error {
	raw, err := callData("get_inbox", map[string]any{"alias": alias})
	if err != nil {
		return err
	}
	var threads []threadRow
	if err := json.Unmarshal(raw, &threads); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tFROM\tTO\tSTATUS\tSUBJECT")
	for _, th := range threads {
		if tasksOnly && th.Kind != "task" {
			continue
		}
		to := th.ToKind
		if th.ToTarget != "" {
			to = th.ToKind + ":" + th.ToTarget
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", th.ID, th.Kind, th.FromAgent, to, th.Status, th.Subject)
	}
	return tw.Flush()
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/humancli/ -v`
Expected: PASS (all).

- [ ] **Step 5: `just verify` + commit**

Run: `TMPDIR=/tmp GODEBUG=netdns=go just verify` → green.
```bash
git add internal/humancli/
git commit -m "feat(cli): send + inbox commands

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 3: `tasks` command + README CLI section

**Files:**
- Modify: `internal/humancli/humancli.go`, `internal/humancli/humancli_test.go`, `README.md`

**Interfaces:**
- `cmdTasks(args []string, out io.Writer) error` — `muster tasks <alias>`; prints the alias's inbox filtered to `kind=task` (reuses `printThreads(out, alias, true)`).

- [ ] **Step 1: Write the failing test (append)**

```go
func TestTasksCommandShowsOnlyTasks(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "rev", "role": "reviewer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	// One message and one task addressed to rev's role.
	if _, err := callData("send_message", map[string]any{"from": "backend", "to_kind": "role", "to_target": "reviewer", "subject": "just a note", "body": "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("task_create", map[string]any{"from": "backend", "to_kind": "role", "to_target": "reviewer", "subject": "please review", "body": "y"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Dispatch([]string{"tasks", "rev"}, &buf); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "please review") {
		t.Fatalf("tasks output missing the task:\n%s", out)
	}
	if strings.Contains(out, "just a note") {
		t.Fatalf("tasks output should exclude the plain message:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/humancli/ -run TestTasksCommand -v`
Expected: FAIL — `tasks` not routed.

- [ ] **Step 3: Implement tasks**

Add the case + func to `humancli.go`:
```go
	case "tasks":
		return cmdTasks(args[1:], out)
```
```go
func cmdTasks(args []string, out io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: muster tasks <alias>")
	}
	return printThreads(out, args[0], true)
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `TMPDIR=/tmp GODEBUG=netdns=go go test ./internal/humancli/ -v`
Expected: PASS (all).

- [ ] **Step 5: Add the README CLI section**

In `README.md`, add after the `## MCP mode` section:
```markdown
## CLI

Beyond `muster mcp` (for agents), muster has operator commands you can run from
any shell to observe and drive the bus (they auto-start the daemon):

```bash
muster agents                              # who's registered
muster inbox <alias>                       # threads addressed to an agent
muster tasks <alias>                       # just the tasks for an agent
muster send <alias> "message"  --from me   # send a directed message
muster send --role reviewer "please look"  --from me   # to a role
muster send --broadcast "heads up"         --from me   # to everyone
```
```

- [ ] **Step 6: `just verify` + commit**

Run: `TMPDIR=/tmp GODEBUG=netdns=go just verify` → green.
```bash
git add internal/humancli/ README.md
git commit -m "feat(cli): tasks command + document the CLI

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

## Milestone D Definition of Done

- `just verify` green (fmt, lint, race, build) locally and CI.
- `muster agents|inbox|send|tasks` work from a plain shell against the (auto-started) daemon.
- No new daemon ops or module deps; existing subcommands (`serve`/`debug`/`mcp`) untouched.

## Self-Review notes (author)
- **Spec coverage:** the operator/human control-plane slice of the design's Milestone D (`send|inbox|tasks|agents`). The permanent Claude/Codex `mcp add` dotfiles wiring is handled separately (dotfiles repo) as part of live-test setup, not this muster-repo milestone.
- **No placeholders:** all code complete.
- **Type consistency:** `Dispatch(args, out)`, `callData(op, args)`, `agentRow`/`threadRow` (snake_case tags matching the json-tagged store wire), `printThreads(out, alias, tasksOnly)` shared by inbox+tasks.
- **Merge-safety with Milestone C:** only ADDs a switch case + new run delegation in main.go and a new package; does not touch `runServe` (which C edits). The one caveat the test notes: match the local `daemon.Serve` arity in this worktree (2-arg until C merges).
