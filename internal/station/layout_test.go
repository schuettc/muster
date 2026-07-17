package station

import (
	"encoding/json"
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

// bigRoster/bigThreads/bigFeed seed a Model with enough roster, thread, and
// feed content to exercise the bordered-box layout meaningfully — two
// projects, live and dead agents, unread counts (plain and action-marked),
// one deliberately very long label (the layout-polish bug report's
// complaint: "roster labels WRAPPING mid-phrase"), every thread intent
// bucket, and a handful of feed events.
func seedLayoutModel(t *testing.T, m Model) Model {
	t.Helper()
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "backend-1", Project: "muster", Label: "backend", Live: true, Unread: 3},
		{Alias: "reviewer-1", Project: "muster", Label: "review", Live: false, Unread: 1, Action: true},
		{Alias: "very-long-session-alias-that-would-have-wrapped-onto-a-second-line", Project: "other-project",
			Label: "a genuinely enormous label that used to wrap mid phrase across the roster pane", Live: true},
		{Alias: "no-label-1", Project: "other-project", Live: true},
	}})
	m = mustModel(t, next)

	now := time.Now()
	next, _ = m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 101, Kind: "send", FromAgent: "backend-1", ToKind: "agent", ToTarget: "reviewer-1", Subject: "please take a look at the auth refactor before EOD", Intent: "action-requested", LastFrom: "backend-1", LastAt: now.Add(-5 * time.Minute).UnixMilli(), EntryCount: 3},
		{ID: 102, Kind: "send", FromAgent: "reviewer-1", ToKind: "agent", ToTarget: "backend-1", Subject: "quick question about the migration", Intent: "reply-requested", LastFrom: "reviewer-1", LastAt: now.Add(-40 * time.Minute).UnixMilli(), EntryCount: 1},
		{ID: 103, Kind: "send", FromAgent: "no-label-1", ToKind: "broadcast", ToTarget: "", Subject: "fyi: deployed v0.6.0", Intent: "fyi", LastFrom: "no-label-1", LastAt: now.Add(-3 * time.Hour).UnixMilli(), EntryCount: 1},
	}})
	m = mustModel(t, next)

	next, _ = m.Update(eventsMsg{page: render.EventsPage{
		Events: []render.EventRow{
			{ID: 1, Kind: "send", Agent: "backend-1", Target: "agent:reviewer-1", ThreadID: 101, Subject: "please take a look at the auth refactor before EOD", Intent: "action-requested"},
			{ID: 2, Kind: "notify", Agent: "reviewer-1", ThreadID: 101, Count: 1, Detail: "lit"},
			{ID: 3, Kind: "send", Agent: "reviewer-1", Target: "agent:backend-1", ThreadID: 102, Subject: "quick question about the migration", Intent: "reply-requested"},
		},
		MaxID: 3,
	}})
	m = mustModel(t, next)
	return m
}

// TestWindowSizeMsgUpdatesLayout is the plumbing check for spec §5 layout
// item 7: tea.WindowSizeMsg must actually reach the Model (a pure Update
// case, not something View() has to guess at from the environment).
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
	if dims.rosterW+dims.rightW != 210 {
		t.Fatalf("layout width = %d+%d, want them to sum to the terminal's 210", dims.rosterW, dims.rightW)
	}
	if dims.feedH+dims.threadsH+statusLineRows != 52 {
		t.Fatalf("layout height = %d+%d+status(%d), want them to sum to the terminal's 52", dims.feedH, dims.threadsH, statusLineRows)
	}
}

// TestViewAt210x52IsBoundedAndNeverBleeds is the structural core of the
// layout-polish slice (spec §5 layout items 1 and 7): at a real 210x52
// terminal size, View() must produce EXACTLY termHeight lines (never more —
// nothing is allowed to overflow its box) and every one of those lines must
// fit within termWidth display columns (lipgloss.Width, which — unlike
// internal/display.Width — is ANSI-aware, the correct measure for RENDERED,
// already-styled terminal output).
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
// (display width <= terminal width) can pass even when a pane's box is
// internally MISALIGNED — e.g. a content line built two columns narrower
// than its box's inner width still renders at the box's declared outer
// width overall, because lipgloss.JoinVertical pads the short line up to
// match its sibling lines, silently shifting that row's right border away
// from where every other row's border sits (a real bug this test caught:
// renderThreadLine's subject-column budget double-subtracted the row
// marker's width). Asserting each pane's OWN box lines come out at EXACTLY
// their declared outer width (not merely under it) catches that class of
// misalignment directly.
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
	checkExactWidth("roster", m.renderRosterBox(dims), dims.rosterW)
	checkExactWidth("feed", m.renderFeedBox(dims), dims.rightW)
	checkExactWidth("threads", m.renderThreadsBox(dims), dims.rightW)
}

// TestRosterLongLabelTruncatesInsteadOfWrapping is the layout-polish slice's
// namesake regression (spec §5 layout item 2): the operator's screenshot
// showed roster labels WRAPPING mid-phrase onto continuation lines. seeded
// via seedLayoutModel's deliberately 80+ char label, this asserts the fixed
// line-count invariant still holds (a wrapped cell would inflate the
// roster box's row count past what renderBox's fixed interior height
// allows) AND that the label was actually truncated (an ellipsis appears,
// the full untruncated label text does not) rather than silently fitting by
// coincidence.
func TestRosterLongLabelTruncatesInsteadOfWrapping(t *testing.T) {
	const longLabel = "a genuinely enormous label that used to wrap mid phrase across the roster pane"
	m := NewModel(fakeCaller{}, Options{})
	m = seedLayoutModel(t, m)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 210, Height: 52})
	m = mustModel(t, next)

	view := m.View()
	if strings.Count(view, "\n")+1 != 52 {
		t.Fatalf("a long roster label changed the total line count away from 52 — it must have wrapped:\n%s", view)
	}
	if strings.Contains(view, longLabel) {
		t.Fatalf("the full long label must not appear verbatim — it should have been truncated with an ellipsis:\n%s", view)
	}
	if !strings.Contains(view, "…") {
		t.Fatalf("expected an ellipsis marking the truncated roster label:\n%s", view)
	}
}

// TestFocusedPaneBorderStylesDifferently is spec §5 layout item 1's focus
// indication check: the FOCUSED pane's border must render distinctly
// (bold/color) from an otherwise-identical unfocused pane's border — not
// just a different title string, which would trivially differ regardless of
// styling. Holding every argument but `focused` constant isolates exactly
// the style difference renderBox is responsible for.
func TestFocusedPaneBorderStylesDifferently(t *testing.T) {
	focused := renderBox("PANE", true, 20, 5, []string{"row one", "row two", "row three"})
	unfocused := renderBox("PANE", false, 20, 5, []string{"row one", "row two", "row three"})
	if focused == unfocused {
		t.Fatalf("a focused pane's box must render differently from an unfocused one with identical content/title")
	}
	// Both must still carry the SAME plain title text and content — only
	// the styling should differ, not the substance.
	for _, want := range []string{"PANE", "row one", "row two", "row three"} {
		if !strings.Contains(focused, want) || !strings.Contains(unfocused, want) {
			t.Fatalf("focused/unfocused boxes must both still show %q", want)
		}
	}
}

// TestThreadsPaneColumnized checks the threads pane's columnized row format
// (spec §5 layout item 3): id, intent tag, participants, last speaker +
// age, and subject each land in their own column, and a long subject is
// capped to the pane rather than bleeding past it.
func TestThreadsPaneColumnized(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	m = seedLayoutModel(t, m)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 210, Height: 52})
	m = mustModel(t, next)

	view := m.View()
	for _, want := range []string{"#101", "[action]", "#102", "[reply?]", "#103", "[fyi]", "backend → review", "review → backend"} {
		if !strings.Contains(view, want) {
			t.Fatalf("threads pane view missing %q:\n%s", want, view)
		}
	}
}

// TestThreadViewOverlayIsBordered checks spec §5 layout item 5: the thread
// view overlay renders as a bordered box (rounded-corner border characters
// present) rather than bare text, and never exceeds the terminal width.
func TestThreadViewOverlayIsBordered(t *testing.T) {
	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op != "get_thread" {
			return json.RawMessage(`{}`), nil
		}
		entries := []threadEntryRow{
			{ID: 1, ThreadID: 101, FromAgent: "backend-1", Body: "please take a look at the auth refactor before EOD", CreatedAt: 1000},
			{ID: 2, ThreadID: 101, FromAgent: "reviewer-1", Body: "on it", CreatedAt: 2000},
		}
		b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": entries, "total": len(entries)})
		return b, nil
	}}
	m := NewModel(fake, Options{})
	m = seedLayoutModel(t, m)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 210, Height: 52})
	m = mustModel(t, next)
	m = focusThreads(t, m)

	next, cmd := m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	for _, msg := range flattenCmds(cmd) {
		next, _ = m.Update(msg)
		m = mustModel(t, next)
	}
	if !m.viewOpen {
		t.Fatalf("enter on the threads pane must open the thread view overlay")
	}

	view := m.View()
	for _, want := range []string{"╭", "╮", "╰", "╯", "│"} {
		if !strings.Contains(view, want) {
			t.Fatalf("thread view overlay must render as a bordered box (missing %q):\n%s", want, view)
		}
	}
	for i, l := range strings.Split(view, "\n") {
		if w := lipgloss.Width(l); w > 210 {
			t.Fatalf("thread view overlay line %d is %d columns wide, want <= 210", i, w)
		}
	}
}

// TestStatusLineShowsKeyHintsAndErrorPrefix checks spec §5 layout item 6:
// the bottom line shows the key-hint vocabulary, and an error status gets a
// visually distinct prefix rather than reading like routine status text.
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
