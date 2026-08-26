package channelmcp

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// drive starts a Server over in-memory pipes and returns a writer for client
// messages and a scanner over the server's replies.
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

// The handshake is the whole point: the experimental channel capability, the
// echoed protocol version, and the instructions string are what make the
// binary a channel at all.
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
		Call: func(name string, _ json.RawMessage) (string, error) {
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

// A notification carries no id and must never be answered; an unknown request
// with an id must be answered with -32601 rather than silence.
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
