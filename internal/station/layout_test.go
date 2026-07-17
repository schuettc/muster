package station

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/schuettc/muster/internal/render"
)

// init forces lipgloss's default renderer to a real color profile: `go
// test` isn't attached to a TTY, so lipgloss's own auto-detection would
// otherwise downgrade every Style().Render() call in this package to plain
// text, and TestFocusedPaneBorderStylesDifferently below (which asserts a
// FOCUSED box's border literally renders different bytes than an unfocused
// one) needs real styling to assert anything meaningful. Forcing this here
// is harmless to every OTHER test in the package: none of them compare
// View() output for byte-exact equality, only strings.Contains against
// plain substrings, which still match whether or not those substrings sit
// inside an ANSI-styled span.
func init() {
	lipgloss.SetColorProfile(termenv.TrueColor)
}

// seedLayoutModel seeds a Model with two projects' worth of roster,
// conversation, and activity content — live and dead agents, unread counts
// (plain and action-marked), one deliberately very long label (a
// pre-redesign layout-polish regression: "roster labels WRAPPING
// mid-phrase"), every conversation intent bucket, a cross-project thread,
// and a handful of journal events — then drills into the "muster" project
// (spec §5-REVISED L1) so the two-column box layout has real content on
// both sides.
func seedLayoutModel(t *testing.T, m Model) Model {
	t.Helper()
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "backend-1", Project: "muster", Label: "backend", Live: true, Unread: 3},
		{Alias: "reviewer-1", Project: "muster", Label: "review", Live: false, Unread: 1, Action: true, ActionCount: 1},
		{Alias: "very-long-session-alias-that-would-have-wrapped-onto-a-second-line", Project: "muster",
			Label: "a genuinely enormous label that used to wrap mid phrase across the roster pane", Live: true},
		{Alias: "no-label-1", Project: "ext", Live: true},
	}})
	m = mustModel(t, next)

	now := time.Now()
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 101, Kind: "send", FromAgent: "backend-1", ToKind: "agent", ToTarget: "reviewer-1", Subject: "please take a look at the auth refactor before EOD", Intent: "action-requested", LastFrom: "backend-1", LastAt: now.Add(-5 * time.Minute).UnixMilli(), EntryCount: 3},
		{ID: 102, Kind: "send", FromAgent: "reviewer-1", ToKind: "agent", ToTarget: "backend-1", Subject: "quick question about the migration", Intent: "reply-requested", LastFrom: "reviewer-1", LastAt: now.Add(-40 * time.Minute).UnixMilli(), EntryCount: 1},
		{ID: 103, Kind: "send", FromAgent: "backend-1", ToKind: "agent", ToTarget: "no-label-1", Subject: "fyi", Intent: "fyi", LastFrom: "backend-1", LastAt: now.Add(-3 * time.Hour).UnixMilli(), EntryCount: 1},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)

	next, _ = m.Update(eventsMsg{page: render.EventsPage{
		Events: []render.EventRow{
			{ID: 1, Kind: "send", Agent: "backend-1", Target: "agent:reviewer-1", ThreadID: 101, Subject: "please take a look at the auth refactor before EOD", Intent: "action-requested"},
			{ID: 2, Kind: "notify", Agent: "reviewer-1", ThreadID: 101, Count: 1, Detail: "lit"},
			{ID: 3, Kind: "send", Agent: "reviewer-1", Target: "agent:backend-1", ThreadID: 102, Subject: "quick question about the migration", Intent: "reply-requested"},
		},
		MaxID: 3,
	}})
	m = mustModel(t, next)

	// Drill into "muster" (two projects exist, so L0 isn't auto-skipped;
	// m.project defaults to "ext" — sorted before "muster" — so move down
	// once first).
	if m.project != "ext" {
		t.Fatalf("setup: expected default L0 selection ext, got %q", m.project)
	}
	next, _ = m.Update(keyMsg("j"))
	m = mustModel(t, next)
	if m.project != "muster" {
		t.Fatalf("setup: expected muster selected after j, got %q", m.project)
	}
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.screen != screenProject {
		t.Fatalf("setup: expected screenProject after drilling into muster")
	}
	return m
}

// TestWindowSizeMsgUpdatesLayout is the plumbing check for spec §5-REVISED
// layout: tea.WindowSizeMsg must actually reach the Model (a pure Update
// case, not something View() has to guess at from the environment), and
// layout()'s box math must derive from it.
func TestWindowSizeMsgUpdatesLayout(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, cmd := m.Update(tea.WindowSizeMsg{Width: 210, Height: 52})
	m = mustModel(t, next)
	if cmd != nil {
		t.Fatalf("applying a WindowSizeMsg must not issue a Cmd")
	}
	if m.termWidth != 210 || m.termHeight != 52 {
		t.Fatalf("termWidth/termHeight = %d/%d, want 210/52", m.termWidth, m.termHeight)
	}
	dims := m.layout()
	if dims.narrow {
		t.Fatalf("210 cols must NOT trigger narrow mode")
	}
	if dims.leftW+dims.rightW != 210 {
		t.Fatalf("layout width = %d+%d, want them to sum to the terminal's 210", dims.leftW, dims.rightW)
	}
	if dims.bodyH+breadcrumbRows+statusLineRows != 52 {
		t.Fatalf("layout height = %d+breadcrumb(%d)+status(%d), want them to sum to the terminal's 52", dims.bodyH, breadcrumbRows, statusLineRows)
	}
}

// TestViewAt210x52IsBoundedAndNeverBleeds is the structural core of the
// layout slice (spec §5 carried-over: nothing overflows its box): at a real
// 210x52 terminal size, View() must produce EXACTLY termHeight lines (never
// more) and every one of those lines must fit within termWidth display
// columns (lipgloss.Width, which — unlike internal/display.Width — is
// ANSI-aware, the correct measure for RENDERED, already-styled output).
func TestViewAt210x52IsBoundedAndNeverBleeds(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	m = seedLayoutModel(t, m)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 210, Height: 52})
	m = mustModel(t, next)

	view := m.View()
	lines := strings.Split(view, "\n")
	if len(lines) != 52 {
		t.Fatalf("View() produced %d lines, want exactly 52 (termHeight)", len(lines))
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w > 210 {
			t.Fatalf("line %d is %d display columns wide, want <= 210:\n%q", i, w, l)
		}
	}
}

// TestBoxLinesAreExactlyOuterWidth is a stricter companion to
// TestViewAt210x52IsBoundedAndNeverBleeds: an upper-bound-only check
// (display width <= terminal width) can pass even when a box is internally
// MISALIGNED — e.g. a content line built two columns narrower than its
// box's inner width still renders at the box's declared outer width
// overall, because lipgloss.JoinVertical pads the short line up to match
// its sibling lines, silently shifting that row's right border away from
// where every other row's border sits. Asserting each box's OWN lines come
// out at EXACTLY their declared outer width (not merely under it) catches
// that class of misalignment directly.
func TestBoxLinesAreExactlyOuterWidth(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	m = seedLayoutModel(t, m)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 210, Height: 52})
	m = mustModel(t, next)
	dims := m.layout()

	checkExactWidth := func(name string, box string, want int) {
		t.Helper()
		for i, l := range strings.Split(box, "\n") {
			if w := lipgloss.Width(l); w != want {
				t.Fatalf("%s line %d is %d display columns wide, want exactly %d:\n%q", name, i, w, want, l)
			}
		}
	}
	checkExactWidth("L1 agents list", m.renderAgentsBox(dims.leftW, dims.convListH), dims.leftW)
	checkExactWidth("thread list (screenAgent)", m.renderConvListBox(dims.leftW, dims.convListH, llAgentThreads, "THREADS", true), dims.leftW)
	checkExactWidth("conversation preview", m.renderConversationBox(dims.rightW, dims.bodyH, false), dims.rightW)
	checkExactWidth("agent page preview", m.renderAgentPagePreviewBox(dims.rightW, dims.bodyH), dims.rightW)
}

// TestAgentStripLongLabelTruncatesInsteadOfWrapping is a pre-redesign
// layout regression carried over: the operator's screenshot showed roster
// labels WRAPPING mid-phrase onto continuation lines. seeded via
// seedLayoutModel's deliberately 80+ char label, this asserts the fixed
// line-count invariant still holds AND that the label was actually
// truncated (an ellipsis appears, the full untruncated label text does not)
// rather than silently fitting by coincidence.
func TestAgentStripLongLabelTruncatesInsteadOfWrapping(t *testing.T) {
	const longLabel = "a genuinely enormous label that used to wrap mid phrase across the roster pane"
	m := NewModel(fakeCaller{}, Options{})
	m = seedLayoutModel(t, m)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 210, Height: 52})
	m = mustModel(t, next)

	view := m.View()
	if strings.Count(view, "\n")+1 != 52 {
		t.Fatalf("a long agent-strip label changed the total line count away from 52 — it must have wrapped:\n%s", view)
	}
	if strings.Contains(view, longLabel) {
		t.Fatalf("the full long label must not appear verbatim — it should have been truncated with an ellipsis:\n%s", view)
	}
	if !strings.Contains(view, "…") {
		t.Fatalf("expected an ellipsis marking the truncated agent-strip label:\n%s", view)
	}
}

// TestFocusedPaneBorderStylesDifferently is spec §5's focus-indication
// check: the FOCUSED box's border must render distinctly (bold/color) from
// an otherwise-identical unfocused box's border — not just a different
// title string, which would trivially differ regardless of styling.
func TestFocusedPaneBorderStylesDifferently(t *testing.T) {
	focused := renderBox("PANE", true, 20, 5, []string{"row one", "row two", "row three"})
	unfocused := renderBox("PANE", false, 20, 5, []string{"row one", "row two", "row three"})
	if focused == unfocused {
		t.Fatalf("a focused pane's box must render differently from an unfocused one with identical content/title")
	}
	for _, want := range []string{"PANE", "row one", "row two", "row three"} {
		if !strings.Contains(focused, want) || !strings.Contains(unfocused, want) {
			t.Fatalf("focused/unfocused boxes must both still show %q", want)
		}
	}
}

// TestConversationListColumnizedAndCrossProjectMarked checks the thread
// list's columnized row format (id, intent tag, participants, last speaker +
// age, subject) and the cross-project marker (spec §5-REVISED: "↔
// otherproj"; iteration-7 item 4: threads now live under their agent, not
// the project-level L1 list, so this descends into backend-1's own thread
// page first).
func TestConversationListColumnizedAndCrossProjectMarked(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	m = seedLayoutModel(t, m)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 210, Height: 52})
	m = mustModel(t, next)

	next, _ = m.Update(keyMsg("enter")) // descend into backend-1 (alphabetically first)
	m = mustModel(t, next)
	if m.screen != screenAgent || m.agent != "backend-1" {
		t.Fatalf("setup: expected screenAgent/backend-1, got screen=%v agent=%q", m.screen, m.agent)
	}

	view := m.View()
	for _, want := range []string{"#101", "[action]", "#102", "[reply?]", "#103", "[fyi]", "backend→review", "review→backend", "↔ ext"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

// TestStatusLineShowsKeyHintsAndErrorPrefix checks the bottom line shows the
// key-hint vocabulary, and an error status gets a visually distinct prefix
// rather than reading like routine status text.
func TestStatusLineShowsKeyHintsAndErrorPrefix(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	m.status = ""
	if got := m.renderStatus(); !strings.Contains(got, "q quit") {
		t.Fatalf("status line missing the quit key hint: %q", got)
	}

	m.status = "events: poll failed, retrying: boom"
	if got := m.renderStatus(); !strings.Contains(got, "✗") {
		t.Fatalf("an error status must carry a distinct prefix, got %q", got)
	}
}

// TestNarrowThresholdMath is the box-math half of spec §5-REVISED's narrow
// mode (the interaction half — Enter/Esc swapping list<->detail — lives in
// nav_test.go's TestNarrowModeSingleColumnSwapsOnFocus): below
// narrowWidthThreshold cols, layout() collapses to one full-width column.
func TestNarrowThresholdMath(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(tea.WindowSizeMsg{Width: narrowWidthThreshold - 1, Height: 40})
	m = mustModel(t, next)
	dims := m.layout()
	if !dims.narrow {
		t.Fatalf("width just under the threshold must be narrow")
	}
	if dims.leftW != narrowWidthThreshold-1 || dims.rightW != narrowWidthThreshold-1 {
		t.Fatalf("narrow mode must give both columns the full terminal width, got left=%d right=%d", dims.leftW, dims.rightW)
	}

	next, _ = m.Update(tea.WindowSizeMsg{Width: narrowWidthThreshold, Height: 40})
	m = mustModel(t, next)
	if m.layout().narrow {
		t.Fatalf("width AT the threshold must not be narrow")
	}
}

// This section is spec iteration-8's own test suite: the threads-level
// layout goes horizontal (an agent's own thread list, and the
// "(unassigned)" bucket's ORPHANED THREADS exception) — full-width table on
// top, full-width preview below — while projects/agents levels keep the
// vertical two-column split, and narrow mode's single-column collapse stays
// exactly as it was.

// TestThreadsLevelLayoutGoesHorizontal is iteration-8's core sizing +
// column-widen check: at screenAgent (an agent's own thread list) and a wide
// (200-col) terminal, layout() must report threadsHorizontal with BOTH the
// table and the preview spanning the full terminal width, stacked list/
// preview by height (default 60/40) rather than split left/right — and the
// widened WHO column must render a from→to pair in FULL that the
// pre-iteration-8 fixed 14-col width would have truncated.
func TestThreadsLevelLayoutGoesHorizontal(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	m = mustModel(t, next)
	m = focusConversationList(t, m, "backend-reviewer")

	const who = "backend-reviewer→frontend-owner" // 31 display cols: fits the new (up to 32) WHO width, not the old fixed 14
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "backend-reviewer", ToKind: "agent", ToTarget: "frontend-owner", Subject: "widen the columns", LastAt: time.Now().UnixMilli(), EntryCount: 1},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)

	dims := m.layout()
	if !dims.threadsHorizontal {
		t.Fatalf("screenAgent's own thread list must use the iteration-8 horizontal split")
	}
	if dims.leftW != 200 || dims.rightW != 200 {
		t.Fatalf("the threads-level table/preview must EACH span the full terminal width, got left=%d right=%d", dims.leftW, dims.rightW)
	}
	if dims.convListH+dims.previewH != dims.bodyH {
		t.Fatalf("table+preview heights must sum to exactly bodyH, got %d+%d != %d", dims.convListH, dims.previewH, dims.bodyH)
	}
	if dims.convListH >= dims.bodyH || dims.previewH >= dims.bodyH {
		t.Fatalf("table/preview must each be a SHARE of bodyH (stacked), not the whole body, got convListH=%d previewH=%d bodyH=%d", dims.convListH, dims.previewH, dims.bodyH)
	}

	view := m.View()
	if !strings.Contains(view, who) {
		t.Fatalf("a WHO pair sized to fit the widened column must render in FULL at a 200-col terminal:\n%s", view)
	}
	if !strings.Contains(view, "THREADS") {
		t.Fatalf("expected the full-width THREADS table, got:\n%s", view)
	}
	if !strings.Contains(view, "THREAD") {
		t.Fatalf("expected the full-width preview box below the table, got:\n%s", view)
	}

	// Proving the widen is real (not just a wider box reusing the same
	// column budget): the SAME who pair, columnized at the OLD
	// (pre-iteration-8) fixed left-column inner width, must have been
	// truncated with an ellipsis.
	rows := m.conversationRows()
	if len(rows) != 1 {
		t.Fatalf("setup: expected exactly one conversation row, got %d", len(rows))
	}
	oldInnerW := leftColWidth - boxBorderCols
	oldLine := m.renderConversationLine(rows[0], oldInnerW)
	if strings.Contains(oldLine, who) {
		t.Fatalf("setup: the who pair must NOT fit at the old left-column width %d (defeats the point of this test): %q", oldInnerW, oldLine)
	}
	if !strings.Contains(oldLine, "…") {
		t.Fatalf("setup: expected the old-width row to show a truncation ellipsis, got %q", oldLine)
	}
}

// TestOrphanedThreadsLevelGoesHorizontal covers iteration-8's OTHER
// threads-table level: the "(unassigned)" bucket's ORPHANED THREADS
// exception (screenProject + l1IsOrphaned) — same horizontal treatment as
// screenAgent's own thread list.
func TestOrphanedThreadsLevelGoesHorizontal(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "agent-a", Subject: "belongs-to-p"},
		// No roster entry for either party: falls into the "(unassigned)"
		// bucket (see threadProjectsOrUnassigned).
		{ID: 99, FromAgent: "ghost-1", ToKind: "agent", ToTarget: "ghost-2", Subject: "orphaned-subject"},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	next, _ = m.Update(agentsMsg{rows: []agentEnriched{{Alias: "agent-a", Project: "p"}}})
	m = mustModel(t, next)
	next, _ = m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	m = mustModel(t, next)

	m.project = unassignedProject
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.screen != screenProject || !m.l1IsOrphaned() {
		t.Fatalf("setup: expected screenProject/l1IsOrphaned, got screen=%v project=%q", m.screen, m.project)
	}

	dims := m.layout()
	if !dims.threadsHorizontal {
		t.Fatalf("the ORPHANED THREADS level must also use the horizontal split")
	}
	if dims.leftW != 200 || dims.rightW != 200 {
		t.Fatalf("orphaned-threads table/preview must each span the full width, got left=%d right=%d", dims.leftW, dims.rightW)
	}
	view := m.View()
	if !strings.Contains(view, "ORPHANED THREADS") || !strings.Contains(view, "orphaned-subject") {
		t.Fatalf("expected the horizontal ORPHANED THREADS table to render, got:\n%s", view)
	}
}

// TestProjectsAndAgentsLevelsKeepVerticalSplit covers iteration-8's OTHER
// half: screenProjects (L0) and a normal project's L1 agents list are
// short-label rosters, not the wide columnized table, so they must keep the
// pre-iteration-8 vertical two-column split rather than going horizontal.
func TestProjectsAndAgentsLevelsKeepVerticalSplit(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	m = seedLayoutModel(t, m) // drills into "muster" (a normal, non-orphaned project's L1 agents list)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 210, Height: 52})
	m = mustModel(t, next)
	if m.screen != screenProject || m.l1IsOrphaned() {
		t.Fatalf("setup: expected screenProject with a normal (non-orphaned) agents list")
	}

	dims := m.layout()
	if dims.threadsHorizontal {
		t.Fatalf("a normal project's L1 agents list must keep the vertical two-column split, not go horizontal")
	}
	if dims.leftW+dims.rightW != 210 {
		t.Fatalf("vertical split must sum to the terminal width, got left=%d right=%d", dims.leftW, dims.rightW)
	}

	// screenProjects (L0) itself, before drilling in at all.
	l0 := NewModel(fakeCaller{}, Options{})
	next, _ = l0.Update(tea.WindowSizeMsg{Width: 210, Height: 52})
	l0 = mustModel(t, next)
	if l0.layout().threadsHorizontal {
		t.Fatalf("screenProjects (L0) must keep the vertical two-column split")
	}
}

// TestThreadsLevelNarrowModeUnchanged covers the brief's explicit carve-out:
// narrow mode's single-column collapse at a threads-table level is
// unchanged by iteration-8 — renderBody must still render ONLY the list
// column, with no stacked preview added underneath.
func TestThreadsLevelNarrowModeUnchanged(t *testing.T) {
	m := focusConversationList(t, NewModel(fakeCaller{}, Options{}), "a")
	next, _ := m.Update(tea.WindowSizeMsg{Width: narrowWidthThreshold - 1, Height: 40})
	m = mustModel(t, next)
	dims := m.layout()
	if !dims.narrow {
		t.Fatalf("narrow mode must still apply at the threads level")
	}
	got := m.renderBody(dims)
	want := m.renderLeftColumn(dims)
	if got != want {
		t.Fatalf("narrow mode at the threads level must render ONLY the list column (unchanged) — no stacked preview")
	}
}

// TestThreadWhoWidthClampsMinAndMax is threadWhoWidth's own boundary check:
// floored at the pre-iteration-8 fixed width, capped so a very wide
// terminal doesn't let WHO swallow the whole table.
func TestThreadWhoWidthClampsMinAndMax(t *testing.T) {
	if got := threadWhoWidth(30); got != threadWhoMinWidth {
		t.Fatalf("threadWhoWidth(30) = %d, want the floor %d", got, threadWhoMinWidth)
	}
	if got := threadWhoWidth(300); got != threadWhoMaxWidth {
		t.Fatalf("threadWhoWidth(300) = %d, want the cap %d", got, threadWhoMaxWidth)
	}
}

// TestSplitThreadsRowsFloorsAndSums is splitThreadsRows' own box-math check:
// list+preview heights always sum to exactly bodyH, whether or not the
// floors are actually binding.
func TestSplitThreadsRowsFloorsAndSums(t *testing.T) {
	listH, previewH := splitThreadsRows(48)
	if listH+previewH != 48 {
		t.Fatalf("split must sum to bodyH, got %d+%d != 48", listH, previewH)
	}
	if listH < minThreadsListRows || previewH < minThreadsPreviewRows {
		t.Fatalf("split must respect the floors at a normal bodyH, got list=%d preview=%d", listH, previewH)
	}

	// A degenerately small bodyH still sums correctly even once the floors
	// can no longer both be honored.
	listH, previewH = splitThreadsRows(6)
	if listH+previewH != 6 {
		t.Fatalf("split must sum to bodyH even in a degenerate small terminal, got %d+%d != 6", listH, previewH)
	}
}
