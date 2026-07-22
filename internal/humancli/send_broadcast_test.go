package humancli

import (
	"bytes"
	"strings"
	"testing"
)

func TestSendBroadcastProjectFlag(t *testing.T) {
	s := startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "w1", "project": "web", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := cmdSend([]string{"--broadcast", "--project", "web", "deploy", "landed", "--from", "tester"}, &buf); err != nil {
		t.Fatalf("scoped broadcast send: %v", err)
	}
	ths, err := s.Threads(10)
	if err != nil || len(ths) != 1 {
		t.Fatalf("threads: %v (%d)", err, len(ths))
	}
	if ths[0].ToKind != "broadcast" || ths[0].ToTarget != "web" {
		t.Fatalf("stored thread addressed %s:%q, want broadcast:web", ths[0].ToKind, ths[0].ToTarget)
	}
	_, entries, err := s.GetThread(ths[0].ID)
	if err != nil || len(entries) != 1 || entries[0].Body != "deploy landed" {
		t.Fatalf("unquoted body must join: %v / %+v", err, entries)
	}
}

func TestSendProjectWithoutBroadcastErrors(t *testing.T) {
	startTestDaemon(t)
	var buf bytes.Buffer
	err := cmdSend([]string{"--project", "web", "hello"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "--project requires --broadcast") {
		t.Fatalf("want '--project requires --broadcast' error, got %v", err)
	}
}

func TestSendBroadcastUnquotedBodyStaysGlobal(t *testing.T) {
	s := startTestDaemon(t)
	// "muster" is a real project on the roster — the exact collision the
	// rejected positional form would have silently mis-scoped.
	if _, err := callData("register_agent", map[string]any{"alias": "m1", "project": "muster", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := cmdSend([]string{"--broadcast", "muster", "is", "broken", "--from", "tester"}, &buf); err != nil {
		t.Fatalf("global broadcast send: %v", err)
	}
	ths, err := s.Threads(10)
	if err != nil || len(ths) != 1 {
		t.Fatalf("threads: %v (%d)", err, len(ths))
	}
	if ths[0].ToTarget != "" {
		t.Fatalf("unquoted broadcast body must stay global, got to_target=%q", ths[0].ToTarget)
	}
	_, entries, err := s.GetThread(ths[0].ID)
	if err != nil || len(entries) != 1 || entries[0].Body != "muster is broken" {
		t.Fatalf("body must join all positionals: %v / %+v", err, entries)
	}
}

func TestSendBroadcastUnknownProjectSurfacesDaemonError(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "w1", "project": "web", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err := cmdSend([]string{"--broadcast", "--project", "wbe", "typo", "--from", "tester"}, &buf)
	if err == nil || !strings.Contains(err.Error(), `no registered agents in project "wbe"`) {
		t.Fatalf("daemon validation error must surface through the CLI, got %v", err)
	}
}
