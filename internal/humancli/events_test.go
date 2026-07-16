package humancli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/display"
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
		r.Line(&out, e)
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

// TestRendererReplyShowsPreviewNotSubject extends the duplicate-look finding:
// a reply row with a non-empty Detail (the journaled body preview) must show
// '↳ <detail>' instead of the thread subject, so an announcement and its
// reply no longer look like a double-send. A reply with no Detail (older
// rows, or a preview that came back empty) still falls back to the subject.
func TestRendererReplyShowsPreviewNotSubject(t *testing.T) {
	rows := []eventRow{
		{Kind: "reply", Agent: "bhw-3", ThreadID: 19, Subject: "spec review", Detail: "looks good, shipping"},
		{Kind: "reply", Agent: "bhw-3", ThreadID: 20, Subject: "no detail case"},
	}
	r := newRenderer(rows, nil, false, false, 120)
	var out bytes.Buffer
	for _, e := range rows {
		r.Line(&out, e)
	}
	got := out.String()
	if !strings.Contains(got, "↳ looks good, shipping") {
		t.Fatalf("reply row must render '↳ <detail>':\n%s", got)
	}
	if strings.Contains(got, "spec review") {
		t.Fatalf("reply row with a detail must not also show the subject (duplicate-look):\n%s", got)
	}
	if !strings.Contains(got, "no detail case") {
		t.Fatalf("reply row with no detail must fall back to the subject:\n%s", got)
	}
}

// TestRendererIntentTags: send/task rows append the [fyi]/[reply?]/[action]
// tag when Intent is set; rows with no intent are untagged.
func TestRendererIntentTags(t *testing.T) {
	rows := []eventRow{
		{Kind: "send", Agent: "a", Target: "agent:b", Subject: "ship it", Intent: "fyi"},
		{Kind: "send", Agent: "a", Target: "agent:b", Subject: "please check", Intent: "reply-requested"},
		{Kind: "task", Agent: "a", Target: "agent:b", Subject: "do the thing", Intent: "action-requested"},
		{Kind: "send", Agent: "a", Target: "agent:b", Subject: "no tag here"},
	}
	r := newRenderer(rows, nil, false, false, 120)
	var out bytes.Buffer
	for _, e := range rows {
		r.Line(&out, e)
	}
	got := out.String()
	for _, want := range []string{"ship it [fyi]", "please check [reply?]", "do the thing [action]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderer output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "no tag here [") {
		t.Fatalf("row with unset intent must not be tagged:\n%s", got)
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

// TestEventsWideRuneWhoFitsWidthAndAligns reproduces the reviewer's finding:
// renderer.fit() measures WHO with display.Width (wide CJK/fullwidth runes
// count as 2), but header()/line() used to pad with fmt's "%-*s", which pads
// by RUNE COUNT. A wide-rune WHO therefore (a) pushed the true rendered
// width of a line past the --width budget, and (b) misaligned the WHAT
// column across rows. Both rows below share a subject marker; the label
// "中文中文中文中文" (8 wide runes, 16 display columns but only 8 runes) is
// what used to trigger the over-pad.
func TestEventsWideRuneWhoFitsWidthAndAligns(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "cjk-agent", "model_type": "claude", "label": "中文中文中文中文"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{"alias": "ascii-agent", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "cjk-agent", "subject": "MARK", "body": "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "ascii-agent", "subject": "MARK", "body": "b"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Dispatch([]string{"events", "--width", "60"}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")

	// (a) every rendered line must fit the display-width budget, not just
	// the rune-count budget.
	for _, line := range lines {
		if w := display.Width(line); w > 60 {
			t.Fatalf("line exceeds display-width budget (%d > 60): %q", w, line)
		}
	}

	// (b) the WHAT column (located by the shared MARK subject)
	// must start at the same display column in both rows — the ASCII WHO
	// row and the wide-rune WHO row.
	var starts []int
	for _, line := range lines {
		idx := strings.Index(line, "MARK")
		if idx < 0 {
			continue
		}
		starts = append(starts, display.Width(line[:idx]))
	}
	if len(starts) != 2 {
		t.Fatalf("expected 2 rows carrying MARK, got %d:\n%s", len(starts), out.String())
	}
	if starts[0] != starts[1] {
		t.Fatalf("WHAT column misaligned across ASCII vs wide-rune WHO rows: %v\n%s", starts, out.String())
	}
}

// TestEventsTaskCreateNoIntentShowsActionTag: a task_create with no explicit
// --intent still carries the thread's EFFECTIVE intent (store's
// effectiveIntent treats an unset task intent as action-requested), and that
// effective value — not the raw stored "" — is what the journal surface
// (muster events) renders as the "[action]" tag.
func TestEventsTaskCreateNoIntentShowsActionTag(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "rev", "role": "reviewer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("task_create", map[string]any{"from": "backend", "to_kind": "role", "to_target": "reviewer", "subject": "please review", "body": "y"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Dispatch([]string{"events", "--kind", "task"}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "please review [action]") {
		t.Fatalf("task_create with no explicit intent must render the effective-intent [action] tag:\n%s", got)
	}
}
