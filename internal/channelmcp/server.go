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
