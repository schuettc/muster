package station

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// This file is station's own navigation test suite (spec §5-LOCK): the pure
// STACK navigation state machine (Enter pushes, Esc pops exactly one frame,
// g pops home), project rollup math, cross-project marking, single-project
// auto-skip, narrow-mode rendering, preview-vs-open-to-acknowledge, and the
// '?' help overlay.

// twoProjectAgents seeds a roster spanning two projects, one agent each.
func twoProjectAgents() []agentEnriched {
	return []agentEnriched{
		{Alias: "alpha-1", Project: "alpha", Live: true},
		{Alias: "beta-1", Project: "beta", Live: true},
	}
}

// TestNavStackPushPopAndHome is spec §5-LOCK decision B's own core test: a
// deep drill (projects → agents → an agent's threads → full-width read)
// pushes one frame per Enter, Esc pops exactly one frame at a time, and 'g'
// jumps straight back to the root regardless of how deep the stack is.
func TestNavStackPushPopAndHome(t *testing.T) {
	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "get_thread" {
			b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": []threadEntryRow{{ID: 1, FromAgent: "beta-1", Body: "hi"}}, "total": 1})
			return b, nil
		}
		return json.RawMessage(`{}`), nil
	}}
	m := NewModel(fake, Options{})
	next, _ := m.Update(agentsMsg{rows: twoProjectAgents()})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "beta-1", ToKind: "agent", ToTarget: "beta-1", EntryCount: 1},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if len(m.stack) != 1 || m.stack[0].screen != screenProjects {
		t.Fatalf("fresh model: expected a 1-deep stack at screenProjects, got %+v", m.stack)
	}

	// L0 -> L1: Enter pushes a screenProject frame.
	m.project = "beta"
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if len(m.stack) != 2 || m.screen != screenProject {
		t.Fatalf("after L0 Enter: expected a 2-deep stack at screenProject, got len=%d screen=%v", len(m.stack), m.screen)
	}

	// L1 -> L2: Enter pushes a screenAgent frame.
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if len(m.stack) != 3 || m.screen != screenAgent || m.agent != "beta-1" {
		t.Fatalf("after L1 Enter: expected a 3-deep stack at screenAgent/beta-1, got len=%d screen=%v agent=%q", len(m.stack), m.screen, m.agent)
	}

	// L2 -> L3: Enter pushes a screenRead frame.
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if len(m.stack) != 4 || m.screen != screenRead {
		t.Fatalf("after L2 Enter: expected a 4-deep stack at screenRead, got len=%d screen=%v", len(m.stack), m.screen)
	}

	// 'g' from the deepest frame jumps straight back to the root.
	next, _ = m.Update(keyMsg("g"))
	m = mustModel(t, next)
	if len(m.stack) != 1 || m.screen != screenProjects {
		t.Fatalf("'g' must pop all the way home from any depth, got len=%d screen=%v", len(m.stack), m.screen)
	}

	// Re-drill to full depth, then Esc pops EXACTLY one frame per press.
	m.project = "beta"
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if len(m.stack) != 4 {
		t.Fatalf("setup: expected a 4-deep stack again, got %d", len(m.stack))
	}

	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	if len(m.stack) != 3 || m.screen != screenAgent {
		t.Fatalf("Esc #1 must pop exactly one frame (to screenAgent), got len=%d screen=%v", len(m.stack), m.screen)
	}
	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	if len(m.stack) != 2 || m.screen != screenProject {
		t.Fatalf("Esc #2 must pop exactly one frame (to screenProject), got len=%d screen=%v", len(m.stack), m.screen)
	}
	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	if len(m.stack) != 1 || m.screen != screenProjects {
		t.Fatalf("Esc #3 must pop exactly one frame (to screenProjects), got len=%d screen=%v", len(m.stack), m.screen)
	}

	// A further Esc at the root is a no-op.
	before := m
	after := mustModel(t, before.popFrame())
	if len(after.stack) != len(before.stack) || after.screen != before.screen {
		t.Fatalf("Esc at the root must be a no-op")
	}
}

// TestNavigationDrillAndClimbTransitions exercises the full chain: L0 → L1
// (a project's agents list) → L2 (agent) and back, verifying Enter
// drills/descends, Esc climbs, and each level's selection survives a
// climb-and-return.
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
	if m.screen != screenProject {
		t.Fatalf("Enter on a project must drill to screenProject (agents list), got screen=%v", m.screen)
	}

	// Drill into the agents list's selected agent (beta-1, the only agent in
	// project beta).
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.screen != screenAgent || m.agent != "beta-1" {
		t.Fatalf("Enter on the AGENTS list must drill to screenAgent for beta-1, got screen=%v agent=%q", m.screen, m.agent)
	}

	// Esc climbs back to screenProject, then to screenProjects.
	m = mustModel(t, m.popFrame())
	if m.screen != screenProject {
		t.Fatalf("first Esc must climb to screenProject, got screen=%v", m.screen)
	}
	if m.project != "beta" {
		t.Fatalf("climbing must preserve the per-level project selection, got %q", m.project)
	}
	m = mustModel(t, m.popFrame())
	if m.screen != screenProjects {
		t.Fatalf("second Esc must climb all the way to screenProjects, got screen=%v", m.screen)
	}
	if m.project != "beta" {
		t.Fatalf("L0's own selection must still be beta after climbing back to it, got %q", m.project)
	}

	// A third Esc at the top of the stack is a no-op.
	before := m
	after := mustModel(t, before.popFrame())
	if after.screen != before.screen || len(after.stack) != len(before.stack) {
		t.Fatalf("Esc at screenProjects must be a no-op")
	}
}

// TestSingleProjectAutoSkipsL0 covers: "Auto-skipped when the bus has
// exactly one project (non-dotfiles users land at L1 — the design must not
// assume the proj-socket convention)".
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
	after := mustModel(t, m.popFrame())
	if after.screen != screenProject {
		t.Fatalf("Esc on a single-project bus must not climb to screenProjects, got %v", after.screen)
	}

	// 'g' is ALSO a no-op — the auto-skipped L1 stands in for home.
	afterHome := m.popToHome()
	if afterHome.screen != screenProject {
		t.Fatalf("'g' on a single-project bus must stay at the auto-skipped L1, got %v", afterHome.screen)
	}
}

// TestProjectRollupMath is computeProjectSummaries' own unit test: sibling
// aliases of the SAME session tuple must not double-count that session's
// unread, while distinct sessions in the same project DO sum.
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

// TestCrossProjectMarkerAppearsInBothProjects covers: "a thread belongs to
// every project having a participant; cross-project threads marked
// '↔ otherproj' and shown in both projects".
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
		t.Fatalf("expected thread 1 in alpha's thread list, got %d rows", len(alphaRows))
	}
	if len(alphaRows[0].OtherProjects) != 1 || alphaRows[0].OtherProjects[0] != "beta" {
		t.Fatalf("alpha's row must mark beta as the other project, got %+v", alphaRows[0].OtherProjects)
	}

	betaRows := conversationsForProject(threads, aliasProj, "beta")
	if len(betaRows) != 1 {
		t.Fatalf("expected thread 1 in beta's thread list too, got %d rows", len(betaRows))
	}
	if len(betaRows[0].OtherProjects) != 1 || betaRows[0].OtherProjects[0] != "alpha" {
		t.Fatalf("beta's row must mark alpha as the other project, got %+v", betaRows[0].OtherProjects)
	}

	// The rendered line carries the "↔" marker.
	m := NewModel(fakeCaller{}, Options{})
	line := m.renderConversationLine(alphaRows[0], 200, m.threadWhoContentWidth(alphaRows))
	if !strings.Contains(line, "↔ beta") {
		t.Fatalf("rendered thread line missing the cross-project marker, got %q", line)
	}

	// A single-project thread carries no marker at all.
	single := conversationsForProject([]listThreadRow{{ID: 2, FromAgent: "alpha-1", ToKind: "broadcast"}}, aliasProj, "alpha")
	if len(single) != 1 || len(single[0].OtherProjects) != 0 {
		t.Fatalf("a single-project thread must carry no OtherProjects, got %+v", single)
	}
}

// TestOriginProjectKeepsThreadReachableAfterParticipantsDeregister: with
// ZERO agents currently registered (every participant has deregistered, the
// ghost-site scenario), a thread whose origin_project was stamped at
// creation still gets an L0/L1 home under that project name — even though
// no agent belongs to it anymore — while a thread that was NEVER stamped
// (and has no roster-resolvable participant either) falls into the
// "(unassigned)" bucket instead of vanishing.
func TestOriginProjectKeepsThreadReachableAfterParticipantsDeregister(t *testing.T) {
	var agents []agentEnriched // every participant has deregistered
	aliasProj := aliasProjectMap(agents)

	stamped := listThreadRow{ID: 9, FromAgent: "ghost-1", ToKind: "agent", ToTarget: "ghost-2", OriginProject: "muster"}
	unstamped := listThreadRow{ID: 14, FromAgent: "ghost-3", ToKind: "agent", ToTarget: "ghost-4"} // OriginProject "" — never resolved
	threads := []listThreadRow{stamped, unstamped}

	summaries := computeProjectSummaries(agents, threads)
	var muster, unassigned *projectSummary
	for i := range summaries {
		switch summaries[i].Name {
		case "muster":
			muster = &summaries[i]
		case unassignedProject:
			unassigned = &summaries[i]
		}
	}
	if muster == nil {
		t.Fatalf("expected an L0 'muster' bucket from the STAMPED thread even with 0 agents left there, got %+v", summaries)
	}
	if muster.Total != 0 || muster.Live != 0 {
		t.Fatalf("the muster bucket must show 0 agents (all deregistered), got %+v", muster)
	}
	if unassigned == nil {
		t.Fatalf("expected an L0 '(unassigned)' bucket from the UNSTAMPED thread, got %+v", summaries)
	}

	musterRows := conversationsForProject(threads, aliasProj, "muster")
	if len(musterRows) != 1 || musterRows[0].ID != 9 {
		t.Fatalf("expected only the stamped thread 9 filed under muster, got %+v", musterRows)
	}

	unassignedRows := conversationsForProject(threads, aliasProj, unassignedProject)
	if len(unassignedRows) != 1 || unassignedRows[0].ID != 14 {
		t.Fatalf("expected only the unstamped thread 14 filed under (unassigned), got %+v", unassignedRows)
	}

	if got := projectDisplayName(unassignedProject); got != "(unassigned)" {
		t.Fatalf("projectDisplayName(unassignedProject) = %q, want \"(unassigned)\"", got)
	}
}

// TestNarrowModeSingleColumnSwapsOnFocus covers: "Narrow terminals (< ~110
// cols): single-column mode" — full-width reading applies on EVERY terminal
// size, not just narrow: Enter on a thread always replaces the whole layout
// with the thread view, and Esc returns to the previous browse state.
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
	if m.screen != screenProject {
		t.Fatalf("a single-project bus must auto-skip straight to screenProject (the agents list), got screen=%v", m.screen)
	}

	// Not reading, still on the AGENTS list: the body must show the agent
	// row, not the detail reader.
	view := m.renderBody()
	if !strings.Contains(view, "alpha-1") {
		t.Fatalf("narrow list view must show the AGENTS list:\n%s", view)
	}
	if strings.Contains(view, "hello there") {
		t.Fatalf("narrow list view must NOT show the detail reader's body:\n%s", view)
	}

	// Descend into alpha-1's own thread list.
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.screen != screenAgent {
		t.Fatalf("Enter on the AGENTS list must descend to screenAgent, got screen=%v", m.screen)
	}
	view = m.renderBody()
	if !strings.Contains(view, "narrow test") {
		t.Fatalf("narrow agent-threads view must show the thread list:\n%s", view)
	}

	// Enter reads the thread full-width — body swaps to the reader.
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenRead {
		t.Fatalf("Enter on a thread must focus the reader")
	}
	view = m.renderBody()
	if !strings.Contains(view, "hello there") {
		t.Fatalf("the full-width reading view must show the focused thread's body:\n%s", view)
	}
	if strings.Contains(view, "narrow test") {
		t.Fatalf("the full-width reading view must NOT also show the list's subject text:\n%s", view)
	}

	// Esc returns to the previous browse state (the agent's thread list,
	// selection intact).
	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	if m.screen != screenAgent {
		t.Fatalf("Esc must return to screenAgent, got screen=%v", m.screen)
	}
}

// TestPreviewDoesNotAcknowledgeFocusDoes is the open-to-acknowledge test:
// selecting a thread (which fetches its preview) must never call get_inbox;
// opening it must call get_inbox EXACTLY once, even across repeated
// open/close of the SAME thread.
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
		t.Fatalf("selecting a thread must still fetch its preview via get_thread")
	}
	if m.screen != screenProject {
		t.Fatalf("single-project auto-skip must already have landed at screenProject (the agents list), got screen=%v", m.screen)
	}
	next, _ = m.Update(keyMsg("enter")) // descend into alpha-1's own thread list, so Enter below opens the thread
	m = mustModel(t, next)
	if m.screen != screenAgent {
		t.Fatalf("setup: expected screenAgent, got %v", m.screen)
	}

	// Opening thread 1 (addressed to station) must ack exactly once.
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if getInboxCalls != 1 {
		t.Fatalf("get_inbox calls after opening a station-addressed thread = %d, want exactly 1", getInboxCalls)
	}

	// Close and reopen the SAME thread: must not ack again.
	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if getInboxCalls != 1 {
		t.Fatalf("reopening the SAME thread must not re-acknowledge, got %d calls", getInboxCalls)
	}
}

// TestHelpOverlayRendersLegendAndClosesOnAnyKey covers: "'?' help overlay:
// keys + glyph legend".
func TestHelpOverlayRendersLegendAndClosesOnAnyKey(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(keyMsg("?"))
	m = mustModel(t, next)
	if !m.helpOpen {
		t.Fatalf("'?' must open the help overlay")
	}
	view := m.View()
	for _, want := range []string{"KEYS", "LEGEND", "●", "✗", "needs action", "↔"} {
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

// TestHelpOverlayReachableFromEveryLevel: '?' must open the overlay from
// every level of the chain — L0 (screenProjects), L1's agents list, L2's
// agent-threads list, AND the full-width reading view. Two DIFFERENT
// projects keep m.singleProject false, so L0 is never auto-skipped and
// stays reachable to test directly.
func TestHelpOverlayReachableFromEveryLevel(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "a1", Project: "p1", Live: true},
		{Alias: "a2", Project: "p2", Live: true},
	}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "a1", ToKind: "agent", ToTarget: "a1", EntryCount: 1},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenProjects {
		t.Fatalf("setup: expected screenProjects with 2 distinct projects registered, got %v", m.screen)
	}

	checkHelpTogglesHere := func(label string) {
		t.Helper()
		next, _ := m.Update(keyMsg("?"))
		probe := mustModel(t, next)
		if !probe.helpOpen {
			t.Fatalf("%s (screen=%v): '?' must open the help overlay", label, probe.screen)
		}
		next, _ = probe.Update(keyMsg("x"))
		probe = mustModel(t, next)
		if probe.helpOpen {
			t.Fatalf("%s: help overlay should close on any key", label)
		}
	}

	checkHelpTogglesHere("L0 projects list")

	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.screen != screenProject {
		t.Fatalf("setup: expected screenProject (the agents list) after Enter, got screen=%v", m.screen)
	}
	checkHelpTogglesHere("L1 agents list")

	next, _ = m.Update(keyMsg("enter")) // descend into the selected agent
	m = mustModel(t, next)
	if m.screen != screenAgent {
		t.Fatalf("setup: expected screenAgent after Enter, got screen=%v", m.screen)
	}
	checkHelpTogglesHere("L2 agent threads")

	next, cmd = m.Update(keyMsg("enter")) // read the selected thread full-width
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenRead {
		t.Fatalf("setup: expected screenRead after Enter on a thread, got %v", m.screen)
	}
	checkHelpTogglesHere("L3 full-width thread reading")
}

// TestFooterKeyHintIsLevelAware: "esc back" and "g home" must not appear in
// the bottom-line hint at a level where they're dead keys — L0
// (screenProjects, nothing to climb back to or jump home from) — but both
// must appear once there's somewhere for them to go.
func TestFooterKeyHintIsLevelAware(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "a1", Project: "p1", Live: true},
		{Alias: "a2", Project: "p2", Live: true},
	}})
	m = mustModel(t, next)
	if m.screen != screenProjects {
		t.Fatalf("setup: expected screenProjects, got %v", m.screen)
	}

	hint := m.renderStatus()
	if strings.Contains(hint, "esc back") {
		t.Fatalf("L0's footer must not advertise esc (a no-op there): %q", hint)
	}
	if strings.Contains(hint, "g home") {
		t.Fatalf("L0's footer must not advertise g (a no-op there): %q", hint)
	}
	if !strings.Contains(hint, "enter drill") {
		t.Fatalf("L0's footer must still advertise enter: %q", hint)
	}

	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.screen != screenProject {
		t.Fatalf("setup: expected screenProject after Enter, got %v", m.screen)
	}
	hint = m.renderStatus()
	if !strings.Contains(hint, "esc back") {
		t.Fatalf("L1's footer must advertise esc (climbs back to L0): %q", hint)
	}
	if !strings.Contains(hint, "g home") {
		t.Fatalf("L1's footer must advertise g (jumps home to L0): %q", hint)
	}
}

// TestNoConversationTerminologyInView: View() output at every screen, the
// help overlay, an open '/' filter, and narrow mode must never contain the
// word "conversation" (case-insensitively) — only "thread".
func TestNoConversationTerminologyInView(t *testing.T) {
	checkNoConversation := func(t *testing.T, label, view string) {
		t.Helper()
		if strings.Contains(strings.ToLower(view), "conversation") {
			t.Fatalf("%s: View() output must not say \"conversation\" (terminology unified on \"threads\"):\n%s", label, view)
		}
	}

	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "get_thread" {
			b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": []threadEntryRow{{ID: 1, FromAgent: "a1", Body: "hello"}}, "total": 1})
			return b, nil
		}
		return json.RawMessage(`{}`), nil
	}}
	m := NewModel(fake, Options{Alias: "station"})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m = mustModel(t, next)
	next, _ = m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "a1", Project: "p1", Live: true},
		{Alias: "a2", Project: "p2", Live: true},
	}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "a1", ToKind: "agent", ToTarget: "a2", Subject: "cross project test", EntryCount: 1},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)

	cases := []struct {
		label   string
		screen  screen
		project string
	}{
		{"L0 projects", screenProjects, ""},
		{"L1 agents list", screenProject, "p1"},
		{"L1 orphaned threads list", screenProject, unassignedProject},
		{"L1 full-width thread reading", screenRead, unassignedProject},
		{"L2 agent threads", screenAgent, "p1"},
		{"L2 full-width thread reading", screenRead, "p1"},
		{"mailbox", screenMailbox, ""},
	}
	for _, c := range cases {
		probe := m
		probe.screen = c.screen
		probe.project = c.project
		if c.screen == screenAgent || (c.screen == screenRead && c.project == "p1") {
			probe.agent = "a1"
		}
		if c.screen == screenRead {
			probe.viewThreadID = 1
			probe.viewEntries = []threadEntryRow{{ID: 1, FromAgent: "a1", Body: "hello"}}
		}
		checkNoConversation(t, c.label, probe.View())
	}

	help := m
	help.helpOpen = true
	checkNoConversation(t, "help overlay", help.View())

	filterOnAgents := m
	filterOnAgents.screen = screenProject
	filterOnAgents = filterOnAgents.openFilter()
	checkNoConversation(t, "'/' filter prompt on the agents list", filterOnAgents.View())

	narrow := m
	narrow.termWidth = 90
	narrow.screen = screenAgent
	narrow.agent = "a1"
	checkNoConversation(t, "narrow mode (list)", narrow.View())
	narrowDetail := narrow
	narrowDetail.screen = screenRead
	narrowDetail.viewThreadID = 1
	narrowDetail.viewEntries = []threadEntryRow{{ID: 1, FromAgent: "a1", Body: "hello"}}
	checkNoConversation(t, "narrow mode (detail)", narrowDetail.View())
}

// TestL1RendersAgentsOnlyNoThreadRows: "L1 = agents ONLY" — a normal
// project's L1 list has exactly one row per agent, j/k walks ONLY the
// agents, and the rendered box shows neither thread subject text nor a
// "THREADS" section header.
func TestL1RendersAgentsOnlyNoThreadRows(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "agent-a", Project: "p"},
		{Alias: "agent-b", Project: "p"},
	}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 10, FromAgent: "agent-a", Subject: "first-thread-subject"},
		{ID: 20, FromAgent: "agent-b", Subject: "second-thread-subject"},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenProject || m.agent != "agent-a" {
		t.Fatalf("setup: expected screenProject/agent-a selected, got screen=%v agent=%q", m.screen, m.agent)
	}

	// The LEFT column is L1's own list — the right column legitimately
	// previews the selected agent's page (their threads + activity), so only
	// the left column is checked for "agents only, no thread rows".
	left := m.renderLeftColumn(m.layout())
	if !strings.Contains(left, "agent-a") || !strings.Contains(left, "agent-b") {
		t.Fatalf("L1 must list every agent:\n%s", left)
	}
	if strings.Contains(left, "first-thread-subject") || strings.Contains(left, "second-thread-subject") {
		t.Fatalf("L1 must show NO thread rows any more:\n%s", left)
	}
	if strings.Contains(left, "THREADS") {
		t.Fatalf("L1 must carry no THREADS section header any more:\n%s", left)
	}

	// j walks only the two agent rows: past the last one, it clamps rather
	// than falling into a (nonexistent) thread section.
	next, _ = m.Update(keyMsg("j"))
	m = mustModel(t, next)
	if m.agent != "agent-b" {
		t.Fatalf("j must move to agent-b, got %q", m.agent)
	}
	next, _ = m.Update(keyMsg("j"))
	m = mustModel(t, next)
	if m.agent != "agent-b" {
		t.Fatalf("j at the last agent row must clamp, got %q", m.agent)
	}
}

// TestUnassignedL1ShowsOrphanedThreads: the "(unassigned)" bucket is the ONE
// deliberate exception to "L1 = agents ONLY" — its L1 list is threads
// directly (no agent to descend through first), self-explainingly labeled
// "ORPHANED THREADS", and Enter on one of its rows reads it full-width
// right there.
func TestUnassignedL1ShowsOrphanedThreads(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	// threadsMsg lands FIRST: applyAgents' single-project auto-skip decides
	// from m.projectRows() at the moment agentsMsg resolves, so the orphaned
	// thread's "(unassigned)" bucket must already be visible in m.threads
	// before the one agent registers, or the auto-skip would fire on a
	// stale single-project snapshot.
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "agent-a", Subject: "belongs-to-p"},
		// No roster entry for either party, and OriginProject unstamped: this
		// falls into the "(unassigned)" bucket (see threadProjectsOrUnassigned).
		{ID: 99, FromAgent: "ghost-1", ToKind: "agent", ToTarget: "ghost-2", Subject: "orphaned-subject"},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	next, _ = m.Update(agentsMsg{rows: []agentEnriched{{Alias: "agent-a", Project: "p"}}})
	m = mustModel(t, next)
	if m.screen != screenProjects {
		t.Fatalf("setup: two distinct L0 buckets (p, unassigned) must keep L0 active, got %v", m.screen)
	}
	if idx := keyIndex(m.projectRows(), projKey, unassignedProject); idx < 0 {
		t.Fatalf("setup: expected an (unassigned) L0 row, got %+v", m.projectRows())
	}

	// Drill into the "(unassigned)" bucket.
	m.project = unassignedProject
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.screen != screenProject || !m.l1IsOrphaned() {
		t.Fatalf("setup: expected screenProject/l1IsOrphaned, got screen=%v project=%q", m.screen, m.project)
	}

	left := m.renderLeftColumn(m.layout())
	if !strings.Contains(left, "ORPHANED THREADS") {
		t.Fatalf("the unassigned bucket's L1 must be labeled ORPHANED THREADS:\n%s", left)
	}
	if !strings.Contains(left, "orphaned-subject") {
		t.Fatalf("the unassigned bucket's L1 must list the orphaned thread:\n%s", left)
	}
	if strings.Contains(left, "belongs-to-p") {
		t.Fatalf("the unassigned bucket's L1 must not leak in a thread that belongs to a real project:\n%s", left)
	}

	// Enter reads it full-width directly — no agent to descend through
	// first (the one exception to the normal agents-first chain).
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenRead || m.conversation != 99 {
		t.Fatalf("Enter on the orphaned thread must read it full-width directly, got screen=%v conversation=%d", m.screen, m.conversation)
	}
}

// TestCrossProjectMarkerUnderBothParticipantAgents: a cross-project thread's
// "↔ otherproj" marker (and the thread itself) must show under EVERY
// participant's own agent thread page — not just one side.
func TestCrossProjectMarkerUnderBothParticipantAgents(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "alpha-1", Project: "alpha"},
		{Alias: "beta-1", Project: "beta"},
	}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "beta-1", Subject: "cross-project"},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)

	m.project = "alpha"
	next, _ = m.Update(keyMsg("enter")) // L0 -> L1 (alpha)
	m = mustModel(t, next)
	next, _ = m.Update(keyMsg("enter")) // descend into alpha-1
	m = mustModel(t, next)
	if m.screen != screenAgent || m.agent != "alpha-1" {
		t.Fatalf("setup: expected screenAgent/alpha-1, got screen=%v agent=%q", m.screen, m.agent)
	}
	alphaRows := m.conversationRows()
	if len(alphaRows) != 1 || alphaRows[0].ID != 1 {
		t.Fatalf("alpha-1's own thread page must list the cross-project thread, got %+v", alphaRows)
	}
	if len(alphaRows[0].OtherProjects) != 1 || alphaRows[0].OtherProjects[0] != "beta" {
		t.Fatalf("alpha-1's own row must mark beta as the other project, got %+v", alphaRows[0].OtherProjects)
	}
	if line := m.renderConversationLine(alphaRows[0], 200, m.threadWhoContentWidth(alphaRows)); !strings.Contains(line, "↔ beta") {
		t.Fatalf("alpha-1's own thread page must render the cross-project marker (↔ beta), got %q", line)
	}

	// Climb back out and descend into beta instead: the SAME thread shows
	// under its OTHER participant's own thread page too, marked "↔ alpha".
	m = mustModel(t, m.popFrame()) // screenAgent -> screenProject
	m = mustModel(t, m.popFrame()) // screenProject -> screenProjects
	m.project = "beta"
	next, _ = m.Update(keyMsg("enter")) // L0 -> L1 (beta)
	m = mustModel(t, next)
	next, _ = m.Update(keyMsg("enter")) // descend into beta-1
	m = mustModel(t, next)
	if m.screen != screenAgent || m.agent != "beta-1" {
		t.Fatalf("setup: expected screenAgent/beta-1, got screen=%v agent=%q", m.screen, m.agent)
	}
	betaRows := m.conversationRows()
	if len(betaRows) != 1 || betaRows[0].ID != 1 {
		t.Fatalf("beta-1's own thread page must ALSO list the cross-project thread, got %+v", betaRows)
	}
	if len(betaRows[0].OtherProjects) != 1 || betaRows[0].OtherProjects[0] != "alpha" {
		t.Fatalf("beta-1's own row must mark alpha as the other project, got %+v", betaRows[0].OtherProjects)
	}
	if line := m.renderConversationLine(betaRows[0], 200, m.threadWhoContentWidth(betaRows)); !strings.Contains(line, "↔ alpha") {
		t.Fatalf("beta-1's own thread page must render the cross-project marker (↔ alpha), got %q", line)
	}
}

// TestAgentDescendLeftListIsTheirThreads: "Enter on an agent DESCENDS a
// level: left column becomes that agent's threads". screenAgent's
// conversationRows() must be EXACTLY conversationsForAgent's participant-
// filtered set for that agent, grouped the same way.
func TestAgentDescendLeftListIsTheirThreads(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "backend-1", Project: "p"},
		{Alias: "reviewer-1", Project: "p"},
		{Alias: "outsider-1", Project: "p"},
	}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "backend-1", ToKind: "agent", ToTarget: "reviewer-1", Subject: "mine"},
		{ID: 2, FromAgent: "reviewer-1", ToKind: "agent", ToTarget: "outsider-1", Subject: "not mine"},
		{ID: 3, FromAgent: "outsider-1", ToKind: "agent", ToTarget: "outsider-1", LastFrom: "backend-1", Subject: "mine via last-speaker"},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)

	// Descend into backend-1 (the AGENTS list's first row, alphabetically).
	if m.agent != "backend-1" {
		t.Fatalf("setup: expected backend-1 selected first, got %q", m.agent)
	}
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.screen != screenAgent || m.agent != "backend-1" {
		t.Fatalf("expected screenAgent for backend-1, got screen=%v agent=%q", m.screen, m.agent)
	}

	got := m.conversationRows()
	want := conversationsForAgent(m.threads, "backend-1")
	if len(got) != len(want) {
		t.Fatalf("screenAgent's left list has %d rows, want %d (conversationsForAgent's own count): %+v", len(got), len(want), got)
	}
	ids := map[int64]bool{}
	for _, r := range got {
		ids[r.ID] = true
	}
	if !ids[1] || !ids[3] {
		t.Fatalf("expected threads 1 (sender) and 3 (last-speaker) in backend-1's list, got %+v", ids)
	}
	if ids[2] {
		t.Fatalf("thread 2 (no backend-1 participation at all) must not appear in backend-1's list, got %+v", ids)
	}
}

// TestFullWidthReadingUsesFullTerminalWidth: Enter on a thread renders the
// reader at the terminal's FULL width/height — not whatever the current
// LEVEL's own box dims happen to be — on every terminal size, wide or
// narrow. screenAgent (this test's level) is itself one of the
// threads-horizontal levels, so its own dims already give both leftW/rightW
// the full terminal width; the meaningful "not reused as-is" axis here is
// HEIGHT — the level's own preview pane is only its smaller bottom share,
// while full-width reading must still claim the WHOLE body height.
func TestFullWidthReadingUsesFullTerminalWidth(t *testing.T) {
	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "get_thread" {
			b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": []threadEntryRow{{ID: 1, FromAgent: "a", Body: "x"}}, "total": 1})
			return b, nil
		}
		return json.RawMessage(`{}`), nil
	}}
	m := NewModel(fake, Options{})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	m = mustModel(t, next)
	next, _ = m.Update(agentsMsg{rows: []agentEnriched{{Alias: "a", Project: "p"}}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{{ID: 1, FromAgent: "a", EntryCount: 1}}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	next, _ = m.Update(keyMsg("enter")) // descend into agent a's own thread list
	m = mustModel(t, next)
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenRead {
		t.Fatalf("setup: expected the thread focused for reading")
	}

	view := m.renderBody()
	for i, l := range strings.Split(view, "\n") {
		if w := lipgloss.Width(l); w != 200 {
			t.Fatalf("full-width reading line %d is %d columns wide, want exactly 200 (the FULL terminal):\n%q", i, w, l)
		}
	}
}

// TestHierarchyWalkEscChainPreservesSelectionAtEveryLevel is the end-to-end
// four-level walk: projects → a project's agents → an agent's threads → a
// full-width thread read → Esc all the way back, asserting the exact
// selection at EVERY level survives the round trip.
func TestHierarchyWalkEscChainPreservesSelectionAtEveryLevel(t *testing.T) {
	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "get_thread" {
			b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": []threadEntryRow{{ID: 1, FromAgent: "a1", Body: "body"}}, "total": 1})
			return b, nil
		}
		return json.RawMessage(`{}`), nil
	}}
	m := NewModel(fake, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "a1", Project: "p1"},
		{Alias: "a2", Project: "p2"},
	}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "a1", ToKind: "agent", ToTarget: "a1", EntryCount: 1},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenProjects {
		t.Fatalf("setup: two distinct projects must keep L0 active, got %v", m.screen)
	}

	// L0: select p2 (moving off the default p1).
	next, _ = m.Update(keyMsg("j"))
	m = mustModel(t, next)
	if m.project != "p2" {
		t.Fatalf("setup: expected p2 selected, got %q", m.project)
	}

	// Drill to L1 on p1 instead (the level under test needs an agent+thread).
	m.project = "p1"
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.screen != screenProject || m.agent != "a1" {
		t.Fatalf("setup: expected screenProject/a1, got screen=%v agent=%q", m.screen, m.agent)
	}

	// L1 -> L2: descend into a1's own thread list.
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.screen != screenAgent || m.agent != "a1" || m.conversation != 1 {
		t.Fatalf("setup: expected screenAgent/a1/conversation=1, got screen=%v agent=%q conversation=%d", m.screen, m.agent, m.conversation)
	}

	// L2 -> L3: read the thread full-width.
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenRead || m.conversation != 1 {
		t.Fatalf("setup: expected full-width reading of conversation 1, got screen=%v conversation=%d", m.screen, m.conversation)
	}

	// Esc #1: out of reading, back to screenAgent's thread list — selection intact.
	m = mustModel(t, m.popFrame())
	if m.screen != screenAgent || m.conversation != 1 || m.agent != "a1" {
		t.Fatalf("Esc #1: expected screenAgent/a1/conversation=1, got screen=%v agent=%q conversation=%d", m.screen, m.agent, m.conversation)
	}

	// Esc #2: climb to screenProject — the agents list, a1 still selected.
	m = mustModel(t, m.popFrame())
	if m.screen != screenProject || m.agent != "a1" {
		t.Fatalf("Esc #2: expected screenProject/a1, got screen=%v agent=%q", m.screen, m.agent)
	}

	// Esc #3: climb to screenProjects — p1 still selected (the drill target,
	// not the p2 the operator moved off of at L0).
	m = mustModel(t, m.popFrame())
	if m.screen != screenProjects || m.project != "p1" {
		t.Fatalf("Esc #3: expected screenProjects/p1, got screen=%v project=%q", m.screen, m.project)
	}
}
