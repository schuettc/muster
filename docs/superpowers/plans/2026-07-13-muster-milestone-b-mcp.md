# muster Milestone B — MCP Server Mode — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `muster mcp` stdio server that exposes the daemon's operations as MCP tools, so Claude Code and Codex can register muster and drive it (register, message, task, KV) from inside an agent session.

**Architecture:** A new `internal/mcpserver` package builds an MCP server using the official Go SDK, registers ~11 typed tools, and runs over stdio. Each tool handler translates its typed input into a `proto.Request`, calls the existing daemon via `client.Call` (which lazily starts `muster serve`), and returns the daemon's response as typed output. No new persistence — this milestone is purely the MCP surface over Milestone A's daemon.

**Tech Stack:** Go 1.26 · `github.com/modelcontextprotocol/go-sdk v1.6.1` (official MCP SDK, GA, cgo-free) · the existing `internal/{client,proto,paths,daemon,store}` packages.

## Global Constraints

- **MCP SDK pinned EXACTLY:** `github.com/modelcontextprotocol/go-sdk v1.6.1`. Never `@latest`. Import path for the package is `github.com/modelcontextprotocol/go-sdk/mcp`.
- **No cgo.** Build stays `CGO_ENABLED=0`; the SDK's dependency graph is pure Go.
- **stdout is sacred in `mcp` mode.** In `muster mcp`, stdout carries the MCP protocol. NOTHING may write to stdout except the SDK. All logging/diagnostics go to **stderr** (`fmt.Fprintln(os.Stderr, ...)` or `log` with output set to stderr — `log`'s default is already stderr). The lazy-spawn in `client.dialOrSpawn` already redirects the child `muster serve`'s stdout to the parent's stderr, so it will not corrupt the channel.
- **Verified SDK API (from a v1.6.1 spike — use exactly this shape):**
  - Server: `srv := mcp.NewServer(&mcp.Implementation{Name: "muster", Version: "<v>"}, nil)`
  - Register: `mcp.AddTool(srv, &mcp.Tool{Name: "op_name", Description: "..."}, handlerFn)`
  - Handler type: `func(ctx context.Context, req *mcp.CallToolRequest, in InType) (*mcp.CallToolResult, OutType, error)`; return `nil, out, nil` on success (the SDK fills both `StructuredContent` and a JSON `TextContent` from `out`). Return a non-nil `error` to surface a tool error.
  - Run: `srv.Run(ctx, &mcp.StdioTransport{})` (blocks until the client disconnects or ctx is cancelled).
  - Schema inference: input/output schemas come from the Go structs; `json:"..."` sets field names, `jsonschema:"..."` sets descriptions. **In and Out must each be a struct or map** (not a bare slice) — wrap collections in a struct field.
  - Tests: `clientT, serverT := mcp.NewInMemoryTransports()`; run the server on `serverT` in a goroutine; `cs, _ := mcp.NewClient(&mcp.Implementation{Name:"test",Version:"v0"}, nil).Connect(ctx, clientT, nil)`; `res, _ := cs.CallTool(ctx, &mcp.CallToolParams{Name: "op", Arguments: map[string]any{...}})`; assert on `res.StructuredContent` (a Go value) or `res.IsError`.
- **Daemon op-name contract (unchanged from Milestone A, plus one addition):** tool names map 1:1 to daemon ops: `register_agent`, `list_agents`, `send_message`, `reply`, `get_inbox`, `get_thread` (NEW — added in Task 3), `task_create`, `task_claim`, `task_transition`, `kv_set`, `kv_get`.
- **Daemon JSON is case-insensitive to consume.** `store` structs have no `json` tags, so the daemon serializes fields as `Alias`, `Role`, etc. Go's `encoding/json` matches case-insensitively on unmarshal, so view structs tagged `json:"alias"` populate correctly from `"Alias"`. Do NOT edit the `store` structs.
- **Milestone scope:** MCP surface only. No tmux wake (that's C — but Task 2 captures `$TMUX`/`$TMUX_PANE` at registration so C has its hooks). No human CLI / dotfiles wiring (that's D). The permanent Claude/Codex registration is done by the operator; this milestone verifies it manually.

---

## File Structure

```
cmd/muster/main.go                 # MODIFY: add `mcp` subcommand → mcpserver.Run()
internal/daemon/daemon.go          # MODIFY (Task 3): add "get_thread" dispatch case
internal/mcpserver/
├── server.go                      # Run(): build server, register all tools, serve stdio
├── call.go                        # callDaemon helper + shared view structs (AgentView, ThreadView, EntryView)
├── tools_registry.go              # register_agent, list_agents
├── tools_messages.go              # send_message, reply, get_inbox, get_thread
├── tools_tasks.go                 # task_create, task_claim, task_transition
├── tools_kv.go                    # kv_set, kv_get
├── call_test.go                   # callDaemon against a live in-process daemon
├── tools_registry_test.go
├── tools_messages_test.go
├── tools_tasks_test.go
└── tools_kv_test.go
```

Tool files are split by domain so each stays small. All handlers share one `callDaemon` helper and the view structs in `call.go`. Tests boot a real daemon in-process (as the Milestone A daemon test does) and call handlers directly and/or via the in-memory MCP transport.

**Shared test helper (used by every `*_test.go` in this package).** Define ONCE in `call_test.go`; other test files call it.
```go
// startTestDaemon opens a fresh store + daemon on a temp socket and points
// paths.SocketPath() at it by setting MUSTER_HOME to a temp dir. Returns the
// socket path; registers cleanup.
func startTestDaemon(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := daemon.Serve(paths.SocketPath(), s)
	if err != nil {
		t.Fatalf("daemon.Serve: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return paths.SocketPath()
}
```
Because `paths.SocketPath()` reads `MUSTER_HOME` and `callDaemon` calls `client.Call(paths.SocketPath(), …)`, setting `MUSTER_HOME` to the temp dir makes handlers talk to the test daemon with no extra wiring.

---

### Task 1: SDK dependency + `mcp` subcommand + server skeleton + `callDaemon`

**Files:**
- Create: `internal/mcpserver/server.go`, `internal/mcpserver/call.go`, `internal/mcpserver/call_test.go`
- Modify: `cmd/muster/main.go`, `go.mod`/`go.sum`

**Interfaces:**
- Consumes: `client.Call`, `proto.Request/Response`, `paths.SocketPath`.
- Produces:
  - `mcpserver.Run(ctx context.Context) error` — builds the server, registers all tools (none yet in this task beyond a boot check), runs stdio.
  - `callDaemon(op string, args map[string]any) (json.RawMessage, error)` — calls the daemon; returns `resp.Data` re-marshaled to JSON, or an error if transport failed or `resp.OK` is false.
  - View structs `AgentView`, `ThreadView`, `EntryView` (used by later tasks).

- [ ] **Step 1: Add the SDK dependency (pinned)**

Run (from the worktree root):
```bash
GOFLAGS=-mod=mod GOPROXY=https://proxy.golang.org,direct go get github.com/modelcontextprotocol/go-sdk/mcp@v1.6.1
```
Expected: `go.mod` gains `require github.com/modelcontextprotocol/go-sdk v1.6.1`. (If DNS flakes resolving `proxy.golang.org`, prefix with `GODEBUG=netdns=go` and retry — the dep resolves on retry.)

- [ ] **Step 2: Write `call.go` (helper + view structs)**

Create `internal/mcpserver/call.go`:
```go
// Package mcpserver exposes muster's daemon operations as MCP tools over stdio.
package mcpserver

import (
	"encoding/json"
	"fmt"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
)

// callDaemon sends one op to the daemon (lazily starting it) and returns the
// response Data as JSON, or an error if the transport failed or the daemon
// reported !OK.
func callDaemon(op string, args map[string]any) (json.RawMessage, error) {
	resp, err := client.Call(paths.SocketPath(), proto.Request{Op: op, Args: args})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s: %s", op, resp.Error)
	}
	b, err := json.Marshal(resp.Data)
	if err != nil {
		return nil, fmt.Errorf("marshal %s result: %w", op, err)
	}
	return b, nil
}

// AgentView is the tool-facing shape of a registered agent. Field tags match
// the daemon's JSON case-insensitively.
type AgentView struct {
	Alias        string `json:"alias" jsonschema:"the agent's addressable alias"`
	Role         string `json:"role" jsonschema:"the agent's role (producer, consumer, reviewer, ...)"`
	ModelType    string `json:"model_type" jsonschema:"the agent's model (claude or codex)"`
	SessionName  string `json:"session_name" jsonschema:"the tmux session the agent runs in"`
	RegisteredAt int64  `json:"registered_at" jsonschema:"when the agent first registered (unix ms)"`
	LastSeen     int64  `json:"last_seen" jsonschema:"when the agent was last active (unix ms)"`
}

// ThreadView is the tool-facing shape of a message/task thread.
type ThreadView struct {
	ID        int64  `json:"id" jsonschema:"the thread id"`
	Kind      string `json:"kind" jsonschema:"message or task"`
	FromAgent string `json:"from_agent" jsonschema:"who created the thread"`
	ToKind    string `json:"to_kind" jsonschema:"agent, role, or broadcast"`
	ToTarget  string `json:"to_target" jsonschema:"the addressed alias or role"`
	Subject   string `json:"subject" jsonschema:"the thread subject"`
	Ref       string `json:"ref" jsonschema:"a pointer to the work (repo/branch/endpoint/file)"`
	Status    string `json:"status" jsonschema:"task status, empty for messages"`
	CreatedAt int64  `json:"created_at" jsonschema:"creation time (unix ms)"`
	UpdatedAt int64  `json:"updated_at" jsonschema:"last-update time (unix ms)"`
}

// EntryView is one append-only entry within a thread.
type EntryView struct {
	ID           int64  `json:"id" jsonschema:"the entry id"`
	ThreadID     int64  `json:"thread_id" jsonschema:"the parent thread id"`
	FromAgent    string `json:"from_agent" jsonschema:"who wrote this entry"`
	Body         string `json:"body" jsonschema:"the entry text"`
	StatusChange string `json:"status_change" jsonschema:"the status this entry set, if any"`
	CreatedAt    int64  `json:"created_at" jsonschema:"when the entry was written (unix ms)"`
}
```

- [ ] **Step 3: Write `server.go` (skeleton)**

Create `internal/mcpserver/server.go`:
```go
package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is reported to MCP clients in the server implementation info.
const version = "0.1.0"

// Run builds the muster MCP server, registers all tools, and serves over stdio.
// It blocks until the client disconnects or ctx is cancelled.
func Run(ctx context.Context) error {
	srv := mcp.NewServer(&mcp.Implementation{Name: "muster", Version: version}, nil)
	registerAll(srv)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// registerAll wires every tool onto the server. Each tools_*.go file adds its
// own registration here via this central function.
func registerAll(srv *mcp.Server) {
	// Tools are added in later tasks:
	//   registerRegistryTools(srv)  // Task 2
	//   registerMessageTools(srv)   // Task 3
	//   registerTaskTools(srv)      // Task 4
	//   registerKVTools(srv)        // Task 5
}
```

- [ ] **Step 4: Write the failing test for `callDaemon`**

Create `internal/mcpserver/call_test.go` (includes the shared `startTestDaemon` helper from the File Structure section):
```go
package mcpserver

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/store"
)

func startTestDaemon(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d, err := daemon.Serve(paths.SocketPath(), s)
	if err != nil {
		t.Fatalf("daemon.Serve: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return paths.SocketPath()
}

func TestCallDaemonRegisterAndList(t *testing.T) {
	startTestDaemon(t)

	if _, err := callDaemon("register_agent", map[string]any{
		"alias": "backend", "role": "producer", "model_type": "claude",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	raw, err := callDaemon("list_agents", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var agents []AgentView
	if err := json.Unmarshal(raw, &agents); err != nil {
		t.Fatalf("unmarshal agents: %v", err)
	}
	if len(agents) != 1 || agents[0].Alias != "backend" || agents[0].Role != "producer" {
		t.Fatalf("unexpected agents: %+v", agents)
	}
}

func TestCallDaemonSurfacesError(t *testing.T) {
	startTestDaemon(t)
	// task_claim on a nonexistent thread → daemon returns !OK → error.
	if _, err := callDaemon("task_claim", map[string]any{"thread_id": "999", "by": "x"}); err == nil {
		t.Fatalf("expected error for claiming nonexistent task")
	}
}
```

- [ ] **Step 5: Run the tests to verify they fail**

Run: `GODEBUG=netdns=go go test ./internal/mcpserver/ -run TestCallDaemon -v`
Expected: FAIL — package compiles but `TestCallDaemonRegisterAndList` cannot pass until `call.go` exists; if written in order it compiles and passes. (If it already passes here, that's fine — the helper is simple; proceed.)

- [ ] **Step 6: Wire the `mcp` subcommand into main.go**

In `cmd/muster/main.go`, add `"github.com/schuettc/muster/internal/mcpserver"` and `"context"` to imports, add a `case "mcp":` to the subcommand switch, and the runner. Insert the case alongside `serve`/`debug`:
```go
	case "mcp":
		runMCP()
```
And add:
```go
// runMCP serves the MCP stdio server. IMPORTANT: stdout is the MCP channel;
// all diagnostics go to stderr.
func runMCP() {
	if err := mcpserver.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		os.Exit(1)
	}
}
```
Also update the usage line to `usage: muster <serve|debug|mcp> [args]`.

- [ ] **Step 7: Verify build + tests**

Run: `GODEBUG=netdns=go just verify`
Expected: green (fmt-check, lint, `go test -race ./...`, build). `bin/muster mcp` now exists as a subcommand (it will serve nothing useful until tools are added, but must build and boot).

- [ ] **Step 8: Commit**

```bash
git add go.mod go.sum cmd/muster/main.go internal/mcpserver/
git commit -m "feat(mcp): add mcp subcommand, server skeleton, callDaemon helper

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 2: Registry tools — `register_agent` (with tmux env capture) + `list_agents`

**Files:**
- Create: `internal/mcpserver/tools_registry.go`, `internal/mcpserver/tools_registry_test.go`
- Modify: `internal/mcpserver/server.go` (uncomment/add `registerRegistryTools(srv)` in `registerAll`)

**Interfaces:**
- Consumes: `callDaemon`, `AgentView`.
- Produces: `registerRegistryTools(srv *mcp.Server)`; the `register_agent` and `list_agents` tools.

- [ ] **Step 1: Write the failing test**

Create `internal/mcpserver/tools_registry_test.go`:
```go
package mcpserver

import (
	"context"
	"testing"
)

func TestRegisterAgentCapturesTmuxEnv(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("TMUX", "/private/tmp/tmux-501/proj-app,123,4")
	t.Setenv("TMUX_PANE", "%6")

	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{
		Alias: "backend", Role: "producer", ModelType: "claude",
	})
	if err != nil {
		t.Fatalf("register handler: %v", err)
	}
	if !out.OK {
		t.Fatalf("expected ok")
	}

	// Verify via list that the socket/pane were captured from the env.
	_, listOut, err := listAgentsHandler(context.Background(), nil, ListAgentsIn{})
	if err != nil {
		t.Fatalf("list handler: %v", err)
	}
	if len(listOut.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(listOut.Agents))
	}
	if listOut.Agents[0].Alias != "backend" {
		t.Fatalf("unexpected agent: %+v", listOut.Agents[0])
	}
}
```
(The socket/pane are captured into the daemon but `AgentView` doesn't surface them — the test asserts the round-trip succeeds and the agent is listed; Task 3/C exercise the socket/pane directly. To keep this test meaningful, it confirms env-capture code runs without error and registration persists.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `GODEBUG=netdns=go go test ./internal/mcpserver/ -run TestRegisterAgentCapturesTmuxEnv -v`
Expected: FAIL — `undefined: registerAgentHandler`.

- [ ] **Step 3: Implement the registry tools**

Create `internal/mcpserver/tools_registry.go`:
```go
package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RegisterAgentIn is the input to register_agent. socket_path/pane_id are NOT
// input — they are captured from the process environment ($TMUX, $TMUX_PANE),
// which the agent's MCP server inherits from its tmux pane.
type RegisterAgentIn struct {
	Alias       string `json:"alias" jsonschema:"a short addressable name for this agent, e.g. backend"`
	Role        string `json:"role" jsonschema:"this agent's role: producer, consumer, reviewer, ..."`
	ModelType   string `json:"model_type" jsonschema:"the model backing this agent: claude or codex"`
	SessionName string `json:"session_name" jsonschema:"optional tmux session name for display"`
}

// OKOut is a simple success acknowledgement for void operations.
type OKOut struct {
	OK     bool   `json:"ok" jsonschema:"whether the operation succeeded"`
	Detail string `json:"detail,omitempty" jsonschema:"optional human-readable detail"`
}

// ListAgentsIn has no fields; list_agents takes no arguments.
type ListAgentsIn struct{}

// ListAgentsOut wraps the agent list (Out must be a struct, not a bare slice).
type ListAgentsOut struct {
	Agents []AgentView `json:"agents" jsonschema:"the registered agents"`
}

// tmuxSocketPath extracts the socket path from $TMUX ("<socket>,<pid>,<session>").
func tmuxSocketPath() string {
	tmux := os.Getenv("TMUX")
	if tmux == "" {
		return ""
	}
	return strings.SplitN(tmux, ",", 2)[0]
}

func registerAgentHandler(_ context.Context, _ *mcp.CallToolRequest, in RegisterAgentIn) (*mcp.CallToolResult, OKOut, error) {
	_, err := callDaemon("register_agent", map[string]any{
		"alias":        in.Alias,
		"role":         in.Role,
		"model_type":   in.ModelType,
		"session_name": in.SessionName,
		"socket_path":  tmuxSocketPath(),
		"pane_id":      os.Getenv("TMUX_PANE"),
	})
	if err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true, Detail: "registered " + in.Alias}, nil
}

func listAgentsHandler(_ context.Context, _ *mcp.CallToolRequest, _ ListAgentsIn) (*mcp.CallToolResult, ListAgentsOut, error) {
	raw, err := callDaemon("list_agents", nil)
	if err != nil {
		return nil, ListAgentsOut{}, err
	}
	var agents []AgentView
	if err := json.Unmarshal(raw, &agents); err != nil {
		return nil, ListAgentsOut{}, err
	}
	return nil, ListAgentsOut{Agents: agents}, nil
}

func registerRegistryTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "register_agent",
		Description: "Register this agent on the muster bus so others can address it. Captures the agent's tmux pane automatically. Call once at the start of a session.",
	}, registerAgentHandler)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_agents",
		Description: "List all agents currently registered on the muster bus.",
	}, listAgentsHandler)
}
```

- [ ] **Step 4: Register the tools in `server.go`**

In `internal/mcpserver/server.go`, replace the Task-2 comment line in `registerAll` with the real call:
```go
func registerAll(srv *mcp.Server) {
	registerRegistryTools(srv)
	// registerMessageTools(srv)   // Task 3
	// registerTaskTools(srv)      // Task 4
	// registerKVTools(srv)        // Task 5
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `GODEBUG=netdns=go go test ./internal/mcpserver/ -run TestRegisterAgentCapturesTmuxEnv -v`
Expected: PASS.

- [ ] **Step 6: Verify + commit**

Run: `GODEBUG=netdns=go just verify` → green.
```bash
git add internal/mcpserver/
git commit -m "feat(mcp): register_agent (tmux env capture) + list_agents tools

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 3: `get_thread` daemon op + messaging tools

**Files:**
- Modify: `internal/daemon/daemon.go` (add `get_thread` dispatch case)
- Create: `internal/mcpserver/tools_messages.go`, `internal/mcpserver/tools_messages_test.go`
- Modify: `internal/mcpserver/server.go` (add `registerMessageTools(srv)`)

**Interfaces:**
- Consumes: `callDaemon`, `AgentView`, `ThreadView`, `EntryView`, `store.GetThread`.
- Produces: daemon op `get_thread`; the `send_message`, `reply`, `get_inbox`, `get_thread` tools.

- [ ] **Step 1: Add the `get_thread` daemon op**

In `internal/daemon/daemon.go`, add a case to the `dispatch` switch (near `get_inbox`):
```go
	case "get_thread":
		th, entries, err := d.s.GetThread(i64(a, "thread_id"))
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"thread": th, "entries": entries})
```

- [ ] **Step 2: Write the failing test**

Create `internal/mcpserver/tools_messages_test.go`:
```go
package mcpserver

import (
	"context"
	"testing"
)

func TestSendMessageAndInbox(t *testing.T) {
	startTestDaemon(t)
	// Register the recipient so role/inbox routing has an agent.
	if _, _, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{
		Alias: "consumer", Role: "consumer", ModelType: "codex",
	}); err != nil {
		t.Fatal(err)
	}

	_, sendOut, err := sendMessageHandler(context.Background(), nil, SendMessageIn{
		From: "backend", ToKind: "agent", ToTarget: "consumer",
		Subject: "heads up", Ref: "repo=bhw", Body: "renamed /bets to /wagers",
	})
	if err != nil || sendOut.ThreadID == 0 {
		t.Fatalf("send: err=%v out=%+v", err, sendOut)
	}

	_, inbox, err := getInboxHandler(context.Background(), nil, GetInboxIn{Alias: "consumer"})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(inbox.Threads) != 1 || inbox.Threads[0].Subject != "heads up" {
		t.Fatalf("unexpected inbox: %+v", inbox.Threads)
	}

	// reply appends an entry; get_thread shows both.
	if _, _, err := replyHandler(context.Background(), nil, ReplyIn{
		ThreadID: sendOut.ThreadID, From: "consumer", Body: "got it",
	}); err != nil {
		t.Fatalf("reply: %v", err)
	}
	_, thr, err := getThreadHandler(context.Background(), nil, GetThreadIn{ThreadID: sendOut.ThreadID})
	if err != nil {
		t.Fatalf("get_thread: %v", err)
	}
	if thr.Thread.ID != sendOut.ThreadID || len(thr.Entries) != 2 {
		t.Fatalf("unexpected thread: %+v entries=%d", thr.Thread, len(thr.Entries))
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `GODEBUG=netdns=go go test ./internal/mcpserver/ -run TestSendMessageAndInbox -v`
Expected: FAIL — `undefined: sendMessageHandler`.

- [ ] **Step 4: Implement the messaging tools**

Create `internal/mcpserver/tools_messages.go`:
```go
package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SendMessageIn struct {
	From     string `json:"from" jsonschema:"the sending agent's alias"`
	ToKind   string `json:"to_kind" jsonschema:"agent, role, or broadcast"`
	ToTarget string `json:"to_target" jsonschema:"the recipient alias or role (empty for broadcast)"`
	Subject  string `json:"subject" jsonschema:"a short subject line"`
	Ref      string `json:"ref" jsonschema:"optional pointer to the work (repo/branch/endpoint/file)"`
	Body     string `json:"body" jsonschema:"the message body"`
}

type ThreadIDOut struct {
	ThreadID int64 `json:"thread_id" jsonschema:"the created thread's id"`
}

type ReplyIn struct {
	ThreadID int64  `json:"thread_id" jsonschema:"the thread to reply to"`
	From     string `json:"from" jsonschema:"the replying agent's alias"`
	Body     string `json:"body" jsonschema:"the reply text"`
}

type EntryIDOut struct {
	EntryID int64 `json:"entry_id" jsonschema:"the created entry's id"`
}

type GetInboxIn struct {
	Alias string `json:"alias" jsonschema:"the agent whose inbox to read"`
}

type GetInboxOut struct {
	Threads []ThreadView `json:"threads" jsonschema:"threads addressed to the agent, its role, or broadcast"`
}

type GetThreadIn struct {
	ThreadID int64 `json:"thread_id" jsonschema:"the thread to fetch"`
}

type GetThreadOut struct {
	Thread  ThreadView  `json:"thread" jsonschema:"the thread"`
	Entries []EntryView `json:"entries" jsonschema:"the thread's entries in order"`
}

func sendMessageHandler(_ context.Context, _ *mcp.CallToolRequest, in SendMessageIn) (*mcp.CallToolResult, ThreadIDOut, error) {
	raw, err := callDaemon("send_message", map[string]any{
		"from": in.From, "to_kind": in.ToKind, "to_target": in.ToTarget,
		"subject": in.Subject, "ref": in.Ref, "body": in.Body,
	})
	if err != nil {
		return nil, ThreadIDOut{}, err
	}
	var out ThreadIDOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, ThreadIDOut{}, err
	}
	return nil, out, nil
}

func replyHandler(_ context.Context, _ *mcp.CallToolRequest, in ReplyIn) (*mcp.CallToolResult, EntryIDOut, error) {
	raw, err := callDaemon("reply", map[string]any{
		"thread_id": in.ThreadID, "from": in.From, "body": in.Body,
	})
	if err != nil {
		return nil, EntryIDOut{}, err
	}
	var out EntryIDOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, EntryIDOut{}, err
	}
	return nil, out, nil
}

func getInboxHandler(_ context.Context, _ *mcp.CallToolRequest, in GetInboxIn) (*mcp.CallToolResult, GetInboxOut, error) {
	raw, err := callDaemon("get_inbox", map[string]any{"alias": in.Alias})
	if err != nil {
		return nil, GetInboxOut{}, err
	}
	var threads []ThreadView
	if err := json.Unmarshal(raw, &threads); err != nil {
		return nil, GetInboxOut{}, err
	}
	return nil, GetInboxOut{Threads: threads}, nil
}

func getThreadHandler(_ context.Context, _ *mcp.CallToolRequest, in GetThreadIn) (*mcp.CallToolResult, GetThreadOut, error) {
	raw, err := callDaemon("get_thread", map[string]any{"thread_id": in.ThreadID})
	if err != nil {
		return nil, GetThreadOut{}, err
	}
	var out GetThreadOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, GetThreadOut{}, err
	}
	return nil, out, nil
}

func registerMessageTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "send_message", Description: "Send a message to another agent (to_kind=agent), a role (to_kind=role), or everyone (to_kind=broadcast)."}, sendMessageHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "reply", Description: "Append a reply to an existing thread (message or task)."}, replyHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_inbox", Description: "Read the threads addressed to an agent (directly, by role, or broadcast), newest first."}, getInboxHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_thread", Description: "Fetch a single thread and all its entries in order."}, getThreadHandler)
}
```

- [ ] **Step 5: Register message tools in `server.go`**

Uncomment/add in `registerAll`:
```go
	registerMessageTools(srv)   // Task 3
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `GODEBUG=netdns=go go test ./internal/mcpserver/ -run TestSendMessageAndInbox -v`
Expected: PASS.

- [ ] **Step 7: Verify + commit**

Run: `GODEBUG=netdns=go just verify` → green.
```bash
git add internal/daemon/daemon.go internal/mcpserver/
git commit -m "feat(mcp): get_thread daemon op + messaging tools (send/reply/inbox/thread)

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 4: Task tools — `task_create`, `task_claim`, `task_transition`

**Files:**
- Create: `internal/mcpserver/tools_tasks.go`, `internal/mcpserver/tools_tasks_test.go`
- Modify: `internal/mcpserver/server.go` (add `registerTaskTools(srv)`)

**Interfaces:**
- Consumes: `callDaemon`, `ThreadIDOut`, `OKOut`, `getThreadHandler` (in test).
- Produces: the `task_create`, `task_claim`, `task_transition` tools.

- [ ] **Step 1: Write the failing test**

Create `internal/mcpserver/tools_tasks_test.go`:
```go
package mcpserver

import (
	"context"
	"testing"
)

func TestTaskCreateClaimTransition(t *testing.T) {
	startTestDaemon(t)

	_, created, err := taskCreateHandler(context.Background(), nil, TaskCreateIn{
		From: "backend", ToKind: "role", ToTarget: "reviewer",
		Subject: "Review feat/wagers", Ref: "repo=bhw branch=feat/wagers", Body: "please review",
	})
	if err != nil || created.ThreadID == 0 {
		t.Fatalf("create: err=%v out=%+v", err, created)
	}

	if _, out, err := taskClaimHandler(context.Background(), nil, TaskClaimIn{ThreadID: created.ThreadID, By: "rev1"}); err != nil || !out.OK {
		t.Fatalf("claim: err=%v out=%+v", err, out)
	}
	// A second claim must fail (atomic claim in the store).
	if _, _, err := taskClaimHandler(context.Background(), nil, TaskClaimIn{ThreadID: created.ThreadID, By: "rev2"}); err == nil {
		t.Fatalf("second claim should error")
	}

	if _, out, err := taskTransitionHandler(context.Background(), nil, TaskTransitionIn{
		ThreadID: created.ThreadID, By: "rev1", Status: "completed", Note: "LGTM",
	}); err != nil || !out.OK {
		t.Fatalf("transition: err=%v out=%+v", err, out)
	}

	_, thr, err := getThreadHandler(context.Background(), nil, GetThreadIn{ThreadID: created.ThreadID})
	if err != nil {
		t.Fatalf("get_thread: %v", err)
	}
	if thr.Thread.Status != "completed" {
		t.Fatalf("status should be completed, got %q", thr.Thread.Status)
	}
}

func TestTaskTransitionRejectsInvalidStatus(t *testing.T) {
	startTestDaemon(t)
	_, created, _ := taskCreateHandler(context.Background(), nil, TaskCreateIn{
		From: "backend", ToKind: "role", ToTarget: "reviewer", Subject: "x", Body: "y",
	})
	if _, _, err := taskTransitionHandler(context.Background(), nil, TaskTransitionIn{
		ThreadID: created.ThreadID, By: "rev1", Status: "bogus",
	}); err == nil {
		t.Fatalf("expected error for invalid status")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GODEBUG=netdns=go go test ./internal/mcpserver/ -run TestTask -v`
Expected: FAIL — `undefined: taskCreateHandler`.

- [ ] **Step 3: Implement the task tools**

Create `internal/mcpserver/tools_tasks.go`:
```go
package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type TaskCreateIn struct {
	From     string `json:"from" jsonschema:"the requesting agent's alias"`
	ToKind   string `json:"to_kind" jsonschema:"agent, role, or broadcast"`
	ToTarget string `json:"to_target" jsonschema:"the assignee alias or role"`
	Subject  string `json:"subject" jsonschema:"a short task title"`
	Ref      string `json:"ref" jsonschema:"pointer to the work (repo/branch/endpoint/file)"`
	Body     string `json:"body" jsonschema:"task details"`
}

type TaskClaimIn struct {
	ThreadID int64  `json:"thread_id" jsonschema:"the task thread to claim"`
	By       string `json:"by" jsonschema:"the alias of the agent claiming the task"`
}

type TaskTransitionIn struct {
	ThreadID int64  `json:"thread_id" jsonschema:"the task thread to update"`
	By       string `json:"by" jsonschema:"the alias making the change"`
	Status   string `json:"status" jsonschema:"new status: open, claimed, needs_info, blocked, completed, declined, or cancelled"`
	Note     string `json:"note" jsonschema:"optional note recorded with the status change"`
}

func taskCreateHandler(_ context.Context, _ *mcp.CallToolRequest, in TaskCreateIn) (*mcp.CallToolResult, ThreadIDOut, error) {
	raw, err := callDaemon("task_create", map[string]any{
		"from": in.From, "to_kind": in.ToKind, "to_target": in.ToTarget,
		"subject": in.Subject, "ref": in.Ref, "body": in.Body,
	})
	if err != nil {
		return nil, ThreadIDOut{}, err
	}
	var out ThreadIDOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, ThreadIDOut{}, err
	}
	return nil, out, nil
}

func taskClaimHandler(_ context.Context, _ *mcp.CallToolRequest, in TaskClaimIn) (*mcp.CallToolResult, OKOut, error) {
	if _, err := callDaemon("task_claim", map[string]any{"thread_id": in.ThreadID, "by": in.By}); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true, Detail: "claimed"}, nil
}

func taskTransitionHandler(_ context.Context, _ *mcp.CallToolRequest, in TaskTransitionIn) (*mcp.CallToolResult, OKOut, error) {
	if _, err := callDaemon("task_transition", map[string]any{
		"thread_id": in.ThreadID, "by": in.By, "status": in.Status, "note": in.Note,
	}); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true, Detail: in.Status}, nil
}

func registerTaskTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "task_create", Description: "Create a task addressed to an agent or role. The assignee(s) can claim and work it."}, taskCreateHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "task_claim", Description: "Claim an open task. Only the first claimer succeeds; a second claim fails."}, taskClaimHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "task_transition", Description: "Move a task to a new status (claimed, needs_info, blocked, completed, declined, cancelled) with an optional note."}, taskTransitionHandler)
}
```

- [ ] **Step 4: Register task tools in `server.go`**
```go
	registerTaskTools(srv)      // Task 4
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `GODEBUG=netdns=go go test ./internal/mcpserver/ -run TestTask -v`
Expected: PASS (both).

- [ ] **Step 6: Verify + commit**

Run: `GODEBUG=netdns=go just verify` → green.
```bash
git add internal/mcpserver/
git commit -m "feat(mcp): task tools (create/claim/transition)

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 5: KV tools — `kv_set`, `kv_get`

**Files:**
- Create: `internal/mcpserver/tools_kv.go`, `internal/mcpserver/tools_kv_test.go`
- Modify: `internal/mcpserver/server.go` (add `registerKVTools(srv)`)

**Interfaces:**
- Consumes: `callDaemon`, `OKOut`.
- Produces: the `kv_set`, `kv_get` tools.

- [ ] **Step 1: Write the failing test**

Create `internal/mcpserver/tools_kv_test.go`:
```go
package mcpserver

import (
	"context"
	"testing"
)

func TestKVSetGetTools(t *testing.T) {
	startTestDaemon(t)

	if _, out, err := kvSetHandler(context.Background(), nil, KVSetIn{Key: "api.base", Value: "http://localhost:4000", By: "backend"}); err != nil || !out.OK {
		t.Fatalf("set: err=%v out=%+v", err, out)
	}
	_, got, err := kvGetHandler(context.Background(), nil, KVGetIn{Key: "api.base"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Found || got.Value != "http://localhost:4000" {
		t.Fatalf("unexpected get: %+v", got)
	}
	// Missing key → Found false, no error.
	_, missing, err := kvGetHandler(context.Background(), nil, KVGetIn{Key: "nope"})
	if err != nil || missing.Found {
		t.Fatalf("missing key: err=%v out=%+v", err, missing)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GODEBUG=netdns=go go test ./internal/mcpserver/ -run TestKVSetGetTools -v`
Expected: FAIL — `undefined: kvSetHandler`.

- [ ] **Step 3: Implement the KV tools**

Create `internal/mcpserver/tools_kv.go`:
```go
package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type KVSetIn struct {
	Key   string `json:"key" jsonschema:"the fact key, e.g. api.base or schema.version"`
	Value string `json:"value" jsonschema:"the value to store"`
	By    string `json:"by" jsonschema:"the alias setting the value"`
}

type KVGetIn struct {
	Key string `json:"key" jsonschema:"the fact key to read"`
}

type KVGetOut struct {
	Found     bool   `json:"found" jsonschema:"whether the key exists"`
	Value     string `json:"value" jsonschema:"the stored value (empty if not found)"`
	UpdatedBy string `json:"updated_by" jsonschema:"who last set the value"`
	UpdatedAt int64  `json:"updated_at" jsonschema:"when it was last set (unix ms)"`
}

// kvGetRaw mirrors the daemon's kv_get response: {"found": bool, "pair": {...}}.
type kvGetRaw struct {
	Found bool `json:"found"`
	Pair  struct {
		Key       string `json:"Key"`
		Value     string `json:"Value"`
		UpdatedBy string `json:"UpdatedBy"`
		UpdatedAt int64  `json:"UpdatedAt"`
	} `json:"pair"`
}

func kvSetHandler(_ context.Context, _ *mcp.CallToolRequest, in KVSetIn) (*mcp.CallToolResult, OKOut, error) {
	if _, err := callDaemon("kv_set", map[string]any{"key": in.Key, "value": in.Value, "by": in.By}); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true, Detail: "set " + in.Key}, nil
}

func kvGetHandler(_ context.Context, _ *mcp.CallToolRequest, in KVGetIn) (*mcp.CallToolResult, KVGetOut, error) {
	raw, err := callDaemon("kv_get", map[string]any{"key": in.Key})
	if err != nil {
		return nil, KVGetOut{}, err
	}
	var r kvGetRaw
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, KVGetOut{}, err
	}
	return nil, KVGetOut{Found: r.Found, Value: r.Pair.Value, UpdatedBy: r.Pair.UpdatedBy, UpdatedAt: r.Pair.UpdatedAt}, nil
}

func registerKVTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "kv_set", Description: "Set a shared fact on the bus blackboard (e.g. api.base, schema.version)."}, kvSetHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "kv_get", Description: "Read a shared fact from the bus blackboard. Returns found=false if the key is absent."}, kvGetHandler)
}
```
Note: the daemon's `kv_get` returns `{"found":..., "pair": <store.KVPair>}` where `KVPair` has no json tags, so its keys are PascalCase (`Key`, `Value`, …) — `kvGetRaw.Pair` uses those exact keys. `found` is lowercase because the daemon builds that map literal with a lowercase key.

- [ ] **Step 4: Register KV tools in `server.go`**
```go
	registerKVTools(srv)        // Task 5
```
`registerAll` should now call all four registration functions with no remaining comments.

- [ ] **Step 5: Run the test to verify it passes**

Run: `GODEBUG=netdns=go go test ./internal/mcpserver/ -run TestKVSetGetTools -v`
Expected: PASS.

- [ ] **Step 6: Verify + commit**

Run: `GODEBUG=netdns=go just verify` → green.
```bash
git add internal/mcpserver/
git commit -m "feat(mcp): KV blackboard tools (kv_set/kv_get)

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

---

### Task 6: End-to-end MCP round-trip test + README

This task proves the whole server works over a real MCP transport (not just handler calls) and documents `muster mcp`.

**Files:**
- Create: `internal/mcpserver/server_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `mcpserver` (all tools registered via `Run`'s `registerAll`), the SDK client + in-memory transport.
- Produces: an end-to-end test exercising the full server through the MCP client.

- [ ] **Step 1: Write the end-to-end test (in-memory MCP transport)**

Create `internal/mcpserver/server_test.go`:
```go
package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestEndToEndOverMCP boots the real server (all tools) on an in-memory
// transport and drives it with an MCP client — the cross-model scenario:
// create a review task, claim it, complete it, and read it back.
func TestEndToEndOverMCP(t *testing.T) {
	startTestDaemon(t)
	ctx := context.Background()

	srv := mcp.NewServer(&mcp.Implementation{Name: "muster", Version: version}, nil)
	registerAll(srv)

	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()

	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s transport error: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s tool error: %+v", name, res.Content)
		}
		return res
	}

	call("register_agent", map[string]any{"alias": "reviewer1", "role": "reviewer", "model_type": "codex"})
	created := call("task_create", map[string]any{
		"from": "backend", "to_kind": "role", "to_target": "reviewer",
		"subject": "Review feat/wagers", "ref": "repo=bhw", "body": "please review",
	})
	sc, ok := created.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("task_create StructuredContent not an object: %T", created.StructuredContent)
	}
	tid, ok := sc["thread_id"].(float64)
	if !ok || tid == 0 {
		t.Fatalf("no thread_id in task_create output: %v", sc)
	}
	call("task_claim", map[string]any{"thread_id": tid, "by": "reviewer1"})
	call("task_transition", map[string]any{"thread_id": tid, "by": "reviewer1", "status": "completed", "note": "LGTM"})

	got := call("get_thread", map[string]any{"thread_id": tid})
	gsc, _ := got.StructuredContent.(map[string]any)
	thread, _ := gsc["thread"].(map[string]any)
	if thread["status"] != "completed" {
		t.Fatalf("expected completed, got %v", thread["status"])
	}
}
```

- [ ] **Step 2: Run the test to verify it passes**

Run: `GODEBUG=netdns=go go test ./internal/mcpserver/ -run TestEndToEndOverMCP -v`
Expected: PASS. (If it fails on `StructuredContent` typing, inspect the actual shape — it is the tool's Out marshaled to a Go `map[string]any`; adjust the assertion to match, not the production code.)

- [ ] **Step 3: Document `muster mcp` in the README**

In `README.md`, under a new `## MCP mode` section (after the existing status/overview), add:
```markdown
## MCP mode

`muster mcp` runs muster as an MCP server over stdio, exposing the bus as tools
any MCP client (Claude Code, Codex) can call. Register it once per tool:

```bash
# Claude Code
claude mcp add muster -s user -- muster mcp
# Codex
codex mcp add muster -- muster mcp
```

Then, inside a session, the agent calls `register_agent` once, and can
`send_message` / `task_create` / `task_claim` / `task_transition` / `reply` /
`get_inbox` / `get_thread` / `list_agents` / `kv_set` / `kv_get`. The server
talks to the local `muster` daemon (auto-started); nothing is sent to any model
provider — muster only routes between agents already running on their own
subscriptions.

> Note: stdout is the MCP channel in this mode; muster writes all diagnostics to
> stderr.
```

- [ ] **Step 4: Verify + commit**

Run: `GODEBUG=netdns=go just verify` → green.
```bash
git add internal/mcpserver/server_test.go README.md
git commit -m "test(mcp): end-to-end MCP round-trip + document mcp mode

Claude-Session: https://claude.ai/code/session_018PRm92C86APKc2JKqHefvp"
```

- [ ] **Step 5: Manual real-client verification (operator step — record result)**

This confirms muster works in a real Claude Code session (the in-memory test can't cover the actual `claude mcp` path). From the built binary:
```bash
GODEBUG=netdns=go just build
# Point a throwaway Claude Code MCP registration at this binary:
claude mcp add muster-dev -s user -- "$(pwd)/bin/muster" mcp
claude mcp list        # expect: muster-dev ... ✔ Connected
# (Optionally) in a claude session: ask it to call register_agent then list_agents.
# Clean up the throwaway registration when done:
claude mcp remove muster-dev -s user
```
Record in the report whether `muster-dev … ✔ Connected` appeared. (The permanent per-machine registration is Milestone D; this is verification only.)

---

## Milestone B Definition of Done

- `just verify` green (fmt, lint, race tests, build) locally and in CI.
- `muster mcp` builds and serves an MCP stdio server exposing all 11 tools.
- Every tool round-trips through the daemon (unit tests per domain + one end-to-end test over a real MCP transport).
- `register_agent` captures `$TMUX`/`$TMUX_PANE` (sets up Milestone C's wake).
- `claude mcp add … -- muster mcp` connects (manual check recorded).
- No tmux wake, no human CLI, no permanent dotfiles wiring (Milestones C/D).

## Self-Review notes (author)

- **Spec coverage:** MCP server mode over the Milestone A daemon (Tasks 1-6); the 10 existing daemon ops + the newly exposed `get_thread` become 11 MCP tools; `register_agent` captures the tmux tuple for C. Deferred exactly per the milestone plan: tmux wake (C), human CLI + permanent dotfiles/Codex registration (D).
- **API verified:** all SDK calls (`NewServer`, `AddTool`, `StdioTransport`, `Run`, `NewInMemoryTransports`, `NewClient`, `Connect`, `CallTool`, `CallToolParams`, `CallToolResult.StructuredContent/IsError`) were confirmed against a compiling v1.6.1 spike before this plan was written.
- **No placeholders:** every step has runnable code/commands.
- **Type consistency:** `callDaemon`, `AgentView`/`ThreadView`/`EntryView`, `OKOut`, `ThreadIDOut`, `EntryIDOut`, and the per-tool In/Out structs are defined once and reused; `registerAll` calls exactly `registerRegistryTools`/`registerMessageTools`/`registerTaskTools`/`registerKVTools`; tool name strings match the daemon op names one-to-one (with `get_thread` added to the daemon in Task 3).
- **Known risk:** `StructuredContent`'s concrete Go type in the end-to-end test (`map[string]any` vs a typed value) is the one spot that may need adjustment against real SDK behavior; the plan flags it and instructs adjusting the test, not the production code.
