# `muster channel` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `muster channel` subcommand that pushes a compact envelope into a coding-agent session over the `claude/channel` MCP convention the moment mail lands on the bus for that session.

**Architecture:** Two new packages — `internal/channelmcp` (a stdlib newline-delimited JSON-RPC server ported from galley's `internal/mcp`, with `Notify`) and `internal/channel` (the carrier: identity via the daemon's `session_aliases` op, a poll loop over `list_events` follow mode, envelope formatting, a status tool) — wired by a `channel` case in `cmd/muster/main.go`. No daemon, store, or `muster mcp` changes. `internal/nudge` gains `pi` in its harness table.

**Tech Stack:** Go (stdlib only for the new code; no new deps), `just verify` gate, `internal/mustertest` + `daemon.Serve` for the integration test.

**Spec:** `docs/superpowers/specs/2026-08-23-muster-channel-design.md`

## Global Constraints

- stdout is sacred in channel mode — it is the MCP transport. Every diagnostic goes to stderr.
- The channel is a peer client of the daemon: it never imports `internal/store` for writes and never touches tmux options. Identity capture goes through `internal/tmuxenv.CaptureEnv()` only.
- No new `store.API` op, no daemon op, no change to `internal/mcpserver`, no new module dependency in `go.mod`.
- Knobs, not constants: poll interval reads `MUSTER_CHANNEL_INTERVAL` (default `1s`, floor `250ms`).
- Push `meta` values are strings; keys are identifiers (letters, digits, underscore).
- cgo-free; macOS tests use `mustertest.ShortHome()` for socket paths.
- `just verify` (gofmt, golangci-lint, `go test -race`, build, cross) before every commit. lefthook runs gofmt on commit.
- Branch: `feat/channel` worktree (already created at `.worktrees/feat/channel`, tracking `origin/dev`). Never touch `main` or `dev` directly.

---

### Task 1: `internal/channelmcp` — the stdlib channel server

**Files:**
- Create: `internal/channelmcp/server.go`
- Create: `internal/channelmcp/server_test.go`

**Interfaces:**
- Produces: `type Tool struct{Name, Description string; InputSchema json.RawMessage}`, `type Handler struct{Name, Version, Instructions string; Tools []Tool; Call func(name string, args json.RawMessage) (string, error)}`, `func New(h Handler) *Server`, `func (s *Server) Run(r io.Reader, w io.Writer) error`, `func (s *Server) Notify(content string, meta map[string]string) error`. Task 4 wires `Run` to stdin/stdout and hands `Notify` to the carrier.

- [ ] **Step 1: Write the failing tests**

```go
package channelmcp

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func drive(t *testing.T, h Handler) (io.WriteCloser, *bufio.Scanner) {
	t.Helper()
	clientOut, serverIn := io.Pipe()
	serverOut, clientIn := io.Pipe()
	s := New(h)
	go func() { _ = s.Run(clientOut, clientIn) }()
	t.Cleanup(func() { _ = serverIn.Close() })
	sc := bufio.NewScanner(serverOut)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	return serverIn, sc
}

func send(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := io.WriteString(w, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, sc *bufio.Scanner) map[string]any {
	t.Helper()
	done := make(chan bool, 1)
	go func() { done <- sc.Scan() }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("server closed its output")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no reply within 2s")
	}
	var m map[string]any
	if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
		t.Fatalf("unparseable reply %q: %v", sc.Text(), err)
	}
	return m
}

func TestInitializeDeclaresTheChannelCapability(t *testing.T) {
	w, sc := drive(t, Handler{Name: "muster-channel", Version: "0.0.1", Instructions: "hello"})
	send(t, w, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	m := read(t, sc)
	res, _ := m["result"].(map[string]any)
	if res == nil {
		t.Fatalf("no result in %v", m)
	}
	if res["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion not echoed: %v", res["protocolVersion"])
	}
	caps, _ := res["capabilities"].(map[string]any)
	exp, _ := caps["experimental"].(map[string]any)
	if _, ok := exp["claude/channel"]; !ok {
		t.Errorf("claude/channel capability missing: %v", caps)
	}
	if res["instructions"] != "hello" {
		t.Errorf("instructions not carried: %v", res["instructions"])
	}
}

func TestToolsListAndCall(t *testing.T) {
	called := ""
	h := Handler{
		Name: "muster-channel", Version: "0.0.1",
		Tools: []Tool{{Name: "muster_channel_status", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Call: func(name string, args json.RawMessage) (string, error) {
			called = name
			return "attached", nil
		},
	}
	w, sc := drive(t, h)
	send(t, w, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	m := read(t, sc)
	if !strings.Contains(sc.Text(), "muster_channel_status") {
		t.Errorf("tools/list did not name the tool: %v", m)
	}
	send(t, w, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"muster_channel_status","arguments":{}}}`)
	m = read(t, sc)
	if called != "muster_channel_status" {
		t.Error("Call was not dispatched")
	}
	if !strings.Contains(sc.Text(), "attached") {
		t.Errorf("tool result not returned: %v", m)
	}
}

func TestToolErrorBecomesIsErrorResult(t *testing.T) {
	h := Handler{Name: "m", Version: "1", Call: func(string, json.RawMessage) (string, error) {
		return "", io.ErrUnexpectedEOF
	}}
	w, sc := drive(t, h)
	send(t, w, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"x","arguments":{}}}`)
	m := read(t, sc)
	res, _ := m["result"].(map[string]any)
	if res == nil || res["isError"] != true {
		t.Fatalf("tool error must be an isError result, not a protocol error: %v", m)
	}
}

func TestNotificationsIgnoredUnknownMethodsRefused(t *testing.T) {
	w, sc := drive(t, Handler{Name: "m", Version: "1"})
	send(t, w, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(t, w, `{"jsonrpc":"2.0","id":7,"method":"resources/read"}`)
	m := read(t, sc)
	if id, _ := m["id"].(float64); id != 7 {
		t.Fatalf("first reply answers the notification, not the request: %v", m)
	}
	if m["error"] == nil {
		t.Errorf("unknown method got a result: %v", m)
	}
}

func TestNotifyEmitsChannelNotificationAndValidatesMetaKeys(t *testing.T) {
	clientOut, serverIn := io.Pipe()
	serverOut, clientIn := io.Pipe()
	s := New(Handler{Name: "m", Version: "1"})
	go func() { _ = s.Run(clientOut, clientIn) }()
	defer func() { _ = serverIn.Close() }()
	sc := bufio.NewScanner(serverOut)

	if err := s.Notify("hello", map[string]string{"bad-key": "x"}); err == nil {
		t.Fatal("a hyphenated meta key must be rejected — the client drops it silently")
	}
	if err := s.Notify("hello", map[string]string{"thread_id": "42", "intent": "fyi"}); err != nil {
		t.Fatal(err)
	}
	if !sc.Scan() {
		t.Fatal("nothing emitted")
	}
	var m map[string]any
	if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["method"] != "notifications/claude/channel" {
		t.Errorf("wrong method: %v", m["method"])
	}
	params, _ := m["params"].(map[string]any)
	if params["content"] != "hello" {
		t.Errorf("content lost: %v", params)
	}
}

// Notifications from goroutines must never interleave bytes with request
// answers: every line the client reads must parse on its own.
func TestConcurrentNotifyNeverInterleaves(t *testing.T) {
	clientOut, serverIn := io.Pipe()
	serverOut, clientIn := io.Pipe()
	s := New(Handler{Name: "m", Version: "1"})
	go func() { _ = s.Run(clientOut, clientIn) }()
	defer func() { _ = serverIn.Close() }()
	sc := bufio.NewScanner(serverOut)
	sc.Buffer(make([]byte, 1<<20), 1<<24)

	const n = 50
	go func() {
		for i := 0; i < n; i++ {
			_ = s.Notify(strings.Repeat("x", 4000), map[string]string{"i": "1"})
		}
	}()
	go func() {
		for i := 0; i < n; i++ {
			_, _ = io.WriteString(serverIn, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n")
		}
	}()
	for i := 0; i < 2*n; i++ {
		if !sc.Scan() {
			t.Fatalf("output closed after %d lines", i)
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %d not standalone JSON: %v", i, err)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd .worktrees/feat/channel && go test ./internal/channelmcp/ 2>&1 | head -5`
Expected: build failure — `undefined: Handler`, `undefined: New`.

- [ ] **Step 3: Write the server**

```go
// Package channelmcp is a minimal MCP server speaking newline-delimited
// JSON-RPC 2.0 over a reader/writer pair — enough to be a claude/channel
// carrier and expose a handful of tools, and nothing more.
//
// DELIBERATELY NOT THE SDK. internal/mcpserver uses the official Go SDK for
// muster's tools, but that SDK (v1.6.1) exposes no way to emit an arbitrary
// notification method, and notifications/claude/channel is the whole point
// here. galley proved this framing, this handshake, and the experimental
// capability are accepted by Claude Code from a dependency-free Go binary.
//
// FRAMING: one JSON object per line, both directions. Not LSP Content-Length.
package channelmcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Tool describes one entry in tools/list. InputSchema is raw JSON Schema,
// passed through verbatim.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Handler is everything the caller configures. Call dispatches tools/call by
// name and returns the text content of the result; an error becomes an
// isError tool result rather than a protocol error, because the model should
// read the failure and adapt rather than the transport breaking.
type Handler struct {
	Name         string
	Version      string
	Instructions string
	Tools        []Tool
	Call         func(name string, args json.RawMessage) (string, error)
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Server owns the write side. All writes go through send, under one mutex,
// because Notify is called from the carrier goroutine while Run's loop is
// answering requests, and interleaved bytes are a parse error on the client
// with no useful message.
type Server struct {
	h     Handler
	mu    sync.Mutex
	w     *bufio.Writer
	ready chan struct{} // closed by Run once the writer is attached
	out   chan rpcMsg   // notifications, drained by the emitter goroutine
}

// New starts the notification emitter alongside the Server. Notify cannot
// write in the caller's goroutine: a stdio peer reads at its own pace and a
// synchronous write would park the carrier on the client's schedule.
// Draining through one channel also keeps notifications in the order made.
func New(h Handler) *Server {
	s := &Server{h: h, ready: make(chan struct{}), out: make(chan rpcMsg, 16)}
	go func() {
		for m := range s.out {
			// A write error here has no caller to return to; the transport
			// being down surfaces on Run's next answer instead.
			_ = s.send(m)
		}
	}()
	return s
}

// send is the only writer, and every path to it is ordered after Run attaches
// s.w: answer runs inside Run's loop, and the emitter only receives what
// Notify queued after the ready latch closed.
func (s *Server) send(m rpcMsg) error {
	m.JSONRPC = "2.0"
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := json.NewEncoder(s.w).Encode(m); err != nil {
		return err
	}
	return s.w.Flush()
}

// Notify emits notifications/claude/channel. Meta keys must be identifiers
// (letters, digits, underscore) — the client silently DROPS anything else, so
// a bad key is refused here where the caller can see it. Notify waits for Run
// to attach the writer; a nil error means queued in order, not read.
func (s *Server) Notify(content string, meta map[string]string) error {
	for k := range meta {
		for _, r := range k {
			ok := r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
			if !ok {
				return fmt.Errorf("meta key %q is not an identifier — the client would drop it silently", k)
			}
		}
	}
	params, err := json.Marshal(map[string]any{"content": content, "meta": meta})
	if err != nil {
		return err
	}
	<-s.ready
	s.out <- rpcMsg{Method: "notifications/claude/channel", Params: params}
	return nil
}

// Run reads requests until EOF. Closing the ready latch here, after the
// writer is attached and under the same lock, is what lets Notify be called
// from the first instant of the process: it parks until this line has run.
func (s *Server) Run(r io.Reader, w io.Writer) error {
	s.mu.Lock()
	s.w = bufio.NewWriter(w)
	select {
	case <-s.ready:
	default:
		close(s.ready)
	}
	s.mu.Unlock()

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // unparseable input is the client's bug; do not die for it
		}
		if len(msg.ID) == 0 {
			continue // a notification is never answered
		}
		if err := s.answer(msg); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) answer(msg rpcMsg) error {
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return s.send(rpcMsg{ID: msg.ID, Result: map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities": map[string]any{
				"experimental": map[string]any{"claude/channel": map[string]any{}},
				"tools":        map[string]any{},
			},
			"serverInfo":   map[string]any{"name": s.h.Name, "version": s.h.Version},
			"instructions": s.h.Instructions,
		}})
	case "ping":
		return s.send(rpcMsg{ID: msg.ID, Result: map[string]any{}})
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.h.Tools))
		for _, t := range s.h.Tools {
			tools = append(tools, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		return s.send(rpcMsg{ID: msg.ID, Result: map[string]any{"tools": tools}})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if s.h.Call == nil {
			return s.send(rpcMsg{ID: msg.ID, Error: &rpcError{Code: -32601, Message: "no tools"}})
		}
		text, err := s.h.Call(p.Name, p.Arguments)
		if err != nil {
			return s.send(rpcMsg{ID: msg.ID, Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			}})
		}
		return s.send(rpcMsg{ID: msg.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		}})
	default:
		return s.send(rpcMsg{ID: msg.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + msg.Method}})
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `cd .worktrees/feat/channel && go test -race ./internal/channelmcp/ -v 2>&1 | tail -12`
Expected: 6 tests PASS.

- [ ] **Step 5: Commit**

```bash
cd .worktrees/feat/channel && git add internal/channelmcp && git commit -m "feat(channel): stdlib claude/channel MCP server"
```

### Task 2: `internal/channel` — envelope formatting

**Files:**
- Create: `internal/channel/envelope.go`
- Create: `internal/channel/envelope_test.go`

**Interfaces:**
- Produces: `type Event struct{ID int64; Kind, Agent, Target string; ThreadID int64; Detail, Subject, Intent string}` (JSON tags `id`, `kind`, `agent`, `target`, `thread_id`, `detail`, `subject`, `intent` — identical to `store.Event`'s wire shape), `func Format(events []Event) (content string, meta map[string]string)`. Task 3 decodes `list_events` rows into `Event` and pushes `Format`'s output.

- [ ] **Step 1: Write the failing tests**

```go
package channel

import (
	"strings"
	"testing"
)

func TestFormatSingleActionRequested(t *testing.T) {
	content, meta := Format([]Event{{ID: 9, Kind: "send", Agent: "reviewer", ThreadID: 42, Subject: "review the channel branch", Intent: "action-requested"}})
	want := `muster: action-requested from reviewer on thread #42 "review the channel branch" — call get_thread 42, act, then reply.`
	if content != want {
		t.Errorf("content:\n got %q\nwant %q", content, want)
	}
	for k, v := range map[string]string{"kind": "send", "from": "reviewer", "thread_id": "42", "intent": "action-requested", "count": "1"} {
		if meta[k] != v {
			t.Errorf("meta[%s] = %q, want %q", k, meta[k], v)
		}
	}
}

func TestFormatSingleFyiNeedsNoReply(t *testing.T) {
	content, _ := Format([]Event{{Kind: "send", Agent: "ops", ThreadID: 41, Subject: "deploy done", Intent: "fyi"}})
	if !strings.Contains(content, "fyi from ops on thread #41") || !strings.Contains(content, "no reply needed") {
		t.Errorf("fyi push must say no reply is needed: %q", content)
	}
}

func TestFormatReplyLabelsAsReply(t *testing.T) {
	content, meta := Format([]Event{{Kind: "reply", Agent: "lead", ThreadID: 40, Subject: "plan", Intent: "reply-requested"}})
	if !strings.HasPrefix(content, `muster: reply from lead on thread #40 "plan"`) {
		t.Errorf("reply events are labeled reply, not by thread intent: %q", content)
	}
	if meta["kind"] != "reply" {
		t.Errorf("meta kind = %q", meta["kind"])
	}
}

func TestFormatCoalescesABurst(t *testing.T) {
	content, meta := Format([]Event{
		{Kind: "send", Agent: "reviewer", ThreadID: 42, Subject: "review", Intent: "action-requested"},
		{Kind: "reply", Agent: "lead", ThreadID: 40, Subject: "plan", Intent: "reply-requested"},
		{Kind: "task", Agent: "ops", ThreadID: 41, Subject: "rotate keys", Intent: "action-requested"},
	})
	want := `muster: 3 new — action-requested from reviewer on #42 "review"; reply from lead on #40 "plan"; action-requested from ops on #41 "rotate keys" — call get_inbox.`
	if content != want {
		t.Errorf("content:\n got %q\nwant %q", content, want)
	}
	if meta["count"] != "3" || meta["thread_id"] != "42" || meta["from"] != "reviewer" {
		t.Errorf("meta describes the first event and the total: %v", meta)
	}
}

func TestFormatFallsBackToKindAndDetail(t *testing.T) {
	content, _ := Format([]Event{{Kind: "task", Agent: "ops", ThreadID: 7, Detail: "from detail"}})
	if !strings.Contains(content, `task from ops on thread #7 "from detail"`) {
		t.Errorf("empty intent → kind label, empty subject → detail: %q", content)
	}
}

func TestFormatEmptyIsEmpty(t *testing.T) {
	content, meta := Format(nil)
	if content != "" || meta != nil {
		t.Errorf("nothing to say: %q %v", content, meta)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd .worktrees/feat/channel && go test ./internal/channel/ 2>&1 | head -3`
Expected: `undefined: Format` / `undefined: Event`.

- [ ] **Step 3: Implement**

```go
// Package channel is the muster channel carrier: it tails the bus journal on
// behalf of one session's aliases and pushes compact envelopes into that
// session over a claude/channel MCP server. It is a peer client of the daemon
// — it never reads the store or touches tmux options itself.
package channel

import (
	"fmt"
	"strconv"
	"strings"
)

// Event is one journal row as list_events returns it. Tags mirror
// store.Event's wire shape so a daemon response decodes straight into it
// without importing the store.
type Event struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	Agent    string `json:"agent"`
	Target   string `json:"target"`
	ThreadID int64  `json:"thread_id"`
	Detail   string `json:"detail"`
	Subject  string `json:"subject"`
	Intent   string `json:"intent"`
}

// label is the one word an agent reads first: a reply is a reply whatever
// the thread's intent; anything else is the thread's effective intent, or
// the bare event kind when the thread carries none.
func label(e Event) string {
	if e.Kind == "reply" {
		return "reply"
	}
	if e.Intent != "" {
		return e.Intent
	}
	return e.Kind
}

func subject(e Event) string {
	if e.Subject != "" {
		return e.Subject
	}
	return e.Detail
}

// Format renders one push for everything one poll tick found. The body never
// travels: the content line tells the agent what arrived and which tool to
// call; meta carries the same facts as strings for the harness. With several
// events, meta describes the first and count carries the total.
func Format(events []Event) (string, map[string]string) {
	if len(events) == 0 {
		return "", nil
	}
	first := events[0]
	meta := map[string]string{
		"kind":      first.Kind,
		"from":      first.Agent,
		"thread_id": strconv.FormatInt(first.ThreadID, 10),
		"intent":    first.Intent,
		"count":     strconv.Itoa(len(events)),
	}
	if len(events) == 1 {
		tail := fmt.Sprintf("call get_thread %d, act, then reply.", first.ThreadID)
		if label(first) == "fyi" {
			tail = fmt.Sprintf("read it with get_thread %d; no reply needed.", first.ThreadID)
		}
		return fmt.Sprintf("muster: %s from %s on thread #%d %q — %s", label(first), first.Agent, first.ThreadID, subject(first), tail), meta
	}
	items := make([]string, 0, len(events))
	for _, e := range events {
		items = append(items, fmt.Sprintf("%s from %s on #%d %q", label(e), e.Agent, e.ThreadID, subject(e)))
	}
	return fmt.Sprintf("muster: %d new — %s — call get_inbox.", len(events), strings.Join(items, "; ")), meta
}
```

- [ ] **Step 4: Run tests**

Run: `cd .worktrees/feat/channel && go test -race ./internal/channel/ -v 2>&1 | tail -8`
Expected: 6 tests PASS.

- [ ] **Step 5: Commit**

```bash
cd .worktrees/feat/channel && git add internal/channel && git commit -m "feat(channel): envelope formatting"
```

### Task 3: `internal/channel` — the carrier (identity, poll loop, status)

**Files:**
- Create: `internal/channel/carrier.go`
- Create: `internal/channel/carrier_test.go`

**Interfaces:**
- Consumes: `Event`, `Format` from Task 2.
- Produces: `type Client func(op string, args map[string]any) (json.RawMessage, error)`, `func DaemonClient(socketPath string) Client`, `type Identity struct{SocketPath, SessionID, PaneID string; SessionCreated int64}`, `type Carrier struct{Call Client; Notify func(string, map[string]string) error; Ident Identity; Interval time.Duration; Sleep func(time.Duration); Errw io.Writer; …}`, `func (c *Carrier) Start() error`, `func (c *Carrier) Tick() error`, `func (c *Carrier) Run(ctx context.Context)`, `func (c *Carrier) Status() string`, `const DefaultInterval = time.Second`, `const MinInterval = 250 * time.Millisecond`. Task 4 constructs a `Carrier` from `tmuxenv.CaptureEnv()` and the server's `Notify`; Task 5 drives it against a real daemon.

- [ ] **Step 1: Write the failing tests**

```go
package channel

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeDaemon answers the three ops the carrier uses, from in-memory state.
type fakeDaemon struct {
	aliases []string
	unread  int
	events  []Event // the whole journal, ascending ids
	fail    map[string]error
}

func (f *fakeDaemon) call(op string, args map[string]any) (json.RawMessage, error) {
	if err := f.fail[op]; err != nil {
		return nil, err
	}
	switch op {
	case "session_aliases":
		return json.Marshal(map[string]any{"aliases": f.aliases})
	case "session_unread":
		return json.Marshal(map[string]any{"total": f.unread, "action": 0})
	case "list_events":
		maxID := int64(0)
		for _, e := range f.events {
			if e.ID > maxID {
				maxID = e.ID
			}
		}
		if b, _ := args["backlog"].(bool); b {
			return json.Marshal(map[string]any{"events": []Event{}, "max_id": maxID})
		}
		after, _ := args["after_id"].(int64)
		agent, _ := args["agent"].(string)
		var out []Event
		for _, e := range f.events {
			if e.ID > after && (e.Agent == agent || e.Target == "agent:"+agent) {
				out = append(out, e)
			}
		}
		return json.Marshal(map[string]any{"events": out, "max_id": maxID})
	}
	return nil, fmt.Errorf("unexpected op %s", op)
}

type pushes struct{ got []string }

func (p *pushes) notify(content string, _ map[string]string) error {
	p.got = append(p.got, content)
	return nil
}

func newCarrier(f *fakeDaemon, p *pushes) *Carrier {
	return &Carrier{
		Call: f.call, Notify: p.notify,
		Ident: Identity{SocketPath: "/tmp/s", SessionID: "$1", PaneID: "%1", SessionCreated: 100},
		Errw:  &strings.Builder{},
	}
}

func TestStartSetsCursorAtMaxAndSummarizesUnread(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker"}, unread: 2, events: []Event{{ID: 5, Kind: "send", Agent: "lead", Target: "agent:worker", ThreadID: 1}}}
	p := &pushes{}
	c := newCarrier(f, p)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 1 || !strings.Contains(p.got[0], "2 unread") || !strings.Contains(p.got[0], "get_inbox") {
		t.Fatalf("startup summary: %v", p.got)
	}
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 1 {
		t.Fatalf("events before the start cursor must never be replayed: %v", p.got)
	}
}

func TestTickPushesNewMailAndAdvancesCursor(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker"}}
	p := &pushes{}
	c := newCarrier(f, p)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	f.events = append(f.events, Event{ID: 1, Kind: "send", Agent: "lead", Target: "agent:worker", ThreadID: 7, Subject: "hi", Intent: "action-requested"})
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 1 || !strings.Contains(p.got[0], "thread #7") {
		t.Fatalf("first tick: %v", p.got)
	}
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 1 {
		t.Fatalf("a quiet tick must push nothing: %v", p.got)
	}
}

func TestTickIgnoresSelfAuthoredAndNonMailKinds(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker"}}
	p := &pushes{}
	c := newCarrier(f, p)
	_ = c.Start()
	f.events = []Event{
		{ID: 1, Kind: "send", Agent: "worker", Target: "agent:lead", ThreadID: 1},   // I sent it
		{ID: 2, Kind: "read", Agent: "worker", Target: "", ThreadID: 1},             // not mail
		{ID: 3, Kind: "notify", Agent: "", Target: "agent:worker", ThreadID: 1},     // wake-layer noise
		{ID: 4, Kind: "reply", Agent: "lead", Target: "", ThreadID: 1, Subject: "s"}, // mail
	}
	_ = c.Tick()
	if len(p.got) != 1 || !strings.HasPrefix(p.got[0], "muster: reply from lead") {
		t.Fatalf("only the reply is mail for me: %v", p.got)
	}
}

func TestTickCoalescesAcrossAliasesWithoutDuplicates(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker", "backend"}}
	p := &pushes{}
	c := newCarrier(f, p)
	_ = c.Start()
	f.events = []Event{
		{ID: 1, Kind: "send", Agent: "lead", Target: "agent:worker", ThreadID: 1, Subject: "a"},
		{ID: 2, Kind: "send", Agent: "lead", Target: "agent:backend", ThreadID: 2, Subject: "b"},
	}
	_ = c.Tick()
	if len(p.got) != 1 || !strings.HasPrefix(p.got[0], "muster: 2 new") {
		t.Fatalf("one push per tick across aliases: %v", p.got)
	}
}

func TestNoRegistrationIdlesWithoutError(t *testing.T) {
	f := &fakeDaemon{}
	p := &pushes{}
	c := newCarrier(f, p)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	f.events = []Event{{ID: 1, Kind: "send", Agent: "lead", Target: "agent:worker", ThreadID: 1}}
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 0 {
		t.Fatalf("unregistered session must push nothing: %v", p.got)
	}
	if !strings.Contains(c.Status(), "not registered") {
		t.Errorf("status must say why it is idle: %q", c.Status())
	}
	f.aliases = []string{"worker"}
	_ = c.Tick()
	if len(p.got) != 1 {
		t.Fatalf("registration picked up on a later tick: %v", p.got)
	}
}

func TestNoPaneIdles(t *testing.T) {
	c := &Carrier{Call: (&fakeDaemon{}).call, Notify: (&pushes{}).notify, Errw: &strings.Builder{}}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Status(), "no tmux pane") {
		t.Errorf("status: %q", c.Status())
	}
}

func TestDaemonErrorIsReportedNotFatal(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker"}, fail: map[string]error{"list_events": fmt.Errorf("socket gone")}}
	p := &pushes{}
	c := newCarrier(f, p)
	if err := c.Start(); err == nil {
		t.Fatal("Start must surface a daemon failure")
	}
	f.fail = nil
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	f.fail = map[string]error{"list_events": fmt.Errorf("socket gone")}
	if err := c.Tick(); err == nil {
		t.Fatal("Tick must surface a daemon failure")
	}
	if !strings.Contains(c.Status(), "socket gone") {
		t.Errorf("status carries the last error: %q", c.Status())
	}
}

func TestRunPollsUntilContextEnds(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker"}}
	p := &pushes{}
	c := newCarrier(f, p)
	c.Interval = MinInterval
	ctx, cancel := context.WithCancel(context.Background())
	ticks := 0
	c.Sleep = func(time.Duration) {
		ticks++
		if ticks == 4 {
			cancel()
		}
	}
	c.Run(ctx)
	if ticks != 4 {
		t.Fatalf("expected 4 sleeps before cancellation stopped the loop, got %d", ticks)
	}
}

func TestStatusReportsAliasesAndCursor(t *testing.T) {
	f := &fakeDaemon{aliases: []string{"worker"}, events: []Event{{ID: 12, Kind: "send", Agent: "x", Target: "agent:y"}}}
	c := newCarrier(f, &pushes{})
	_ = c.Start()
	s := c.Status()
	for _, want := range []string{"worker", "%1", "cursor 12"} {
		if !strings.Contains(s, want) {
			t.Errorf("status %q lacks %q", s, want)
		}
	}
}
```

The test file's import block needs `"context"` alongside the imports shown above.

- [ ] **Step 2: Run to verify failure**

Run: `cd .worktrees/feat/channel && go test ./internal/channel/ 2>&1 | head -3`
Expected: `undefined: Carrier`.

- [ ] **Step 3: Implement the carrier**

```go
package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/proto"
)

// DefaultInterval is the poll cadence when MUSTER_CHANNEL_INTERVAL is unset;
// MinInterval is the floor a knob cannot go under (a tighter loop would just
// hammer the daemon socket for no visible gain).
const (
	DefaultInterval = time.Second
	MinInterval     = 250 * time.Millisecond
)

// mailKinds are the journal kinds that mean "something arrived for you".
// notify/read/nudge/claim/transition/register/become are wake-layer or
// lifecycle noise and never wake a session.
var mailKinds = map[string]bool{"send": true, "task": true, "reply": true}

// Client sends one op to the daemon and returns its Data as JSON, or an error
// if the transport failed or the daemon reported !OK. Injectable so tests run
// against an in-memory fake.
type Client func(op string, args map[string]any) (json.RawMessage, error)

// DaemonClient is the production Client over the unix socket (lazily
// starting the daemon exactly as every other peer client does).
func DaemonClient(socketPath string) Client {
	return func(op string, args map[string]any) (json.RawMessage, error) {
		resp, err := client.Call(socketPath, proto.Request{Op: op, Args: args})
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
}

// Identity is the session tuple the carrier pushes for — the same tuple
// register_agent stores, captured through tmuxenv by the caller.
type Identity struct {
	SocketPath     string
	SessionID      string
	PaneID         string
	SessionCreated int64
}

func (id Identity) paneless() bool {
	return id.SocketPath == "" || id.SessionID == "" || id.PaneID == ""
}

// Carrier tails the journal for one session's aliases and pushes envelopes.
// Zero-value seams: Interval → DefaultInterval, Sleep → time.Sleep, Errw →
// os.Stderr.
type Carrier struct {
	Call     Client
	Notify   func(content string, meta map[string]string) error
	Ident    Identity
	Interval time.Duration
	Sleep    func(time.Duration)
	Errw     io.Writer

	mu       sync.Mutex
	aliases  []string
	cursor   int64
	lastPush time.Time
	lastErr  string
	started  bool
}

func (c *Carrier) errw() io.Writer {
	if c.Errw == nil {
		return os.Stderr
	}
	return c.Errw
}

// resolve asks the daemon which live aliases this session holds. An empty
// list is not an error — the SessionStart hook may not have registered yet.
func (c *Carrier) resolve() ([]string, error) {
	raw, err := c.Call("session_aliases", map[string]any{
		"socket_path": c.Ident.SocketPath, "session_id": c.Ident.SessionID, "session_created": c.Ident.SessionCreated,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Aliases []string `json:"aliases"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode session_aliases: %w", err)
	}
	return out.Aliases, nil
}

func (c *Carrier) maxEventID() (int64, error) {
	raw, err := c.Call("list_events", map[string]any{"backlog": true, "limit": 1})
	if err != nil {
		return 0, err
	}
	var out struct {
		MaxID int64 `json:"max_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("decode list_events: %w", err)
	}
	return out.MaxID, nil
}

// Start places the cursor at the journal's current head — nothing before the
// channel existed is replayed — and emits one summary if mail is already
// waiting. A paneless session idles: no error, nothing pushed, Status says why.
func (c *Carrier) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = true
	if c.Ident.paneless() {
		c.lastErr = "no tmux pane (channel started outside tmux); idle"
		fmt.Fprintln(c.errw(), "[muster channel]", c.lastErr)
		return nil
	}
	head, err := c.maxEventID()
	if err != nil {
		c.lastErr = err.Error()
		return err
	}
	c.cursor = head
	c.lastErr = ""
	raw, err := c.Call("session_unread", map[string]any{
		"socket_path": c.Ident.SocketPath, "session_id": c.Ident.SessionID, "session_created": c.Ident.SessionCreated,
	})
	if err != nil {
		c.lastErr = err.Error()
		return err
	}
	var unread struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(raw, &unread); err != nil {
		return fmt.Errorf("decode session_unread: %w", err)
	}
	if unread.Total > 0 {
		c.push(fmt.Sprintf("muster: %d unread message(s) waiting — call get_inbox.", unread.Total),
			map[string]string{"kind": "summary", "count": strconv.Itoa(unread.Total)})
	}
	return nil
}

// Tick is one poll: re-resolve aliases, read everything past the cursor that
// concerns them, drop self-authored and non-mail rows, push one envelope.
func (c *Carrier) Tick() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Ident.paneless() {
		return nil
	}
	aliases, err := c.resolve()
	if err != nil {
		c.lastErr = err.Error()
		return err
	}
	c.aliases = aliases
	if len(aliases) == 0 {
		c.lastErr = "session not registered on the bus yet (waiting for register_agent)"
		return nil
	}
	mine := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		mine[a] = true
	}
	seen := map[int64]bool{}
	var batch []Event
	head := c.cursor
	for _, a := range aliases {
		raw, err := c.Call("list_events", map[string]any{"agent": a, "after_id": c.cursor})
		if err != nil {
			c.lastErr = err.Error()
			return err
		}
		var out struct {
			Events []Event `json:"events"`
			MaxID  int64   `json:"max_id"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			c.lastErr = err.Error()
			return fmt.Errorf("decode list_events: %w", err)
		}
		if out.MaxID > head {
			head = out.MaxID
		}
		for _, e := range out.Events {
			if seen[e.ID] || !mailKinds[e.Kind] || mine[e.Agent] {
				continue
			}
			seen[e.ID] = true
			batch = append(batch, e)
		}
	}
	c.cursor = head
	c.lastErr = ""
	if len(batch) > 0 {
		content, meta := Format(batch)
		c.push(content, meta)
	}
	return nil
}

// push hands one envelope to the channel server. Must hold c.mu.
func (c *Carrier) push(content string, meta map[string]string) {
	if err := c.Notify(content, meta); err != nil {
		c.lastErr = "push failed: " + err.Error()
		fmt.Fprintln(c.errw(), "[muster channel]", c.lastErr)
		return
	}
	c.lastPush = time.Now()
}

// Run is Start followed by Tick every Interval until ctx ends. Errors are
// logged and retried on the next tick — a daemon restart must not kill the
// channel, and the process lives exactly as long as the session does.
func (c *Carrier) Run(ctx context.Context) {
	interval := c.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	if interval < MinInterval {
		interval = MinInterval
	}
	sleep := c.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	if err := c.Start(); err != nil {
		fmt.Fprintln(c.errw(), "[muster channel] start:", err)
	}
	for ctx.Err() == nil {
		if err := c.Tick(); err != nil {
			fmt.Fprintln(c.errw(), "[muster channel] tick:", err)
		}
		sleep(interval)
	}
}

// Status is the muster_channel_status tool's answer: who this channel pushes
// for, where the cursor sits, when it last pushed, and why it is idle if it is.
func (c *Carrier) Status() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	if c.Ident.paneless() {
		b.WriteString("idle: no tmux pane — the channel only serves sessions running inside tmux.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "pane %s on session %s (socket %s)\n", c.Ident.PaneID, c.Ident.SessionID, c.Ident.SocketPath)
	if len(c.aliases) == 0 {
		b.WriteString("aliases: none — session not registered on the bus yet; call register_agent (or wait for the SessionStart hook)\n")
	} else {
		fmt.Fprintf(&b, "aliases: %s\n", strings.Join(c.aliases, ", "))
	}
	fmt.Fprintf(&b, "journal cursor %d\n", c.cursor)
	if c.lastPush.IsZero() {
		b.WriteString("last push: never\n")
	} else {
		fmt.Fprintf(&b, "last push: %s ago\n", time.Since(c.lastPush).Round(time.Second))
	}
	if c.lastErr != "" {
		fmt.Fprintf(&b, "last problem: %s\n", c.lastErr)
	}
	return b.String()
}
```

- [ ] **Step 4: Run tests**

Run: `cd .worktrees/feat/channel && go test -race ./internal/channel/ -v 2>&1 | tail -14`
Expected: all Task 2 + Task 3 tests PASS (15 total).

- [ ] **Step 5: Commit**

```bash
cd .worktrees/feat/channel && git add internal/channel && git commit -m "feat(channel): carrier — identity, journal tail, status"
```

### Task 4: `muster channel` subcommand, registry entry, interval knob

**Files:**
- Modify: `cmd/muster/main.go` (switch at the `case "mcp":` block; constants after `PollIntervalEnv`)
- Create: `cmd/muster/channel.go`
- Modify: `internal/humancli/registry.go` (after the `mcp` entry, ~line 365)
- Modify: `internal/humancli/registry_test.go:15-28`

**Interfaces:**
- Consumes: `channelmcp.New/Run/Notify`, `channel.Carrier/DaemonClient/Identity/DefaultInterval/MinInterval`, `tmuxenv.CaptureEnv()` (fields `SocketPath, SessionID, PaneID, SessionCreated`), `paths.SocketPath()`.
- Produces: `muster channel` runnable; `muster channel --help` served by the registry.

- [ ] **Step 1: Registry test first — add `channel` to both lists**

In `internal/humancli/registry_test.go`, change the `wantCommandNames` line `"serve", "mcp", "lambda", "hook", "debug",` to `"serve", "mcp", "channel", "lambda", "hook", "debug",` and `mainOwnedCommands` to `map[string]bool{"serve": true, "mcp": true, "channel": true, "lambda": true, "debug": true}`.

Run: `cd .worktrees/feat/channel && go test ./internal/humancli/ -run TestRegistry 2>&1 | tail -4`
Expected: FAIL — registry has one fewer command than wanted.

- [ ] **Step 2: Registry entry**

Insert after the `mcp` Command literal in `internal/humancli/registry.go`:

```go
		{
			Name:     "channel",
			Synopsis: "channel",
			Summary:  "Run the MCP channel carrier that pushes new mail into this session.",
			Help: `A claude/channel MCP server over stdio for the session that launched it.
Register it once per harness beside 'muster mcp':

    claude mcp add muster-channel -s user -- muster channel

It tails the bus journal for this tmux session's aliases and pushes a compact
envelope (who, intent, thread, subject) into the session the moment mail
lands — waking an idle agent without anyone typing. Bodies stay in the bus:
the agent answers with get_thread / get_inbox / reply as before. The 📬 badge,
the Stop-hook drain and 'muster nudge' keep working unchanged as fallbacks.

MUSTER_CHANNEL_INTERVAL tunes the poll cadence (Go duration, default 1s,
floor 250ms). stdout is the MCP protocol — diagnostics go to stderr. One tool,
muster_channel_status, reports what the channel is attached to.`,
			Group: GroupPlumbing,
		},
```

Run: `cd .worktrees/feat/channel && go test ./internal/humancli/ -run TestRegistry 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 3: main.go routing and knob**

In the `main()` switch, after the `case "mcp":` block add:

```go
	case "channel":
		if wantsHelp(os.Args[2:]) {
			_ = humancli.HelpFor("channel", os.Stdout)
			return
		}
		runChannel()
```

After the `pollInterval` function add:

```go
// ChannelIntervalEnv tunes how often `muster channel` polls the journal for
// new mail addressed to its session (a Go duration, e.g. "500ms"). Bounds
// only how late a push can be; the floor is channel.MinInterval. Unparseable
// or non-positive values fall back to the default with a warning.
const ChannelIntervalEnv = "MUSTER_CHANNEL_INTERVAL"

// channelInterval reads ChannelIntervalEnv, defaulting to channel.DefaultInterval.
func channelInterval() time.Duration {
	raw := os.Getenv(ChannelIntervalEnv)
	if raw == "" {
		return channel.DefaultInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "muster: %s=%q is not a positive duration; using %s\n", ChannelIntervalEnv, raw, channel.DefaultInterval)
		return channel.DefaultInterval
	}
	if d < channel.MinInterval {
		return channel.MinInterval
	}
	return d
}
```

Add `"github.com/schuettc/muster/internal/channel"` to main.go's imports. Also update the routing comment above `default:` if it enumerates main-owned commands (it references "serve/mcp/debug/lambda" in `wantsHelp`'s comment — add `channel` there).

- [ ] **Step 4: Write cmd/muster/channel.go**

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/schuettc/muster/internal/channel"
	"github.com/schuettc/muster/internal/channelmcp"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/tmuxenv"
	"github.com/schuettc/muster/internal/version"
)

// channelInstructions is the handshake text: the whole protocol, taught once.
const channelInstructions = `This channel is muster, the local coordination bus. A <channel source="muster-channel"> message means mail arrived on the bus for THIS session.
- It names the intent, sender, thread id and subject. The body is not included.
- action-requested or reply-requested: call get_thread with the thread id, do what it asks, then answer with the muster reply tool.
- fyi: read it with get_thread; no reply is needed.
- A push naming several items, or a "N unread" summary: call get_inbox and work through each thread.
- Act autonomously. Do not ask the user whether to check mail; reading it is the point.
- muster_channel_status reports which aliases this channel pushes for and why it might be idle.`

// runChannel serves a claude/channel MCP server on stdio for the session that
// launched it and runs the carrier beside it. stdout is the protocol.
func runChannel() {
	cap := tmuxenv.CaptureEnv()
	carrier := &channel.Carrier{
		Call:   channel.DaemonClient(paths.SocketPath()),
		Ident:  channel.Identity{SocketPath: cap.SocketPath, SessionID: cap.SessionID, PaneID: cap.PaneID, SessionCreated: cap.SessionCreated},
		Interval: channelInterval(),
	}
	srv := channelmcp.New(channelmcp.Handler{
		Name:         "muster-channel",
		Version:      version.Version(),
		Instructions: channelInstructions,
		Tools: []channelmcp.Tool{{
			Name:        "muster_channel_status",
			Description: "Is the muster channel attached? Reports the tmux pane, the aliases it pushes for, the journal cursor, the last push, and why it is idle if it is.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		}},
		Call: func(name string, _ json.RawMessage) (string, error) {
			if name != "muster_channel_status" {
				return "", fmt.Errorf("unknown tool %q", name)
			}
			return carrier.Status(), nil
		},
	})
	carrier.Notify = srv.Notify

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go carrier.Run(ctx)
	if err := srv.Run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "muster: channel:", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 5: Build and smoke the handshake by hand**

Run: `cd .worktrees/feat/channel && go build -o /tmp/muster-dev ./cmd/muster && printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}' '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"muster_channel_status","arguments":{}}}' | MUSTER_NO_AUTOSPAWN=1 /tmp/muster-dev channel 2>/dev/null`
Expected: two JSON lines — the initialize result with `"claude/channel"` under `experimental`, then a tool result whose text starts with `idle: no tmux pane` (this shell is not the tmux pane of a registered agent) or a pane line if it is.

Run: `/tmp/muster-dev channel --help | head -3`
Expected: the registry help text.

- [ ] **Step 6: Gate and commit**

Run: `cd .worktrees/feat/channel && just verify 2>&1 | tail -5`
Expected: all green.

```bash
cd .worktrees/feat/channel && git add cmd/muster internal/humancli && git commit -m "feat(channel): muster channel subcommand with MUSTER_CHANNEL_INTERVAL knob"
```

### Task 5: Integration test against a real daemon

**Files:**
- Create: `internal/channel/integration_test.go`

**Interfaces:**
- Consumes: `daemon.Serve(sock, store, nil, "")`, `store.Open`, `mustertest.ShortHome()`, `channel.DaemonClient`, `Carrier`.

- [ ] **Step 1: Write the test**

```go
package channel

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/mustertest"
	"github.com/schuettc/muster/internal/store"
)

// A real daemon on a temp socket; two registered agents; the carrier for
// "worker" sees exactly the mail meant for it.
func TestCarrierAgainstRealDaemon(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	t.Setenv("MUSTER_NO_AUTOSPAWN", "1")
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sock := filepath.Join(dir, "sock")
	d, err := daemon.Serve(sock, s, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	call := DaemonClient(sock)
	must := func(op string, args map[string]any) {
		t.Helper()
		if _, err := call(op, args); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	must("register_agent", map[string]any{"alias": "worker", "role": "producer", "model_type": "claude",
		"socket_path": "/tmp/tsock", "session_id": "$3", "pane_id": "%9", "session_name": "worker", "session_created": 1700000000})
	must("register_agent", map[string]any{"alias": "lead", "role": "producer", "model_type": "claude",
		"socket_path": "/tmp/tsock", "session_id": "$4", "pane_id": "%10", "session_name": "lead", "session_created": 1700000001})

	p := &pushes{}
	c := &Carrier{Call: call, Notify: p.notify, Errw: &strings.Builder{},
		Ident: Identity{SocketPath: "/tmp/tsock", SessionID: "$3", PaneID: "%9", SessionCreated: 1700000000}}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 0 {
		t.Fatalf("no unread at start, no summary: %v", p.got)
	}

	must("send_message", map[string]any{"from": "lead", "to_kind": "agent", "to_target": "worker",
		"subject": "review the branch", "body": "please", "intent": "action-requested"})
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 1 || !strings.Contains(p.got[0], `action-requested from lead on thread #`) || !strings.Contains(p.got[0], `"review the branch"`) {
		t.Fatalf("send → one push: %v", p.got)
	}
	threadID := threadFromPush(t, p.got[0])

	must("reply", map[string]any{"from": "lead", "thread_id": threadID, "body": "and one more thing"})
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 2 || !strings.HasPrefix(p.got[1], "muster: reply from lead") {
		t.Fatalf("reply → second push: %v", p.got)
	}

	must("get_inbox", map[string]any{"alias": "worker"})
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 2 {
		t.Fatalf("reading mail must not push: %v", p.got)
	}

	// worker's own outbound mail never comes back at it.
	must("send_message", map[string]any{"from": "worker", "to_kind": "agent", "to_target": "lead", "subject": "done", "body": "ok"})
	if err := c.Tick(); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 2 {
		t.Fatalf("self-authored send must not push: %v", p.got)
	}
	if !strings.Contains(c.Status(), "aliases: worker") {
		t.Errorf("status: %q", c.Status())
	}
}

// threadFromPush parses "thread #N" out of a single-event content line.
func threadFromPush(t *testing.T, content string) int64 {
	t.Helper()
	i := strings.Index(content, "thread #")
	if i < 0 {
		t.Fatalf("no thread id in %q", content)
	}
	var id int64
	for _, r := range content[i+len("thread #"):] {
		if r < '0' || r > '9' {
			break
		}
		id = id*10 + int64(r-'0')
	}
	if id == 0 {
		t.Fatalf("thread id parse failed for %q", content)
	}
	return id
}
```

- [ ] **Step 2: Run it**

Run: `cd .worktrees/feat/channel && go test -race ./internal/channel/ -run TestCarrierAgainstRealDaemon -v 2>&1 | tail -6`
Expected: PASS. If `session_aliases` returns no aliases, the registration is missing `session_created` or the socket/session/pane tuple differs from the carrier's Identity — they must match exactly.

- [ ] **Step 3: Gate and commit**

Run: `cd .worktrees/feat/channel && just verify 2>&1 | tail -3`

```bash
cd .worktrees/feat/channel && git add internal/channel/integration_test.go && git commit -m "test(channel): carrier against a real daemon"
```

### Task 6: `pi` in the nudge harness table

**Files:**
- Modify: `internal/nudge/nudge.go:76-81` (the `switch modelType` in `TypeLine`)
- Modify: `internal/nudge/nudge_test.go` (beside the `cursor` harness test at line ~54)

- [ ] **Step 1: Failing test**

Add next to the existing cursor case:

```go
func TestNudgePiPastesThenSubmitsAfterDelay(t *testing.T) {
	testNudgePasteSubmitHarness(t, "pi")
}
```

Run: `cd .worktrees/feat/channel && go test ./internal/nudge/ -run TestNudgePi 2>&1 | tail -3`
Expected: FAIL — `pi must report submitted=true`.

- [ ] **Step 2: Add `pi` to the delayed-submit case**

Change `case "codex", "cursor":` to `case "codex", "cursor", "pi":` and extend the comment above `TypeLine` so its list of delayed harnesses reads `codex, cursor and pi need pasteSubmitDelay`.

Run: `cd .worktrees/feat/channel && go test -race ./internal/nudge/ 2>&1 | tail -2`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
cd .worktrees/feat/channel && git add internal/nudge && git commit -m "feat(nudge): pi submits after the paste delay"
```

### Task 7: Docs, package map, version

**Files:**
- Modify: `README.md` (the "## Setup" step 2 block and the "## MCP mode" section)
- Modify: `contrib/README.md`
- Modify: `CLAUDE.md` (the "Package map" paragraph and the "Wake is split" bullet)
- Modify: `VERSION`

- [ ] **Step 1: README**

In "## Setup" step 2, after the Claude Code `claude mcp add muster -s user -- muster mcp` line add:

```bash
claude mcp add muster-channel -s user -- muster channel   # optional: push delivery (Claude Code channels)
```

Add a new section after "## MCP mode":

```markdown
## Channel mode

`muster channel` is an MCP **channel** carrier (Claude Code's `claude/channel` convention; pi via its `pi-channels` extension). Registered beside `muster mcp`, it tails the bus journal for the session's aliases and pushes a compact envelope — intent, sender, thread, subject, never the body — into the session the moment mail lands. An idle agent wakes and answers with `get_thread`/`reply`; nobody types. The 📬 badge, the Stop-hook drain and `muster nudge` are unchanged fallbacks. `MUSTER_CHANNEL_INTERVAL` (default `1s`) tunes the poll cadence; `muster_channel_status` (the carrier's one tool) reports what it is attached to.
```

- [ ] **Step 2: contrib/README.md**

Add a short subsection under the hooks material: the same registration line, and the note that the channel is additive — hooks stay as configured.

- [ ] **Step 3: CLAUDE.md**

Package map: append `· internal/channelmcp stdlib claude/channel MCP server · internal/channel the channel carrier (journal tail → push)` after the `internal/nudge` entry. "Wake is split" bullet: append a sentence — `internal/channel` pushes into the session over MCP and is the third wake path; like `wake`, it never types.

- [ ] **Step 4: VERSION**

Latest tag is `v0.14.0` and `VERSION` reads `0.14.0`. Set `VERSION` to `0.15.0` (a new subcommand is a minor bump). Per CLAUDE.md, pick from tags not from `dev` — re-run `git tag --sort=-v:refname | head -1` before writing and use one minor above it.

- [ ] **Step 5: Gate and commit**

Run: `cd .worktrees/feat/channel && just verify 2>&1 | tail -3`

```bash
cd .worktrees/feat/channel && git add README.md contrib/README.md CLAUDE.md VERSION && git commit -m "docs(channel): channel mode, package map, bump to 0.15.0"
```

### Task 8: Live trial on the Claude Code fleet **[COURT]**

**Files:** none in the repo (a note in the PR description).

- [ ] **Step 1: Install the branch binary**

Run: `cd .worktrees/feat/channel && CGO_ENABLED=0 go build -o ~/.local/bin/muster ./cmd/muster && muster version`

- [ ] **Step 2 [COURT]: Register the channel once**

Run: `claude mcp add muster-channel -s user -- muster channel`

- [ ] **Step 3 [COURT]: Two sessions, one push**

Start a fresh Claude Code session in tmux (it registers on the bus via the SessionStart hook). Leave it idle at its prompt. From any other session or the CLI: `muster send <that-alias> "channel trial — reply with the word ping" --intent reply-requested`. Watch the idle session: within about a second it should show the `<channel source="muster-channel">` message, call `get_thread`, and `reply` — with nobody typing. Then call `muster_channel_status` in that session and confirm it lists the alias and a recent last push.

- [ ] **Step 4: Record the outcome**

Note what happened (wake latency, any confusion in the agent's reading of the envelope) in the PR body when the branch goes up. If the agent needed the body in the push to act well, that is a spec revision, not a tweak — raise it before merging.
