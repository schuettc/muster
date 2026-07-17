package station

import (
	"encoding/json"
	"strings"
	"testing"
)

// This file is spec iteration-5's own test suite: the operator-mail
// elevation (📬 header badge, the pinned FOR YOU section, 'm' jump) and the
// activity/attach enrichments (Tier 0/Tier 1).

// TestMailBadgeAlwaysVisibleDimWhenClearBrightWhenNot covers spec iteration-6
// item 4 ("📬 always visible in the header: dim '📬 0' when clear, bright
// '📬 N for you' when not") and item 5 (the header count and forYouRows()
// share the ONE canonical source, operatorInboxCount) — count comes from
// threads addressed directly to station's own alias, never get_inbox.
func TestMailBadgeAlwaysVisibleDimWhenClearBrightWhenNot(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "station"}, {Alias: "alpha-1", Project: "alpha"}}})
	m = mustModel(t, next)

	// No threads addressed to station yet: the badge is ALWAYS visible now,
	// showing the dim "📬 0" form rather than disappearing entirely.
	if !strings.Contains(m.renderBreadcrumb(), "📬 0") {
		t.Fatalf("badge must show '📬 0' with no unread mail, got:\n%s", m.renderBreadcrumb())
	}
	if m.operatorInboxCount() != 0 {
		t.Fatalf("operatorInboxCount = %d, want 0", m.operatorInboxCount())
	}

	next, _ = m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "alpha-1", Subject: "one", LastAt: 100, EntryCount: 1},
		{ID: 2, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "alpha-1", Subject: "two", LastAt: 200, EntryCount: 1},
		{ID: 3, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "alpha-1", Subject: "three", LastAt: 300, EntryCount: 1},
	}})
	m = mustModel(t, next)
	if !strings.Contains(m.renderBreadcrumb(), "📬 3 for you") {
		t.Fatalf("badge must show '📬 3 for you' with 3 unread, got:\n%s", m.renderBreadcrumb())
	}
	// Canonical count equality (spec item 5): the header's count and the FOR
	// YOU section's own row count must ALWAYS be the same number — they now
	// literally derive from the same forYouRows() call, so there is no
	// "header said 1, FOR YOU said 2" mismatch left to reproduce.
	if got, want := m.operatorInboxCount(), len(m.forYouRows()); got != want {
		t.Fatalf("operatorInboxCount()=%d != len(forYouRows())=%d — the header and FOR YOU section must never disagree", got, want)
	}
	if m.operatorInboxCount() != 3 {
		t.Fatalf("operatorInboxCount = %d, want 3", m.operatorInboxCount())
	}

	// Back to 0 (every thread acked): the badge returns to the dim "📬 0"
	// form rather than vanishing.
	m.ackedThreads = map[int64]bool{1: true, 2: true, 3: true}
	if !strings.Contains(m.renderBreadcrumb(), "📬 0") {
		t.Fatalf("badge must show '📬 0' once every thread is acked, got:\n%s", m.renderBreadcrumb())
	}
}

// forYouTestCaller builds a fakeCaller that counts get_inbox calls and
// answers get_thread with an empty page — the shape every FOR YOU test below
// needs (mirrors TestPreviewDoesNotAcknowledgeFocusDoes in nav_test.go).
func forYouTestCaller(getInboxCalls *int) fakeCaller {
	return fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "get_inbox":
			*getInboxCalls++
			return json.RawMessage(`[]`), nil
		case "get_thread":
			b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": []threadEntryRow{}, "total": 0})
			return b, nil
		}
		return json.RawMessage(`{}`), nil
	}}
}

// TestForYouSectionRendersAndEnterAcknowledges covers items 1a/1b: the
// pinned FOR YOU section lists station's unread thread (subject/from/age),
// never calls get_inbox on render/select, and opening it via Tab+Enter is
// the ONE acknowledge (exactly one get_inbox call), landing directly on the
// thread's reader.
func TestForYouSectionRendersAndEnterAcknowledges(t *testing.T) {
	var getInboxCalls int
	fake := forYouTestCaller(&getInboxCalls)

	m := NewModel(fake, Options{Alias: "station"})
	// Two distinct projects (station's own "" vs alpha-1's "alpha") keep L0
	// (screenProjects) from auto-skipping, so the FOR YOU section stays
	// reachable to test directly.
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "station", Project: "", Unread: 1},
		{Alias: "alpha-1", Project: "alpha", Live: true},
	}})
	m = mustModel(t, next)
	if m.screen != screenProjects {
		t.Fatalf("setup: two distinct projects must keep L0 active, got screen=%v", m.screen)
	}

	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 7, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "alpha-1", Subject: "need review", LastAt: 1000, EntryCount: 1},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if getInboxCalls != 0 {
		t.Fatalf("rendering/selecting FOR YOU must never call get_inbox before Enter, got %d", getInboxCalls)
	}
	if m.forYou != 7 {
		t.Fatalf("FOR YOU selection should default to the only row (7), got %d", m.forYou)
	}

	view := m.View()
	if !strings.Contains(view, "FOR YOU") {
		t.Fatalf("expected the FOR YOU section title, got:\n%s", view)
	}
	if !strings.Contains(view, "need review") {
		t.Fatalf("expected the FOR YOU section listing the thread's subject, got:\n%s", view)
	}

	next, _ = m.Update(keyMsg("tab"))
	m = mustModel(t, next)
	if m.focus != focusForYou {
		t.Fatalf("tab at L0 with unread mail must focus the FOR YOU section, got focus=%v", m.focus)
	}

	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if getInboxCalls != 1 {
		t.Fatalf("opening a FOR YOU thread must call get_inbox exactly once, got %d", getInboxCalls)
	}
	if m.screen != screenProject || m.focus != focusConvRight || m.conversation != 7 {
		t.Fatalf("expected the thread opened directly (screenProject/focusConvRight/conversation=7), got screen=%v focus=%v conversation=%d", m.screen, m.focus, m.conversation)
	}

	// Re-opening (e.g. climbing out and back via 'm') must not re-acknowledge.
	next, mCmd := m.Update(keyMsg("m"))
	m = mustModel(t, next)
	m = drainCmd(t, m, mCmd)
	if getInboxCalls != 1 {
		t.Fatalf("re-opening the SAME already-acked thread must not re-acknowledge, got %d", getInboxCalls)
	}
}

// TestMailJumpOpensSingleThreadFromDeepLevel covers item 1c: 'm' from ANY
// level (here, deep at screenProject) jumps straight into the single unread
// thread when there is exactly one, acknowledging it exactly once.
func TestMailJumpOpensSingleThreadFromDeepLevel(t *testing.T) {
	var getInboxCalls int
	fake := forYouTestCaller(&getInboxCalls)

	m := NewModel(fake, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "station", Project: "", Unread: 1},
		{Alias: "alpha-1", Project: "alpha", Live: true},
	}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 9, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "alpha-1", Subject: "single unread", LastAt: 500, EntryCount: 1},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)

	// Drill down to a deep level (screenProject) before pressing 'm'.
	next, _ = m.Update(keyMsg("enter")) // L0 -> L1 (screenProject)
	m = mustModel(t, next)
	if m.screen != screenProject {
		t.Fatalf("setup: expected screenProject after drilling in, got %v", m.screen)
	}

	next, cmd = m.Update(keyMsg("m"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if getInboxCalls != 1 {
		t.Fatalf("'m' with exactly one unread thread must acknowledge it, got %d get_inbox calls", getInboxCalls)
	}
	if m.screen != screenProject || m.focus != focusConvRight || m.conversation != 9 {
		t.Fatalf("'m' must open the single unread thread directly, got screen=%v focus=%v conversation=%d", m.screen, m.focus, m.conversation)
	}
}

// TestMailJumpWithMultipleRowsFocusesSection covers item 1c's other branch:
// 'm' with more than one unread thread jumps to the FOR YOU section itself
// (screenProjects/focusForYou) rather than guessing which one to open.
func TestMailJumpWithMultipleRowsFocusesSection(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "station", Project: "", Unread: 2},
		{Alias: "alpha-1", Project: "alpha", Live: true},
	}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "alpha-1", Subject: "one", LastAt: 100, EntryCount: 1},
		{ID: 2, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "alpha-1", Subject: "two", LastAt: 200, EntryCount: 1},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)

	next, _ = m.Update(keyMsg("enter")) // drill into L1 first — 'm' must reach across levels
	m = mustModel(t, next)

	next, _ = m.Update(keyMsg("m"))
	m = mustModel(t, next)
	if m.screen != screenProjects || m.focus != focusForYou {
		t.Fatalf("'m' with 2 unread threads must focus the FOR YOU section, got screen=%v focus=%v", m.screen, m.focus)
	}
}

// TestMailJumpNoOpWithoutUnread covers 'm' when station has no unread mail —
// it must not navigate anywhere, only note the status.
func TestMailJumpNoOpWithoutUnread(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{Alias: "station"})
	// Two distinct projects keep L0 from auto-skipping, so the assertion
	// below actually exercises "stayed put" rather than an unrelated
	// single-project auto-skip.
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "station", Project: "", Unread: 0},
		{Alias: "alpha-1", Project: "alpha", Live: true},
	}})
	m = mustModel(t, next)

	next, _ = m.Update(keyMsg("m"))
	m = mustModel(t, next)
	if m.screen != screenProjects || m.focus != focusProjectList {
		t.Fatalf("'m' with no unread mail must not navigate, got screen=%v focus=%v", m.screen, m.focus)
	}
	if !strings.Contains(m.status, "no mail") {
		t.Fatalf("expected a status note, got %q", m.status)
	}
}

// TestLastActiveRendersOnAgentStripAndActivityHeader covers Tier 0a: once
// lastActiveMsg has resolved an alias, its "last active: <relative>" text
// shows on the roster/strip row AND the agent page's ACTIVITY header — fed
// directly (bypassing the fetch) since the model's cache is the only thing
// rendering depends on.
func TestLastActiveRendersOnAgentStripAndActivityHeader(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "alpha-1", Project: "alpha", Live: true}}})
	m = mustModel(t, next)
	m.pollGen = 5
	next, _ = m.Update(lastActiveMsg{alias: "alpha-1", ts: 1000, gen: 5})
	m = mustModel(t, next)
	if m.lastActive["alpha-1"] != 1000 {
		t.Fatalf("expected lastActive[alpha-1]=1000, got %v", m.lastActive)
	}

	stripLine := m.renderRosterLine("  ", m.agents[0], 80)
	if !strings.Contains(stripLine, "last active:") {
		t.Fatalf("agent strip row must show 'last active:', got %q", stripLine)
	}

	// A stale-generation msg must not apply.
	next, _ = m.Update(lastActiveMsg{alias: "alpha-1", ts: 999999, gen: 1})
	m = mustModel(t, next)
	if m.lastActive["alpha-1"] != 1000 {
		t.Fatalf("a stale-gen lastActiveMsg must not overwrite the cache, got %v", m.lastActive["alpha-1"])
	}

	// Agent page (screenAgent) header shows it too.
	m.screen = screenAgent
	m.agent = "alpha-1"
	activityView := m.renderActivityBox(80, 20)
	if !strings.Contains(activityView, "last active") {
		t.Fatalf("agent page ACTIVITY header must show last-active, got:\n%s", activityView)
	}
}

// TestUnreadAgeRendersOnRosterAndProjectRows covers Tier 0b: an unread
// count's row gains "(N · age)" — the oldest waiting thread's age — derived
// client-side from already-fetched thread data (never get_inbox).
func TestUnreadAgeRendersOnRosterAndProjectRows(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	// SocketPath/SessionID must be set: computeProjectSummaries only rolls
	// an agent's Unread into its project's total for a resolvable session
	// tuple (see nav.go's computeProjectSummaries).
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "alpha-1", Project: "alpha", Live: true, Unread: 2, SocketPath: "/s", SessionID: "$1"},
	}})
	m = mustModel(t, next)
	next, _ = m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "other", ToKind: "agent", ToTarget: "alpha-1", LastFrom: "other", LastAt: 1},
	}})
	m = mustModel(t, next)

	stripLine := m.renderRosterLine("  ", m.agents[0], 80)
	if !strings.Contains(stripLine, "(2") || !strings.Contains(stripLine, " · ") {
		t.Fatalf("roster row must show the unread age suffix '(2 · age)', got %q", stripLine)
	}

	rows := m.projectRows()
	idx := keyIndex(rows, projKey, "alpha")
	if idx < 0 {
		t.Fatalf("expected an 'alpha' project row, got %+v", rows)
	}
	projLine := m.renderProjectLine("  ", rows[idx], 80)
	if !strings.Contains(projLine, " · ") {
		t.Fatalf("project rollup row must show the unread age suffix, got %q", projLine)
	}
}

// TestEyeMarkerRemovedEverywhere covers spec iteration-6 item 1: "remove the
// attach/eye marker everywhere in station UI (roster rows, agent pages, help
// legend)" — internal/tmuxenv.SessionAttached and its own test stay in
// place (unused-by-station is fine per the task brief), but nothing in
// station's rendering may reference it or show the 👁 glyph any more.
func TestEyeMarkerRemovedEverywhere(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "alpha-1", Project: "alpha", Live: true},
		{Alias: "alpha-2", Project: "alpha", Live: true},
	}})
	m = mustModel(t, next)

	idx := keyIndex(m.agents, agentKey, "alpha-1")
	if strings.Contains(m.renderRosterLine("  ", m.agents[idx], 80), "👁") {
		t.Fatalf("roster rows must never show the 👁 marker any more")
	}

	m.screen = screenAgent
	m.agent = "alpha-1"
	if strings.Contains(m.renderBreadcrumb(), "👁") {
		t.Fatalf("the agent page header must never show the 👁 marker any more, got:\n%s", m.renderBreadcrumb())
	}

	help := m
	help.helpOpen = true
	if strings.Contains(help.View(), "👁") {
		t.Fatalf("the help overlay must never mention the 👁 marker any more:\n%s", help.View())
	}
}
