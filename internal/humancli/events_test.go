package humancli

import (
	"bytes"
	"strings"
	"testing"
)

func TestEventsCommandPrintsLog(t *testing.T) {
	startTestDaemon(t)
	// A send to a session-less agent produces a "skipped" notify event only
	// when a notifier is wired; with the nil-notifier test daemon the event
	// log is fed by get_inbox reads.
	if _, err := callData("register_agent", map[string]any{"alias": "api", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("get_inbox", map[string]any{"alias": "api"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Dispatch([]string{"events"}, &out); err != nil {
		t.Fatalf("events: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "KIND") || !strings.Contains(got, "read") || !strings.Contains(got, "api") {
		t.Fatalf("events output missing header or read event:\n%s", got)
	}
	// --agent filter excludes other agents' events.
	out.Reset()
	if err := Dispatch([]string{"events", "--agent", "nobody"}, &out); err != nil {
		t.Fatalf("events --agent: %v", err)
	}
	if strings.Contains(out.String(), "api") {
		t.Fatalf("--agent nobody must filter out api's events:\n%s", out.String())
	}
}

func TestEventsFiltersAndOneLineRendering(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "api", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "line1\nline2\ttabbed", "body": "b"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Dispatch([]string{"events", "--kind", "send"}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "SUBJECT") || !strings.Contains(got, "line1 line2") {
		t.Fatalf("send row with sanitized subject expected:\n%s", got)
	}
	if lines := strings.Count(got, "\n"); lines != 2 { // header + one row
		t.Fatalf("multi-line subject leaked, %d lines:\n%s", lines, got)
	}
	out.Reset()
	if err := Dispatch([]string{"events", "--kind", "read", "--thread", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "send") {
		t.Fatalf("kind filter leaked send rows:\n%s", out.String())
	}
}
