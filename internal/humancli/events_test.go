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
	if !strings.Contains(got, "WHAT") || !strings.Contains(got, "line1 line2") {
		t.Fatalf("send row with sanitized subject expected:\n%s", got)
	}
	if strings.Count(got, "line1") != 1 {
		t.Fatalf("subject must print once, not duplicated via detail:\n%s", got)
	}
	if !strings.Contains(got, "web → api") {
		t.Fatalf("send row must render direction 'web → api':\n%s", got)
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

// TestEventsRendersLabelsAndDirections: WHO shows current labels (alias
// fallback), notifies render as deliveries ('→ x') with the count folded
// into the outcome, and --aliases restores the raw view.
func TestEventsRendersLabelsAndDirections(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "bettor-help-workspace-3", "model_type": "codex", "label": "code review", "socket_path": "/s", "session_id": "$2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "bettor-help-workspace-3", "subject": "review req", "body": "b"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Dispatch([]string{"events"}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "web → code review") {
		t.Fatalf("send WHO must use the recipient's label:\n%s", got)
	}
	out.Reset()
	if err := Dispatch([]string{"events", "--aliases"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "bettor-help-workspace-3") || strings.Contains(out.String(), "code review") {
		t.Fatalf("--aliases must show raw aliases only:\n%s", out.String())
	}
}

// TestRendererKindShapes exercises the per-kind WHO/WHAT rendering directly
// (the humancli test daemon runs without a notifier, so notify/nudge rows
// never reach the integration path).
func TestRendererKindShapes(t *testing.T) {
	labels := map[string]string{"bhw-3": "code review"}
	rows := []eventRow{
		{Kind: "notify", Agent: "bhw-3", ThreadID: 19, Count: 2, Detail: "lit", Subject: "spec review"},
		{Kind: "nudge", Target: "bhw-3", Detail: "submitted"},
		{Kind: "reply", Agent: "bhw-3", ThreadID: 19, Subject: "spec review"},
		{Kind: "read", Agent: "bhw-3"},
	}
	r := newRenderer(rows, labels, false, false, 120)
	var out bytes.Buffer
	for _, e := range rows {
		r.line(&out, e)
	}
	got := out.String()
	for _, want := range []string{
		"→ code review", "lit(2) — spec review", // notify: delivery arrow + folded count
		"submitted",   // nudge keeps its outcome
		"#19",         // thread renders with # prefix
		"code review", // reply shows the bare actor label
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderer output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "spec review — spec review") {
		t.Fatalf("subject duplicated:\n%s", got)
	}
}

// TestEventsLinesFitWidth: with a tight --width, no rendered line exceeds
// the budget (the WHAT column absorbs the squeeze).
func TestEventsLinesFitWidth(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "api", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("subject word ", 30)
	if _, err := callData("send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": long, "body": "b"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Dispatch([]string{"events", "--width", "80"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if n := len([]rune(line)); n > 80 {
			t.Fatalf("line exceeds width budget (%d > 80): %q", n, line)
		}
	}
}
