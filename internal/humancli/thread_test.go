package humancli

import (
	"bytes"
	"strings"
	"testing"
)

// TestThreadAndReplyRoundTrip covers the CLI half of the read-and-respond
// loop (the MCP-less fallback the hook nudge points at): send opens a
// thread, reply appends to it, and thread prints the whole conversation —
// header, both authors, and verbatim multi-line bodies.
func TestThreadAndReplyRoundTrip(t *testing.T) {
	sock := startCLITestDaemon(t)
	registerViaDaemon(t, sock, "peer", "/s", "$1")

	var buf bytes.Buffer
	if err := cmdSend([]string{"peer", "first line", "--from", "tester", "--subject", "roundtrip"}, &buf); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(buf.String(), "sent (thread ") {
		t.Fatalf("unexpected send output: %q", buf.String())
	}

	buf.Reset()
	if err := cmdReply([]string{"1", "got it, on it", "--from", "peer"}, &buf); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if !strings.Contains(buf.String(), "replied to thread 1 (entry ") {
		t.Fatalf("unexpected reply output: %q", buf.String())
	}

	// A multi-line body (the shape MCP tool calls produce) must print
	// verbatim, every line indented under its entry header.
	if _, err := callData("reply", map[string]any{
		"thread_id": 1, "from": "peer", "body": "line one\nline two",
	}); err != nil {
		t.Fatal(err)
	}

	buf.Reset()
	if err := cmdThread([]string{"1"}, &buf); err != nil {
		t.Fatalf("thread: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"thread 1 · message · tester → agent:peer",
		"subject: roundtrip",
		"] tester",
		"  first line",
		"] peer",
		"  got it, on it",
		"  line one\n  line two",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("thread output missing %q:\n%s", want, out)
		}
	}
}

// TestThreadAndReplyErrors pins the argument contract: non-numeric ids,
// unknown threads, and missing positionals all fail with usable errors
// instead of daemon round-trips or silent no-ops.
func TestThreadAndReplyErrors(t *testing.T) {
	startCLITestDaemon(t)
	var buf bytes.Buffer
	if err := cmdThread([]string{"abc"}, &buf); err == nil || !strings.Contains(err.Error(), "must be a number") {
		t.Fatalf("thread abc: want numeric-id error, got %v", err)
	}
	if err := cmdThread(nil, &buf); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("thread with no args: want usage error, got %v", err)
	}
	if err := cmdReply([]string{"999", "hello"}, &buf); err == nil || !strings.Contains(err.Error(), "thread") {
		t.Fatalf("reply to unknown thread: want thread-not-found error, got %v", err)
	}
	if err := cmdReply([]string{"1"}, &buf); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("reply with no body: want usage error, got %v", err)
	}
}
