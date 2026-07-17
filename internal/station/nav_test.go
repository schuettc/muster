package station

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// This file is the IA-redesign's own test suite (spec §5-REVISED): the
// project-first two-column drill-down's navigation state machine, project
// rollup math, cross-project marking, single-project auto-skip, narrow-mode
// rendering, preview-vs-focus-acknowledge, and the '?' help overlay.

// twoProjectAgents seeds a roster spanning two projects, one agent each.
func twoProjectAgents() []agentEnriched {
	return []agentEnriched{
		{Alias: "alpha-1", Project: "alpha", Live: true},
		{Alias: "beta-1", Project: "beta", Live: true},
	}
}

// TestNavigationDrillAndClimbTransitions exercises the full stack: L0 → L1
// (agent strip / conversations) → L1.5 (agent) and back, verifying Enter
// drills, Esc climbs, Tab cycles the current screen's sub-targets, and each
// level's selection survives a climb-and-return (spec §5-REVISED "per-level
// selection").
func TestNavigationDrillAndClimbTransitions(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: twoProjectAgents()})
	m = mustModel(t, next)
	if m.screen != screenProjects {
		t.Fatalf("two projects must NOT auto-skip L0, got screen=%v", m.screen)
	}
	if m.project != "alpha" {
		t.Fatalf("default L0 selection = %q, want alpha (sorted first)", m.project)
	}

	// Move to "beta" and drill in.
	next, cmd := m.Update(keyMsg("j"))
	m = mustModel(t, next)
	if cmd != nil {
		t.Fatalf("moving the L0 selection must not issue a Cmd")
	}
	if m.project != "beta" {
		t.Fatalf("project after j = %q, want beta", m.project)
	}
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.screen != screenProject || m.focus != focusConvList {
		t.Fatalf("Enter on a project must drill to screenProject/focusConvList, got screen=%v focus=%v", m.screen, m.focus)
	}

	// Tab cycles: focusConvList -> focusConvRight -> focusAgentStrip -> focusConvList.
	next, _ = m.Update(keyMsg("tab"))
	m = mustModel(t, next)
	if m.focus != focusConvRight {
		t.Fatalf("focus after 1st tab = %v, want focusConvRight", m.focus)
	}
	next, _ = m.Update(keyMsg("tab"))
	m = mustModel(t, next)
	if m.focus != focusAgentStrip {
		t.Fatalf("focus after 2nd tab = %v, want focusAgentStrip", m.focus)
	}

	// Drill into the agent strip's selected agent (beta-1, the only agent in
	// project beta).
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.screen != screenAgent || m.agent != "beta-1" {
		t.Fatalf("Enter on the agent strip must drill to screenAgent for beta-1, got screen=%v agent=%q", m.screen, m.agent)
	}

	// Esc climbs back to screenProject/focusAgentStrip, then to screenProjects.
	m = mustModel(t, m.handleEscKey())
	if m.screen != screenProject || m.focus != focusAgentStrip {
		t.Fatalf("first Esc must climb to screenProject/focusAgentStrip, got screen=%v focus=%v", m.screen, m.focus)
	}
	if m.project != "beta" {
		t.Fatalf("climbing must preserve the per-level project selection, got %q", m.project)
	}
	m = mustModel(t, m.handleEscKey())
	if m.screen != screenProjects || m.focus != focusProjectList {
		t.Fatalf("second Esc must climb all the way to screenProjects, got screen=%v focus=%v", m.screen, m.focus)
	}
	if m.project != "beta" {
		t.Fatalf("L0's own selection must still be beta after climbing back to it, got %q", m.project)
	}

	// A third Esc at the top of the stack is a no-op.
	before := m
	after := mustModel(t, before.handleEscKey())
	if after.screen != before.screen || after.focus != before.focus {
		t.Fatalf("Esc at screenProjects must be a no-op")
	}
}

// TestSingleProjectAutoSkipsL0 covers spec §5-REVISED: "Auto-skipped when
// the bus has exactly one project (non-dotfiles users land at L1 —design
// must not assume the proj-socket convention)".
func TestSingleProjectAutoSkipsL0(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "solo-1", Project: "onlyproj", Live: true},
		{Alias: "solo-2", Project: "onlyproj", Live: false},
	}})
	m = mustModel(t, next)
	if m.screen != screenProject {
		t.Fatalf("a single-project bus must auto-skip L0, got screen=%v", m.screen)
	}
	if m.project != "onlyproj" {
		t.Fatalf("auto-skip must select the one project, got %q", m.project)
	}
	if !m.singleProject {
		t.Fatalf("singleProject must be true")
	}

	// Esc must be a no-op — there is no L0 to climb back to.
	after := mustModel(t, m.handleEscKey())
	if after.screen != screenProject {
		t.Fatalf("Esc on a single-project bus must not climb to screenProjects, got %v", after.screen)
	}
}

// TestProjectRollupMath is computeProjectSummaries' own unit test (spec
// §5-REVISED L0: "unread rollup (+action)"): sibling aliases of the SAME
// session tuple must not double-count that session's unread — mirroring
// spec §3's "no summing of per-alias counts" principle — while distinct
// sessions in the same project DO sum.
func TestProjectRollupMath(t *testing.T) {
	agents := []agentEnriched{
		{Alias: "a-session-name", Project: "muster", SocketPath: "/s", SessionID: "$1", Unread: 3, ActionCount: 1},
		{Alias: "a-chosen-alias", Project: "muster", SocketPath: "/s", SessionID: "$1", Unread: 3, ActionCount: 1}, // SAME session tuple as above
		{Alias: "b", Project: "muster", SocketPath: "/s", SessionID: "$2", Unread: 2, ActionCount: 0, Live: true},
		{Alias: "c", Project: "other", SocketPath: "/s", SessionID: "$3", Unread: 5, ActionCount: 5},
	}
	summaries := computeProjectSummaries(agents, nil)
	var muster, other projectSummary
	for _, s := range summaries {
		switch s.Name {
		case "muster":
			muster = s
		case "other":
			other = s
		}
	}
	if muster.Total != 3 {
		t.Fatalf("muster.Total = %d, want 3 (three agents)", muster.Total)
	}
	if muster.Live != 1 {
		t.Fatalf("muster.Live = %d, want 1", muster.Live)
	}
	if muster.Unread != 5 {
		t.Fatalf("muster.Unread = %d, want 5 (3 from the ONE shared session + 2 from the other) — got double-counted if 8", muster.Unread)
	}
	if muster.ActionUnread != 1 {
		t.Fatalf("muster.ActionUnread = %d, want 1 (deduped session's action count, not doubled)", muster.ActionUnread)
	}
	if other.Unread != 5 || other.ActionUnread != 5 {
		t.Fatalf("other project rollup = %+v, want Unread=5 ActionUnread=5", other)
	}
}

// TestCrossProjectMarkerAppearsInBothProjects covers spec §5-REVISED: "a
// thread belongs to every project having a participant; cross-project
// threads marked '↔ otherproj' and shown in both projects".
func TestCrossProjectMarkerAppearsInBothProjects(t *testing.T) {
	agents := []agentEnriched{
		{Alias: "alpha-1", Project: "alpha"},
		{Alias: "beta-1", Project: "beta"},
	}
	aliasProj := aliasProjectMap(agents)
	threads := []listThreadRow{
		{ID: 1, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "beta-1"},
	}

	alphaRows := conversationsForProject(threads, aliasProj, "alpha")
	if len(alphaRows) != 1 {
		t.Fatalf("expected thread 1 in alpha's conversation list, got %d rows", len(alphaRows))
	}
	if len(alphaRows[0].OtherProjects) != 1 || alphaRows[0].OtherProjects[0] != "beta" {
		t.Fatalf("alpha's row must mark beta as the other project, got %+v", alphaRows[0].OtherProjects)
	}

	betaRows := conversationsForProject(threads, aliasProj, "beta")
	if len(betaRows) != 1 {
		t.Fatalf("expected thread 1 in beta's conversation list too, got %d rows", len(betaRows))
	}
	if len(betaRows[0].OtherProjects) != 1 || betaRows[0].OtherProjects[0] != "alpha" {
		t.Fatalf("beta's row must mark alpha as the other project, got %+v", betaRows[0].OtherProjects)
	}

	// The rendered line carries the "↔" marker.
	m := NewModel(fakeCaller{}, Options{})
	line := m.renderConversationLine(alphaRows[0], 200)
	if !strings.Contains(line, "↔ beta") {
		t.Fatalf("rendered conversation line missing the cross-project marker, got %q", line)
	}

	// A single-project thread carries no marker at all.
	single := conversationsForProject([]listThreadRow{{ID: 2, FromAgent: "alpha-1", ToKind: "broadcast"}}, aliasProj, "alpha")
	if len(single) != 1 || len(single[0].OtherProjects) != 0 {
		t.Fatalf("a single-project thread must carry no OtherProjects, got %+v", single)
	}
}

// TestNarrowModeSingleColumnSwapsOnFocus covers spec §5-REVISED: "Narrow
// terminals (< ~110 cols): single-column mode — Enter swaps list→detail,
// Esc back." Narrow rendering is derived purely from focus (see
// renderBody), so this exercises the SAME Enter/Esc transitions as wide
// mode and checks the rendered body shows only one side at a time.
func TestNarrowModeSingleColumnSwapsOnFocus(t *testing.T) {
	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "get_thread" {
			b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": []threadEntryRow{{ID: 1, FromAgent: "alpha-1", Body: "hello there"}}, "total": 1})
			return b, nil
		}
		return json.RawMessage(`{}`), nil
	}}
	m := NewModel(fake, Options{})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 40}) // < narrowWidthThreshold (110)
	m = mustModel(t, next)
	dims := m.layout()
	if !dims.narrow {
		t.Fatalf("90 cols must trigger narrow mode")
	}

	next, _ = m.Update(agentsMsg{rows: []agentEnriched{{Alias: "alpha-1", Project: "alpha", Live: true}}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 5, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "alpha-1", Subject: "narrow test", EntryCount: 1},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenProject || m.focus != focusConvList {
		t.Fatalf("a single-project bus must auto-skip straight to screenProject/focusConvList, got screen=%v focus=%v", m.screen, m.focus)
	}

	// Not focused-right: the body must show the LIST (conversation subject),
	// not the detail reader.
	view := m.renderBody(m.layout())
	if !strings.Contains(view, "narrow test") {
		t.Fatalf("narrow list view must show the conversation list:\n%s", view)
	}
	if strings.Contains(view, "hello there") {
		t.Fatalf("narrow list view must NOT show the detail reader's body:\n%s", view)
	}

	// Enter focuses the conversation (detail) — body swaps to the reader.
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.focus != focusConvRight {
		t.Fatalf("Enter on the conversation list must focus the reader")
	}
	view = m.renderBody(m.layout())
	if !strings.Contains(view, "hello there") {
		t.Fatalf("narrow detail view must show the focused conversation's body:\n%s", view)
	}
	if strings.Contains(view, "narrow test") {
		t.Fatalf("narrow detail view must NOT also show the list's subject text:\n%s", view)
	}

	// Esc swaps back to the list.
	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	if m.focus != focusConvList {
		t.Fatalf("Esc must swap back to the list (focusConvList)")
	}
}

// TestPreviewDoesNotAcknowledgeFocusDoes is the open-to-acknowledge test
// (spec §5-REVISED: "preview of a station-addressed thread does NOT
// acknowledge; only Enter-focusing it does, keep that explicit"). Selecting
// a conversation (which fetches its preview) must never call get_inbox;
// Enter-focusing it must call get_inbox EXACTLY once, even across repeated
// focus/unfocus of the SAME thread.
func TestPreviewDoesNotAcknowledgeFocusDoes(t *testing.T) {
	var getInboxCalls int
	var getThreadCalls int
	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "get_inbox":
			getInboxCalls++
			return json.RawMessage(`[]`), nil
		case "get_thread":
			getThreadCalls++
			b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": []threadEntryRow{}, "total": 0})
			return b, nil
		}
		return json.RawMessage(`{}`), nil
	}}

	m := NewModel(fake, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "alpha-1", Project: "alpha"}}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", Intent: "action-requested", EntryCount: 1},
		{ID: 2, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "someone-else", Intent: "action-requested", EntryCount: 1},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd) // the auto-selected thread's preview fetch — no ack yet
	if getInboxCalls != 0 {
		t.Fatalf("the PREVIEW fetch (selection, no Enter) must never call get_inbox, got %d calls", getInboxCalls)
	}
	if getThreadCalls == 0 {
		t.Fatalf("selecting a conversation must still fetch its preview via get_thread")
	}
	if m.screen != screenProject || m.focus != focusConvList {
		t.Fatalf("single-project auto-skip must already have landed at screenProject/focusConvList, got screen=%v focus=%v", m.screen, m.focus)
	}

	// Enter-focusing thread 1 (addressed to station) must ack exactly once.
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if getInboxCalls != 1 {
		t.Fatalf("get_inbox calls after focusing a station-addressed thread = %d, want exactly 1", getInboxCalls)
	}

	// Un-focus and re-focus the SAME thread: must not ack again.
	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if getInboxCalls != 1 {
		t.Fatalf("re-focusing the SAME thread must not re-acknowledge, got %d calls", getInboxCalls)
	}
}

// TestHelpOverlayRendersLegendAndClosesOnAnyKey covers spec §5-REVISED: "'?'
// help overlay: keys + glyph legend".
func TestHelpOverlayRendersLegendAndClosesOnAnyKey(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(keyMsg("?"))
	m = mustModel(t, next)
	if !m.helpOpen {
		t.Fatalf("'?' must open the help overlay")
	}
	view := m.View()
	for _, want := range []string{"KEYS", "LEGEND", "●", "✗", "[action]", "↔"} {
		if !strings.Contains(view, want) {
			t.Fatalf("help overlay missing %q:\n%s", want, view)
		}
	}
	next, _ = m.Update(keyMsg("x"))
	m = mustModel(t, next)
	if m.helpOpen {
		t.Fatalf("any key must close the help overlay")
	}
}
