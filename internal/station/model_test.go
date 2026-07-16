package station

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/schuettc/muster/internal/render"
)

// keyMsg builds a tea.KeyMsg for the given key name, matching how the real
// program would deliver it (KeyTab for "tab", KeyRunes otherwise) — enough
// for exercising Update's key.Matches branches without a PTY.
func keyMsg(name string) tea.KeyMsg {
	if name == "tab" {
		return tea.KeyMsg{Type: tea.KeyTab}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)}
}

// fakeCaller is the model-level test double for render.Caller: every op
// resolves through fn, or a zero-value success ("{}"") when fn is nil, so
// tests that never drive Init/pollCmd (only feed typed msgs via Update)
// don't need to stub anything.
type fakeCaller struct {
	fn func(op string, args map[string]any) (json.RawMessage, error)
}

func (f fakeCaller) Call(op string, args map[string]any) (json.RawMessage, error) {
	if f.fn != nil {
		return f.fn(op, args)
	}
	return json.RawMessage(`{}`), nil
}

// mustModel type-asserts the tea.Model Update returns back to a station
// Model, failing the test if the framework interface ever holds something
// else.
func mustModel(t *testing.T, v interface{}) Model {
	t.Helper()
	m, ok := v.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want station.Model", v)
	}
	return m
}

// TestCursorAdvancesOnlyOnAppliedEvents is the data-loop's core invariant
// (spec §5): the cursor moves ONLY in the events-msg branch, and only after
// a page is actually applied. A threads-fetch failure between two
// successful events pages must leave the cursor untouched and must not
// cause the next events page to skip anything.
func TestCursorAdvancesOnlyOnAppliedEvents(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})

	next, cmd := m.Update(eventsMsg{page: render.EventsPage{
		Events: []render.EventRow{{ID: 1, Kind: "send", Agent: "a", Target: "agent:b", Subject: "s1"}},
		MaxID:  1,
	}})
	m = mustModel(t, next)
	if cmd != nil {
		t.Fatalf("applying an events page must not issue a Cmd")
	}
	if m.cursor != 1 {
		t.Fatalf("cursor = %d, want 1 after the first applied page", m.cursor)
	}

	// A threads-fetch failure must land on the status line and leave the
	// cursor exactly where it was.
	next, _ = m.Update(threadsMsg{err: errors.New("boom")})
	m = mustModel(t, next)
	if m.cursor != 1 {
		t.Fatalf("a threads failure moved the cursor: got %d, want 1", m.cursor)
	}
	if !strings.Contains(m.status, "boom") {
		t.Fatalf("status should note the threads failure, got %q", m.status)
	}

	// The next events page must apply cleanly: nothing skipped, both pages'
	// events present, cursor caught up to the new max_id.
	next, _ = m.Update(eventsMsg{page: render.EventsPage{
		Events: []render.EventRow{{ID: 2, Kind: "send", Agent: "a", Target: "agent:b", Subject: "s2"}},
		MaxID:  2,
	}})
	m = mustModel(t, next)
	if m.cursor != 2 {
		t.Fatalf("cursor = %d, want 2 after the second applied page", m.cursor)
	}
	if len(m.events) != 2 {
		t.Fatalf("expected both pages' events applied (2 total), got %d: %+v", len(m.events), m.events)
	}

	// A failed events fetch itself must be equally inert: status note only,
	// no cursor movement, no events lost or duplicated.
	next, _ = m.Update(eventsMsg{err: errors.New("dial failed")})
	m = mustModel(t, next)
	if m.cursor != 2 {
		t.Fatalf("a failed events fetch moved the cursor: got %d, want 2", m.cursor)
	}
	if len(m.events) != 2 {
		t.Fatalf("a failed events fetch changed the buffered events: got %d, want 2", len(m.events))
	}

	// A regression (max_id < cursor) resets the cursor to the new tail
	// without applying the stale page's events.
	next, _ = m.Update(eventsMsg{page: render.EventsPage{
		Events: []render.EventRow{{ID: 1, Kind: "send", Agent: "a", Target: "agent:b", Subject: "stale"}},
		MaxID:  1,
	}})
	m = mustModel(t, next)
	if m.cursor != 1 {
		t.Fatalf("regression reset: cursor = %d, want 1 (the new tail)", m.cursor)
	}
	if len(m.events) != 2 {
		t.Fatalf("regression must not apply the stale page's events: got %d events, want 2", len(m.events))
	}
	if !strings.Contains(m.status, "reset") {
		t.Fatalf("status should note the regression reset, got %q", m.status)
	}
}

// TestRosterRendersLabelsAndCounts checks the roster pane groups by project
// and shows each agent's live dot, label (with alias fallback), and
// per-session unread count (with an "!" marker for action-requested unread).
func TestRosterRendersLabelsAndCounts(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "backend-1", Project: "muster", Label: "backend", Live: true, Unread: 3},
		{Alias: "reviewer-1", Project: "muster", Label: "review", Live: false, Unread: 1, Action: true},
		{Alias: "no-label-1", Project: "other", Live: true},
	}})
	m = mustModel(t, next)

	view := m.View()
	for _, want := range []string{"muster", "other", "backend", "review", "no-label-1", "(3)", "(1!)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("roster view missing %q:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "●") {
		t.Fatalf("roster view missing the live dot:\n%s", view)
	}
	if !strings.Contains(view, "✗") {
		t.Fatalf("roster view missing the dead dot:\n%s", view)
	}
	// no-label-1 has no explicit label — the alias itself must stand in,
	// and unread must render nothing (no stray "(0)").
	if strings.Contains(view, "(0)") {
		t.Fatalf("a zero-unread row must not render a count:\n%s", view)
	}
}

// TestFeedUsesRendererVocabulary checks the feed pane renders through
// render.Renderer verbatim: labels resolve via the roster's alias→label map,
// send/target arrows read 'from → to', and notify folds its count into the
// outcome ('lit(2)').
func TestFeedUsesRendererVocabulary(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "bhw-3", Label: "code review"}}})
	m = mustModel(t, next)

	next, _ = m.Update(eventsMsg{page: render.EventsPage{
		Events: []render.EventRow{
			{ID: 1, Kind: "send", Agent: "web", Target: "agent:bhw-3", ThreadID: 1, Subject: "review req"},
			{ID: 2, Kind: "notify", Agent: "bhw-3", ThreadID: 1, Count: 2, Detail: "lit"},
		},
		MaxID: 2,
	}})
	m = mustModel(t, next)

	view := m.View()
	for _, want := range []string{"web → code review", "lit(2)", "#1", "WHAT"} {
		if !strings.Contains(view, want) {
			t.Fatalf("feed view missing %q:\n%s", want, view)
		}
	}
}

// TestTabCyclesFocusAndRosterCursorMoves exercises the read-only keys this
// task wires: Tab cycles pane focus, j/k move the roster cursor (bounded),
// q sets quitting and issues tea.Quit.
func TestTabCyclesFocusAndRosterCursorMoves(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "a", Project: "p"}, {Alias: "b", Project: "p"}, {Alias: "c", Project: "p"},
	}})
	m = mustModel(t, next)
	if m.focus != paneRoster {
		t.Fatalf("initial focus = %v, want paneRoster", m.focus)
	}

	next, _ = m.Update(keyMsg("j"))
	m = mustModel(t, next)
	if m.rosterIdx != 1 {
		t.Fatalf("rosterIdx = %d after j, want 1", m.rosterIdx)
	}
	next, _ = m.Update(keyMsg("k"))
	m = mustModel(t, next)
	if m.rosterIdx != 0 {
		t.Fatalf("rosterIdx = %d after k, want 0", m.rosterIdx)
	}
	// k at the top must not go negative.
	next, _ = m.Update(keyMsg("k"))
	m = mustModel(t, next)
	if m.rosterIdx != 0 {
		t.Fatalf("rosterIdx = %d, want clamped to 0", m.rosterIdx)
	}

	next, _ = m.Update(keyMsg("tab"))
	m = mustModel(t, next)
	if m.focus != paneFeed {
		t.Fatalf("focus after tab = %v, want paneFeed", m.focus)
	}
	next, _ = m.Update(keyMsg("tab"))
	m = mustModel(t, next)
	if m.focus != paneThreads {
		t.Fatalf("focus after second tab = %v, want paneThreads", m.focus)
	}
	next, _ = m.Update(keyMsg("tab"))
	m = mustModel(t, next)
	if m.focus != paneRoster {
		t.Fatalf("focus after third tab = %v, want it wraps back to paneRoster", m.focus)
	}

	next, cmd := m.Update(keyMsg("q"))
	m = mustModel(t, next)
	if !m.quitting {
		t.Fatalf("q must set quitting")
	}
	if cmd == nil {
		t.Fatalf("q must issue a Cmd (tea.Quit)")
	}
}
