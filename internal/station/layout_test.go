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
	checkExactWidth("merged AGENTS+THREADS list", m.renderProjectItemsBox(dims.leftW, dims.convListH), dims.leftW)
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

// TestConversationListColumnizedAndCrossProjectMarked checks the
// conversation list's columnized row format (id, intent tag, participants,
// last speaker + age, subject) and the cross-project marker (spec
// §5-REVISED: "↔ otherproj").
func TestConversationListColumnizedAndCrossProjectMarked(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	m = seedLayoutModel(t, m)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 210, Height: 52})
	m = mustModel(t, next)

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
