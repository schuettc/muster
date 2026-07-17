package station

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// focusConversationList drives m to screenProject with a single default
// project (registering one agent auto-skips L0, landing on the merged
// list's AGENTS section — spec iteration-4: "agents first") — the entry
// point every conversation-list test below needs before it can
// select/open a conversation via enterThreadsSection once its own threadsMsg
// has landed. Mirrors the pre-redesign focusThreads helper's role.
func focusConversationList(t *testing.T, m Model, agentAlias string) Model {
	t.Helper()
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: agentAlias}}})
	m = mustModel(t, next)
	if m.screen != screenProject || m.focus != focusProjectItems || m.l1Section != l1SectionAgents {
		t.Fatalf("setup: expected screenProject/focusProjectItems/AGENTS after registering one agent, got screen=%v focus=%v section=%v", m.screen, m.focus, m.l1Section)
	}
	return m
}

// TestConversationSelectionStableAcrossRegroup is the selection-stability
// test (spec §5 carried-over fix): a conversation that moves between intent
// buckets across a poll must keep the SAME conversation selected by ID — a
// poll must never jump the operator's cursor.
func TestConversationSelectionStableAcrossRegroup(t *testing.T) {
	m := focusConversationList(t, NewModel(fakeCaller{}, Options{}), "a")
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "a", Intent: "action-requested", UpdatedAt: 300},
		{ID: 2, FromAgent: "a", Intent: "", UpdatedAt: 200},
		{ID: 3, FromAgent: "a", Intent: "", UpdatedAt: 100},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	m = enterThreadsSection(t, m) // cross into THREADS; lands on the default (first grouped row, conversation 1)

	// Select conversation 2 by moving down once from the default (first
	// grouped row, conversation 1).
	next, cmd = m.Update(keyMsg("j"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.conversation != 2 {
		t.Fatalf("conversation = %d, want 2 after one down-move", m.conversation)
	}

	// Conversation 2 is promoted to action-requested — it jumps to the TOP
	// of the grouped order — but the selection must follow it by ID.
	next, cmd = m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "a", Intent: "action-requested", UpdatedAt: 300},
		{ID: 2, FromAgent: "a", Intent: "action-requested", UpdatedAt: 250},
		{ID: 3, FromAgent: "a", Intent: "", UpdatedAt: 100},
	}})
	m = mustModel(t, next)
	_ = drainCmd(t, m, cmd)
	if m.conversation != 2 {
		t.Fatalf("conversation = %d, want 2 (preserved across regroup)", m.conversation)
	}
}

// TestPaginationLazyLoadRequestsOffsetCorrectly is the pagination test (spec
// §5, carried over): the initial get_thread fetch requests the newest
// threadViewPageSize window (offset = entry_count - limit), and reaching the
// top of the loaded window (k/up at viewCursor==0, once FOCUSED) issues
// exactly one "load older" fetch for offset=0, limit=<the window's own
// offset> — prepending the result.
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

	m := focusConversationList(t, NewModel(fake, Options{}), "a")
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "a", Intent: "", EntryCount: 250},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd) // auto-selected conversation's preview fetch (the "initial" fetch below)
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 get_thread call on selection, got %d: %+v", len(calls), calls)
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
	m = enterThreadsSection(t, m) // cross into THREADS; the already-loaded preview means no extra get_thread call

	// Focus the conversation (L2) before load-older applies (k/up only
	// lazily loads while FOCUSED — scrollConversation).
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.focus != focusConvRight {
		t.Fatalf("setup: expected the conversation focused")
	}
	if m.viewCursor != 0 {
		t.Fatalf("viewCursor = %d, want 0 right after focusing", m.viewCursor)
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

// TestConversationReaderWindowingBoundsLineCountAndCursorAtBottomShowsNewest
// is the render-windowing carried-over fix (spec §5): a 200-entry
// conversation's focused reader must stay bounded by the pane height rather
// than dumping every wrapped entry, and moving the cursor to the bottom must
// bring the newest entry into view.
func TestConversationReaderWindowingBoundsLineCountAndCursorAtBottomShowsNewest(t *testing.T) {
	const n = 200
	entries := make([]threadEntryRow, n)
	for i := 0; i < n; i++ {
		entries[i] = threadEntryRow{ID: int64(i), ThreadID: 1, FromAgent: "a", Body: fmt.Sprintf("entry-%d", i), CreatedAt: int64(i)}
	}
	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op != "get_thread" {
			return json.RawMessage(`{}`), nil
		}
		b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": entries, "total": n})
		return b, nil
	}}

	m := focusConversationList(t, NewModel(fake, Options{}), "a")
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{{ID: 1, FromAgent: "a", EntryCount: n}}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if len(m.viewEntries) != n {
		t.Fatalf("loaded %d entries, want %d", len(m.viewEntries), n)
	}
	m = enterThreadsSection(t, m)
	next, _ = m.Update(keyMsg("enter")) // focus the reader (L2)
	m = mustModel(t, next)

	// Move the cursor to the very bottom (the newest loaded entry) and check
	// the rendered view is both bounded and actually shows it.
	m.viewCursor = n - 1
	view := m.renderConversationBox(80, conversationReaderRows+boxBorderRows, true)
	if lineCount := strings.Count(view, "\n") + 1; lineCount > conversationReaderRows+10 {
		t.Fatalf("focused reader produced %d lines, want it bounded by conversationReaderRows (%d) plus a small fixed overhead:\n%s", lineCount, conversationReaderRows, view)
	}
	if !strings.Contains(view, fmt.Sprintf("entry-%d", n-1)) {
		t.Fatalf("cursor at the bottom must show the newest entry:\n%s", view)
	}
	if strings.Contains(view, "entry-0") {
		t.Fatalf("windowing must not show entries far from the cursor:\n%s", view)
	}
}

// TestThreadPageMsgStaleGenerationDiscarded is the threadPageMsg-staleness
// carried-over fix (spec §5): a page left over from a PREVIOUS load of the
// SAME thread ID (an older viewGen) must be dropped even though
// msg.threadID still matches what's loaded.
func TestThreadPageMsgStaleGenerationDiscarded(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	m.viewThreadID = 1
	m.viewGen = 2 // this thread has been (re)loaded once already
	m.viewEntries = []threadEntryRow{{ID: 99, Body: "current"}}

	next, cmd := m.Update(threadPageMsg{threadID: 1, gen: 1, entries: []threadEntryRow{{ID: 1, Body: "stale"}}, total: 1})
	m2 := mustModel(t, next)
	if cmd != nil {
		t.Fatalf("discarding a stale-generation page must not issue a Cmd")
	}
	if len(m2.viewEntries) != 1 || m2.viewEntries[0].Body != "current" {
		t.Fatalf("a stale gen-1 page must not apply, got %+v", m2.viewEntries)
	}
}

// TestGetThreadLiveTotalSelfCorrectsStaleCachedEntryCount is the
// newest-entries-gap carried-over fix (spec §5): a stale cached entry_count
// (from list_threads) makes the initial offset guess too low; get_thread's
// live `total` on the response exposes the gap, and station issues exactly
// ONE corrective re-fetch to land on the true tail.
func TestGetThreadLiveTotalSelfCorrectsStaleCachedEntryCount(t *testing.T) {
	const liveTotal = 300 // the list_threads snapshot's cached entry_count (100) is stale
	var calls []map[string]any
	fake := fakeCaller{fn: func(op string, args map[string]any) (json.RawMessage, error) {
		if op != "get_thread" {
			return json.RawMessage(`{}`), nil
		}
		calls = append(calls, args)
		offset, _ := args["offset"].(int64)
		limit, _ := args["limit"].(int64)
		var entries []threadEntryRow
		for i := int64(0); i < limit && offset+i < liveTotal; i++ {
			entries = append(entries, threadEntryRow{ID: offset + i, ThreadID: 1, Body: "e"})
		}
		b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": entries, "total": liveTotal})
		return b, nil
	}}

	m := focusConversationList(t, NewModel(fake, Options{}), "a")
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{{ID: 1, FromAgent: "a", EntryCount: 100}}}) // stale cached entry_count
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)

	if len(calls) != 2 {
		t.Fatalf("expected an initial fetch + one self-correction fetch, got %d: %+v", len(calls), calls)
	}
	if calls[0]["offset"] != int64(0) {
		t.Fatalf("initial (stale-guess) offset = %v, want 0 (100 - 200 clamped)", calls[0]["offset"])
	}
	if calls[1]["offset"] != int64(100) {
		t.Fatalf("corrected offset = %v, want 100 (the live total 300 - limit 200)", calls[1]["offset"])
	}
	if m.viewTotal != liveTotal {
		t.Fatalf("viewTotal = %d, want %d (the live total)", m.viewTotal, liveTotal)
	}
	if len(m.viewEntries) != 200 {
		t.Fatalf("after self-correction, loaded %d entries, want 200 (the true tail)", len(m.viewEntries))
	}
	if got := m.viewEntries[len(m.viewEntries)-1].ID; got != liveTotal-1 {
		t.Fatalf("last loaded entry ID = %d, want %d (the true newest)", got, liveTotal-1)
	}
}

// TestViewNewerCountIndicatorAndGFetchesTail is the newest-entries-gap
// carried-over fix's ONGOING case (spec §5): once a conversation is FOCUSED
// and showing what was, at load time, genuinely the tail, NEW entries
// arriving afterward (surfaced by the ordinary list_threads poll bumping
// this thread's entry_count) must show a "N newer — press G" indicator, and
// G fetches the tail.
func TestViewNewerCountIndicatorAndGFetchesTail(t *testing.T) {
	liveTotal := int64(100)
	var calls []map[string]any
	fake := fakeCaller{fn: func(op string, args map[string]any) (json.RawMessage, error) {
		if op != "get_thread" {
			return json.RawMessage(`{}`), nil
		}
		calls = append(calls, args)
		offset, _ := args["offset"].(int64)
		limit, _ := args["limit"].(int64)
		var entries []threadEntryRow
		for i := int64(0); i < limit && offset+i < liveTotal; i++ {
			entries = append(entries, threadEntryRow{ID: offset + i, ThreadID: 1, Body: "e"})
		}
		b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": entries, "total": liveTotal})
		return b, nil
	}}

	m := focusConversationList(t, NewModel(fake, Options{}), "a")
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{{ID: 1, FromAgent: "a", EntryCount: 100}}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 get_thread call on selection (guess matched live total), got %d: %+v", len(calls), calls)
	}
	m = enterThreadsSection(t, m)
	next, _ = m.Update(keyMsg("enter")) // focus (L2)
	m = mustModel(t, next)
	if m.viewTotal != 100 || m.viewNewerCount() != 0 {
		t.Fatalf("viewTotal=%d viewNewerCount=%d, want 100/0 before anything new arrives", m.viewTotal, m.viewNewerCount())
	}

	// New replies arrive; the ordinary list_threads poll refreshes the
	// thread's entry_count while the reader stays FOCUSED — no get_thread
	// fetch happens on its own (a focused reader's scroll position is never
	// disturbed by a tick; only End/G explicitly fetches the tail).
	liveTotal = 105
	calls = nil
	next, _ = m.Update(threadsMsg{threads: []listThreadRow{{ID: 1, FromAgent: "a", EntryCount: 105}}})
	m = mustModel(t, next)
	if len(calls) != 0 {
		t.Fatalf("a tick refresh while FOCUSED must not auto-fetch, got %d get_thread calls", len(calls))
	}
	if n := m.viewNewerCount(); n != 5 {
		t.Fatalf("viewNewerCount = %d, want 5 (105 - the loaded total of 100)", n)
	}
	if got := m.renderConversationBox(80, conversationReaderRows+boxBorderRows, true); !strings.Contains(got, "5 newer") {
		t.Fatalf("focused reader must show the newer-entries indicator:\n%s", got)
	}

	calls = nil
	next, cmd = m.Update(keyMsg("end"))
	m = mustModel(t, next)
	if cmd == nil {
		t.Fatalf("G/End with newer entries pending must issue a tail-fetch Cmd")
	}
	for _, msg := range flattenCmds(cmd) {
		next, _ = m.Update(msg)
		m = mustModel(t, next)
	}
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 tail-fetch get_thread call, got %d: %+v", len(calls), calls)
	}
	if m.viewTotal != 105 || m.viewNewerCount() != 0 {
		t.Fatalf("after the tail fetch, viewTotal=%d viewNewerCount=%d, want 105/0", m.viewTotal, m.viewNewerCount())
	}
	if len(m.viewEntries) != 105 {
		t.Fatalf("after the tail fetch, loaded %d entries, want 105 (all of them)", len(m.viewEntries))
	}
}

// TestEscUnfocusesConversationReader checks the reader un-focuses on Esc and
// stops owning keys once unfocused (Tab reaches the lists underneath
// again).
func TestEscUnfocusesConversationReader(t *testing.T) {
	m := focusConversationList(t, NewModel(fakeCaller{}, Options{}), "a")
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{{ID: 1, FromAgent: "a", EntryCount: 1}}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	m = enterThreadsSection(t, m)

	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.focus != focusConvRight {
		t.Fatalf("expected the conversation focused after Enter")
	}

	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	if m.focus != focusProjectItems || m.l1Section != l1SectionThreads {
		t.Fatalf("Esc must un-focus the reader back to focusProjectItems/THREADS, got focus=%v section=%v", m.focus, m.l1Section)
	}
}
