package station

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/schuettc/muster/internal/render"
)

// flattenCmds executes cmd and, if it resolves to a tea.BatchMsg, recurses
// into each sub-Cmd — letting tests drive pollCmd()/Init()'s fetch tree
// without a running Bubble Tea program. Never used on a Cmd that contains
// tickCmd's timer (that would block for the model's --interval); tests that
// need the tick's reschedule construct tickMsg directly instead.
func flattenCmds(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, flattenCmds(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// eventRowsDesc builds n render.EventRow values with descending IDs from
// hi down to hi-n+1 — the shape a BACKLOG page comes back in (newest-first).
func eventRowsDesc(hi int64, n int) []render.EventRow {
	rows := make([]render.EventRow, n)
	for i := 0; i < n; i++ {
		rows[i] = render.EventRow{ID: hi - int64(i), Kind: "send", Agent: "a", Target: "agent:b", Subject: "s"}
	}
	return rows
}

// eventRowsAsc builds n render.EventRow values with ascending IDs starting
// at lo — the shape a FOLLOW page comes back in (oldest-first).
func eventRowsAsc(lo int64, n int) []render.EventRow {
	rows := make([]render.EventRow, n)
	for i := 0; i < n; i++ {
		rows[i] = render.EventRow{ID: lo + int64(i), Kind: "send", Agent: "a", Target: "agent:b", Subject: "s"}
	}
	return rows
}

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

// TestColdStartBootstrapsFromBacklogNotFollow is Finding 1's regression test:
// a fresh Model must never issue a follow-mode list_events call with
// after_id=0. Against a mature journal, follow mode from 0 would let the
// daemon cap the page at its oldest-1000-row window while still reporting
// the GLOBAL tail as max_id — applyEvents would then jump the cursor straight
// to that tail, silently skipping every event between row 1000 and the tail.
// The fix: the first fetch is BACKLOG mode, seeding the cursor from its own
// max_id; only the next tick follows, and from that seeded cursor.
func TestColdStartBootstrapsFromBacklogNotFollow(t *testing.T) {
	const tailMaxID = 5000
	var calls []map[string]any
	fake := fakeCaller{fn: func(op string, args map[string]any) (json.RawMessage, error) {
		if op != "list_events" {
			return json.RawMessage(`{}`), nil
		}
		calls = append(calls, args)
		if _, ok := args["backlog"]; ok {
			page := render.EventsPage{Events: eventRowsDesc(tailMaxID, 10), MaxID: tailMaxID}
			return json.Marshal(page)
		}
		// A follow call from after_id=0 against a mature journal: the
		// daemon's oldest-1000-row cap, but still reporting the real tail.
		page := render.EventsPage{Events: eventRowsAsc(1, 1000), MaxID: tailMaxID}
		return json.Marshal(page)
	}}

	m := NewModel(fake, Options{})
	msgs := flattenCmds(m.pollCmd())

	var model tea.Model = m
	for _, msg := range msgs {
		if em, ok := msg.(eventsMsg); ok && !em.backlog {
			t.Fatalf("cold start issued a follow-mode eventsMsg, want only a backlog one: %+v", em)
		}
		model, _ = model.Update(msg)
	}
	mm := mustModel(t, model)
	if mm.cursor != tailMaxID {
		t.Fatalf("cursor after cold start = %d, want %d seeded from the backlog response", mm.cursor, tailMaxID)
	}
	if !mm.bootstrapped {
		t.Fatalf("model must be bootstrapped after the backlog page applies")
	}
	if len(mm.events) != 10 {
		t.Fatalf("expected the 10 backlog events applied, got %d", len(mm.events))
	}
	for _, args := range calls {
		if s, ok := args["after_id"].(string); ok && s == "0" {
			t.Fatalf("model issued a follow-mode list_events call with after_id=0: %+v", args)
		}
	}

	// The next tick must follow FROM the seeded cursor, not from 0.
	next, _ := mm.Update(tickMsg(time.Now()))
	mm2 := mustModel(t, next)
	calls = nil
	_ = flattenCmds(mm2.pollCmd())
	sawFollow := false
	for _, args := range calls {
		if s, ok := args["after_id"].(string); ok {
			sawFollow = true
			if s != "5000" {
				t.Fatalf("follow call after_id = %q, want \"5000\"", s)
			}
		}
	}
	if !sawFollow {
		t.Fatalf("expected a follow-mode list_events call once bootstrapped")
	}
}

// TestStaleTickEventsMsgDiscarded is Finding 2's regression test: an
// events msg from an older tick generation must never apply after a newer
// generation's msg already has — otherwise a slow in-flight fetch can
// double-apply events, or look like a journal regression (its stale max_id
// below the already-advanced cursor) and reset the cursor backwards.
func TestStaleTickEventsMsgDiscarded(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	m.bootstrapped = true // skip backlog bootstrap; exercise follow-mode gating directly
	m.pollGen = 2         // pretend two ticks have already fired

	next, _ := m.Update(eventsMsg{
		gen: 2,
		page: render.EventsPage{
			Events: []render.EventRow{{ID: 500, Kind: "send", Agent: "a", Target: "agent:b", Subject: "current"}},
			MaxID:  500,
		},
	})
	m = mustModel(t, next)
	if m.cursor != 500 {
		t.Fatalf("cursor = %d, want 500 after applying the current (gen-2) page", m.cursor)
	}

	// A gen-1 (stale) msg resolves late, with a lower max_id than the cursor
	// has already advanced past — without gating this reads as a regression.
	next, cmd := m.Update(eventsMsg{
		gen: 1,
		page: render.EventsPage{
			Events: []render.EventRow{{ID: 100, Kind: "send", Agent: "a", Target: "agent:b", Subject: "stale"}},
			MaxID:  100,
		},
	})
	m2 := mustModel(t, next)
	if cmd != nil {
		t.Fatalf("discarding a stale events msg must not issue a Cmd")
	}
	if m2.cursor != 500 {
		t.Fatalf("stale gen-1 msg moved the cursor: got %d, want 500 unchanged", m2.cursor)
	}
	if len(m2.events) != 1 {
		t.Fatalf("stale gen-1 msg altered the buffered events: got %d, want 1", len(m2.events))
	}
	if strings.Contains(m2.status, "reset") {
		t.Fatalf("stale gen-1 msg must not trigger a regression-reset status note, got %q", m2.status)
	}
}
