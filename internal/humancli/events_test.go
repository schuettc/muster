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
