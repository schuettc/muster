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
	if err := cmdSend([]string{"--broadcast", "--yes", "--project", "web", "deploy", "landed", "--from", "tester"}, &buf); err != nil {
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
	if err := cmdSend([]string{"--broadcast", "--yes", "muster", "is", "broken", "--from", "tester"}, &buf); err != nil {
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

func TestSendBroadcastStandingFlag(t *testing.T) {
	s := startTestDaemon(t)
	var buf bytes.Buffer
	if err := cmdSend([]string{"--broadcast", "--yes", "--standing", "read", "CONTRACT.md", "--from", "tester"}, &buf); err != nil {
		t.Fatalf("standing broadcast send: %v", err)
	}
	ths, err := s.Threads(10)
	if err != nil || len(ths) != 1 {
		t.Fatalf("threads: %v (%d)", err, len(ths))
	}
	if ths[0].ToKind != "broadcast" || ths[0].ToTarget != "" || !ths[0].Standing {
		t.Fatalf("want standing global broadcast, got kind=%s target=%q standing=%v", ths[0].ToKind, ths[0].ToTarget, ths[0].Standing)
	}
}

func TestSendBroadcastWakeFlag(t *testing.T) {
	s := startTestDaemon(t)
	var buf bytes.Buffer
	if err := cmdSend([]string{"--broadcast", "--yes", "--wake", "deploy", "now", "--from", "tester"}, &buf); err != nil {
		t.Fatalf("wake broadcast send: %v", err)
	}
	ths, err := s.Threads(10)
	if err != nil || len(ths) != 1 || !ths[0].Wake {
		t.Fatalf("want a wake broadcast, got %+v (%v)", ths, err)
	}
}

func TestSendWakeWithoutBroadcastErrors(t *testing.T) {
	startTestDaemon(t)
	var buf bytes.Buffer
	err := cmdSend([]string{"--wake", "web:label", "hello"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "--wake requires --broadcast") {
		t.Fatalf("want '--wake requires --broadcast' error, got %v", err)
	}
}

func TestSendStandingWithoutBroadcastErrors(t *testing.T) {
	startTestDaemon(t)
	var buf bytes.Buffer
	err := cmdSend([]string{"--standing", "web:label", "hello"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "--standing requires --broadcast") {
		t.Fatalf("want '--standing requires --broadcast' error, got %v", err)
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

func TestSendBroadcastPromptConfirms(t *testing.T) {
	s := startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "w1", "project": "web", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	oldInteractive, oldIn := sendInteractive, sendConfirmIn
	sendInteractive = func() bool { return true }
	sendConfirmIn = strings.NewReader("y\n")
	defer func() { sendInteractive, sendConfirmIn = oldInteractive, oldIn }()

	var buf bytes.Buffer
	if err := cmdSend([]string{"--broadcast", "--project", "web", "hi", "--from", "tester"}, &buf); err != nil {
		t.Fatalf("prompted broadcast: %v", err)
	}
	ths, err := s.Threads(10)
	if err != nil || len(ths) != 1 {
		t.Fatalf("a confirmed prompt must send exactly one thread: %v (%d)", err, len(ths))
	}
	if !strings.Contains(buf.String(), "reaches 1 agent") {
		t.Fatalf("prompt should show the blast radius, got %q", buf.String())
	}
}

func TestSendBroadcastPromptAborts(t *testing.T) {
	s := startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "w1", "project": "web", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	oldInteractive, oldIn := sendInteractive, sendConfirmIn
	sendInteractive = func() bool { return true }
	sendConfirmIn = strings.NewReader("n\n")
	defer func() { sendInteractive, sendConfirmIn = oldInteractive, oldIn }()

	var buf bytes.Buffer
	if err := cmdSend([]string{"--broadcast", "--project", "web", "hi", "--from", "tester"}, &buf); err != nil {
		t.Fatalf("a declined prompt is not an error: %v", err)
	}
	ths, err := s.Threads(10)
	if err != nil || len(ths) != 0 {
		t.Fatalf("a declined prompt must send nothing: %v (%d)", err, len(ths))
	}
	if !strings.Contains(buf.String(), "aborted") {
		t.Fatalf("want an abort notice, got %q", buf.String())
	}
}

func TestSendBroadcastNonInteractiveNeedsYes(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "w1", "project": "web", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	old := sendInteractive
	sendInteractive = func() bool { return false }
	defer func() { sendInteractive = old }()

	var buf bytes.Buffer
	err := cmdSend([]string{"--broadcast", "--project", "web", "hi", "--from", "tester"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("a non-interactive broadcast without --yes must demand --yes, got %v", err)
	}
}
