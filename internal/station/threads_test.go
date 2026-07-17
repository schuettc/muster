package station

import (
	"encoding/json"
	"testing"

	"github.com/schuettc/muster/internal/render"
)

// focusThreads drives the model from its initial paneRoster focus to
// paneThreads via two Tab presses — the same path the real program takes.
func focusThreads(t *testing.T, m Model) Model {
	t.Helper()
	for i := 0; i < 2; i++ {
		next, _ := m.Update(keyMsg("tab"))
		m = mustModel(t, next)
	}
	if m.focus != paneThreads {
		t.Fatalf("focus = %v, want paneThreads after two tabs", m.focus)
	}
	return m
}

// TestThreadsGroupingOrder is the grouping-order test (spec §5,
// brief item 1): action-requested pinned first, then reply-requested, then
// everything else, and — since list_threads already returns updated_at
// DESC, id DESC — grouping must be a STABLE partition that never re-sorts
// within a bucket.
func TestThreadsGroupingOrder(t *testing.T) {
	rows := []listThreadRow{
		{ID: 5, Intent: "", UpdatedAt: 500},                 // rest
		{ID: 4, Intent: "reply-requested", UpdatedAt: 400},  // reply
		{ID: 3, Intent: "action-requested", UpdatedAt: 300}, // action
		{ID: 2, Intent: "fyi", UpdatedAt: 200},              // rest
		{ID: 1, Intent: "action-requested", UpdatedAt: 100}, // action
	}
	got := groupThreads(rows)
	wantIDs := []int64{3, 1, 4, 5, 2}
	if len(got) != len(wantIDs) {
		t.Fatalf("groupThreads returned %d rows, want %d", len(got), len(wantIDs))
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Fatalf("groupThreads[%d].ID = %d, want %d (order %v)", i, got[i].ID, id, wantIDs)
		}
	}
}

// TestThreadSelectionStableAcrossRegroup is the selection-stability test
// (spec §5, brief item 1): a thread that moves between intent buckets across
// a poll must keep the SAME thread selected by ID — a poll must never jump
// the operator's cursor.
func TestThreadSelectionStableAcrossRegroup(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, Intent: "action-requested", UpdatedAt: 300},
		{ID: 2, Intent: "", UpdatedAt: 200},
		{ID: 3, Intent: "", UpdatedAt: 100},
	}})
	m = mustModel(t, next)
	m = focusThreads(t, m)

	// Select thread 2 by moving down once from the default (first grouped
	// row, thread 1).
	next, _ = m.Update(keyMsg("j"))
	m = mustModel(t, next)
	if m.threadSelected != 2 {
		t.Fatalf("threadSelected = %d, want 2 after one down-move", m.threadSelected)
	}

	// Thread 2 is promoted to action-requested — it jumps to the TOP of the
	// grouped order — but the selection must follow it by ID, not stay
	// pinned to whatever index it used to occupy.
	next, _ = m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, Intent: "action-requested", UpdatedAt: 300},
		{ID: 2, Intent: "action-requested", UpdatedAt: 250},
		{ID: 3, Intent: "", UpdatedAt: 100},
	}})
	m = mustModel(t, next)
	if m.threadSelected != 2 {
		t.Fatalf("threadSelected = %d, want 2 (preserved across regroup)", m.threadSelected)
	}
}

// TestOpenStationAddressedThreadAcknowledgesOnce is the open-to-acknowledge
// test (spec §5, brief item 3): opening a thread addressed to station's own
// alias issues exactly ONE get_inbox, only on open — never on selection or
// focus, and never for a thread addressed to someone else.
func TestOpenStationAddressedThreadAcknowledgesOnce(t *testing.T) {
	var getInboxCalls int
	var getInboxAlias string
	fake := fakeCaller{fn: func(op string, args map[string]any) (json.RawMessage, error) {
		switch op {
		case "get_inbox":
			getInboxCalls++
			getInboxAlias, _ = args["alias"].(string)
			return json.RawMessage(`[]`), nil
		case "get_thread":
			return json.RawMessage(`{"thread":{},"entries":[]}`), nil
		}
		return json.RawMessage(`{}`), nil
	}}

	m := NewModel(fake, Options{Alias: "station"})
	next, _ := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, ToKind: "agent", ToTarget: "station", Intent: "action-requested", EntryCount: 1},
		{ID: 2, ToKind: "agent", ToTarget: "someone-else", Intent: "action-requested", EntryCount: 1},
	}})
	m = mustModel(t, next)
	m = focusThreads(t, m)

	// Focus alone (the two tabs above) must not have read anything.
	if getInboxCalls != 0 {
		t.Fatalf("focusing the threads pane issued %d get_inbox calls, want 0", getInboxCalls)
	}

	// Moving the selection (still thread 1, the default) must not read
	// anything either.
	next, _ = m.Update(keyMsg("k")) // no-op at the top, but exercises the path
	m = mustModel(t, next)
	if getInboxCalls != 0 {
		t.Fatalf("moving selection issued %d get_inbox calls, want 0", getInboxCalls)
	}

	// Opening thread 1 (addressed to station) must issue exactly one
	// get_inbox for station's alias.
	next, cmd := m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if cmd == nil {
		t.Fatalf("opening a thread must issue a Cmd (at least the get_thread fetch)")
	}
	for _, msg := range flattenCmds(cmd) {
		next, _ = m.Update(msg)
		m = mustModel(t, next)
	}
	if getInboxCalls != 1 {
		t.Fatalf("get_inbox calls after opening a station-addressed thread = %d, want exactly 1", getInboxCalls)
	}
	if getInboxAlias != "station" {
		t.Fatalf("get_inbox alias = %q, want %q", getInboxAlias, "station")
	}

	// Close and open the OTHER thread (addressed to someone else): no
	// additional get_inbox call.
	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	m.threadSelected = 2
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	for _, msg := range flattenCmds(cmd) {
		next, _ = m.Update(msg)
		m = mustModel(t, next)
	}
	if getInboxCalls != 1 {
		t.Fatalf("get_inbox calls after opening a non-station thread = %d, want still 1", getInboxCalls)
	}
}

// TestPaginationLazyLoadRequestsOffsetCorrectly is the pagination test (spec
// §5, brief item 2): the initial get_thread fetch requests the newest
// threadViewPageSize window (offset = entry_count - limit), and reaching the
// top of the loaded window (k/up at viewCursor==0) issues exactly one
// "load older" fetch for offset=0, limit=<the window's own offset> —
// prepending the result.
func TestPaginationLazyLoadRequestsOffsetCorrectly(t *testing.T) {
	var calls []map[string]any
	fake := fakeCaller{fn: func(op string, args map[string]any) (json.RawMessage, error) {
		if op != "get_thread" {
			return json.RawMessage(`{}`), nil
		}
		calls = append(calls, args)
		offset, _ := args["offset"].(int64)
		limit, _ := args["limit"].(int64)
		entries := make([]threadEntryRow, 0, limit)
		for i := int64(0); i < limit; i++ {
			id := offset + i
			entries = append(entries, threadEntryRow{ID: id, ThreadID: 1, FromAgent: "a", Body: "e"})
		}
		b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": entries})
		return b, nil
	}}

	m := NewModel(fake, Options{})
	next, _ := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, Intent: "", EntryCount: 250},
	}})
	m = mustModel(t, next)
	m = focusThreads(t, m)

	next, cmd := m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	for _, msg := range flattenCmds(cmd) {
		next, _ = m.Update(msg)
		m = mustModel(t, next)
	}
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 get_thread call on open, got %d: %+v", len(calls), calls)
	}
	if calls[0]["offset"] != int64(50) || calls[0]["limit"] != int64(200) {
		t.Fatalf("initial get_thread args = %+v, want offset=50 limit=200 (250 - 200)", calls[0])
	}
	if len(m.viewEntries) != 200 {
		t.Fatalf("loaded %d entries, want 200", len(m.viewEntries))
	}
	if m.viewOffset != 50 {
		t.Fatalf("viewOffset = %d, want 50", m.viewOffset)
	}
	if m.viewCursor != 0 {
		t.Fatalf("viewCursor = %d, want 0 right after opening", m.viewCursor)
	}

	// Reaching the top (viewCursor already 0) and pressing up again must
	// issue the "load older" fetch for exactly the missing window.
	calls = nil
	next, cmd = m.Update(keyMsg("k"))
	m = mustModel(t, next)
	if cmd == nil {
		t.Fatalf("k at the top of the loaded window must issue a load-older Cmd")
	}
	for _, msg := range flattenCmds(cmd) {
		next, _ = m.Update(msg)
		m = mustModel(t, next)
	}
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 load-older get_thread call, got %d: %+v", len(calls), calls)
	}
	if calls[0]["offset"] != int64(0) || calls[0]["limit"] != int64(50) {
		t.Fatalf("load-older args = %+v, want offset=0 limit=50", calls[0])
	}
	if len(m.viewEntries) != 250 {
		t.Fatalf("after load-older, loaded %d entries, want 250 (all of them)", len(m.viewEntries))
	}
	if m.viewOffset != 0 {
		t.Fatalf("viewOffset after load-older = %d, want 0 (nothing more above)", m.viewOffset)
	}
	if m.viewCursor != 50 {
		t.Fatalf("viewCursor after load-older = %d, want 50 (the previously-topmost entry kept its position)", m.viewCursor)
	}

	// Now at the true top (viewOffset==0): k must not issue another fetch.
	calls = nil
	next, cmd = m.Update(keyMsg("k"))
	m = mustModel(t, next)
	if cmd != nil {
		for _, msg := range flattenCmds(cmd) {
			if _, ok := msg.(threadPageMsg); ok {
				t.Fatalf("k at the true top issued an unexpected further get_thread fetch")
			}
		}
	}
	if len(calls) != 0 {
		t.Fatalf("k at the true top issued %d get_thread calls, want 0", len(calls))
	}
}

// TestFeedScrollLockHoldsAgainstNewEventsAndGResumesFollowing is the feed
// scroll-lock test deferred from Task 7 (spec §5 feed bullet): once the
// operator scrolls up, new events must not move the viewport, and G (or
// End) snaps back to live-follow.
func TestFeedScrollLockHoldsAgainstNewEventsAndGResumesFollowing(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(eventsMsg{page: render.EventsPage{Events: eventRowsAsc(1, 30), MaxID: 30}})
	m = mustModel(t, next)
	if !m.feedFollow {
		t.Fatalf("a fresh model must start in live-follow")
	}

	// Focus the feed and scroll up: this must drop out of live-follow.
	next, _ = m.Update(keyMsg("tab"))
	m = mustModel(t, next)
	if m.focus != paneFeed {
		t.Fatalf("focus = %v, want paneFeed after one tab", m.focus)
	}
	next, _ = m.Update(keyMsg("k"))
	m = mustModel(t, next)
	if m.feedFollow {
		t.Fatalf("scrolling up (k) must drop out of live-follow")
	}
	scrolledTop := m.feedWindowStart()

	// New events arriving must NOT move the scrolled-up viewport.
	next, _ = m.Update(eventsMsg{page: render.EventsPage{Events: eventRowsAsc(31, 5), MaxID: 35}})
	m = mustModel(t, next)
	if m.feedFollow {
		t.Fatalf("applying new events while scrolled up must not resume live-follow")
	}
	if got := m.feedWindowStart(); got != scrolledTop {
		t.Fatalf("feedWindowStart = %d after new events, want unchanged %d (scroll-lock)", got, scrolledTop)
	}

	// G resumes live-follow, snapping to the tail.
	next, _ = m.Update(keyMsg("G"))
	m = mustModel(t, next)
	if !m.feedFollow {
		t.Fatalf("G must resume live-follow")
	}
	wantTail := len(m.events) - defaultRows
	if wantTail < 0 {
		wantTail = 0
	}
	if got := m.feedWindowStart(); got != wantTail {
		t.Fatalf("feedWindowStart after G = %d, want %d (the live tail)", got, wantTail)
	}
}

// TestEscClosesThreadView checks the thread view overlay closes on Esc and
// stops owning keys once closed (Tab reaches the panes underneath again).
func TestEscClosesThreadView(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(threadsMsg{threads: []listThreadRow{{ID: 1, EntryCount: 1}}})
	m = mustModel(t, next)
	m = focusThreads(t, m)

	next, cmd := m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	for _, msg := range flattenCmds(cmd) {
		next, _ = m.Update(msg)
		m = mustModel(t, next)
	}
	if !m.viewOpen {
		t.Fatalf("expected the thread view to be open after Enter")
	}

	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	if m.viewOpen {
		t.Fatalf("Esc must close the thread view")
	}
}
