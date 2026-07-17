package station

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/schuettc/muster/internal/display"
	"github.com/schuettc/muster/internal/render"
)

// This file is the layout-polish slice's own concern (Task 12): real
// lipgloss-bordered pane boxes, box math driven by tea.WindowSizeMsg, and the
// column/width discipline that keeps every rendered line inside its own pane
// — nothing here touches the Model's data, keys, or polling logic; it is
// consumed entirely from model.go's View()-side rendering functions.

// Box-math knobs (spec §5 layout polish item 7: "respect tea.WindowSizeMsg
// for all box math"). rosterWidth is spec §5's "~34 cols" fixed left column;
// the fallback* constants reproduce the PRE-polish layout's effective
// footprint (defaultRows visible feed events, the same for the threads pane)
// so any test — or the real program's very first frame, before Init's
// tea.WindowSizeMsg has arrived — that never sees a window size renders
// exactly the content it always did, just now inside a border.
const (
	rosterWidth = 34

	statusLineRows = 1 // the bottom line View() appends outside the box layout
	boxBorderRows  = 2 // top+bottom border rows every box spends
	boxBorderCols  = 2 // left+right border columns every box spends
	minPaneOuter   = 5 // smallest sane box (border rows/cols + >=1-3 interior) — a floor, not a target, for a degenerate terminal size

	feedFallbackContentRows    = defaultRows + 1 // +1 for the feed's Renderer.Header() row
	threadsFallbackContentRows = defaultRows + 1 // +1 for the threads pane's own header row

	fallbackTermWidth = 120
	// fallbackTermHeight is derived so that layout()'s generic avail/2 split
	// reproduces feedFallbackContentRows/threadsFallbackContentRows exactly:
	// avail = fallbackTermHeight-statusLineRows, each box spends boxBorderRows
	// plus its content rows, and the two boxes split avail evenly.
	fallbackTermHeight = statusLineRows + 2*boxBorderRows + feedFallbackContentRows + threadsFallbackContentRows
)

// layoutDims is the box math result driving View()'s three main panes —
// computed fresh every render from the model's last-known terminal size
// (tea.WindowSizeMsg) or, before one has arrived, the fallback footprint
// above. All *W/*H fields are OUTER box sizes (border chars included).
type layoutDims struct {
	rosterW, rosterH int
	rightW           int
	feedH, threadsH  int
}

// layout computes this render's box dimensions from the model's last-known
// terminal size, clamped so nothing ever goes negative or wider than the
// terminal even at a degenerate size (spec §5 layout item 7: "never render
// wider than the terminal").
func (m Model) layout() layoutDims {
	w := m.termWidth
	if w <= 0 {
		w = fallbackTermWidth
	}
	h := m.termHeight
	if h <= 0 {
		h = fallbackTermHeight
	}

	rosterW := rosterWidth
	if rosterW > w-minPaneOuter {
		rosterW = w - minPaneOuter
	}
	if rosterW < minPaneOuter {
		rosterW = minPaneOuter
	}
	rightW := w - rosterW
	if rightW < minPaneOuter {
		rightW = minPaneOuter
	}

	avail := h - statusLineRows
	if avail < minPaneOuter*2 {
		avail = minPaneOuter * 2
	}
	feedH := avail / 2
	if feedH < minPaneOuter {
		feedH = minPaneOuter
	}
	threadsH := avail - feedH
	if threadsH < minPaneOuter {
		threadsH = minPaneOuter
	}

	return layoutDims{rosterW: rosterW, rosterH: feedH + threadsH, rightW: rightW, feedH: feedH, threadsH: threadsH}
}

// Pane border styles (spec §5 layout item 1): the FOCUSED pane's border
// reads distinctly (bright + bold title) from the other two panes' dim
// border, so the operator's eye finds the active pane at a glance instead of
// three visually identical boxes.
var (
	paneBorderFocusedColor = lipgloss.Color("212")
	paneBorderDimColor     = lipgloss.Color("240")

	// Bold only, deliberately no Underline: lipgloss's underline handling
	// re-wraps a styled string word-by-word (so it can leave trailing/
	// separating spaces unstyled), which splits contiguous text like
	// "muster" into one ANSI-wrapped span PER RUNE — harmless to a real
	// terminal's rendering, but it silently breaks a plain strings.Contains
	// check against the rendered text (the substring is no longer
	// contiguous bytes). Bold alone doesn't do this.
	projectHeaderStyle = lipgloss.NewStyle().Bold(true)

	tagStyleFYI    = lipgloss.NewStyle().Faint(true)
	tagStyleReply  = lipgloss.NewStyle().Foreground(lipgloss.Color("221"))
	tagStyleAction = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)

	statusErrStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
)

// renderBox draws a rounded, bordered box titled in its own top border
// (spec §5 layout item 1) around exactly outerH-2 rows of content. lines
// must already be width-correct: each entry exactly outerW-2 display
// columns wide (render.PadDisplay/display.Sanitize having already run) —
// renderBox never re-truncates or re-pads a content line itself, because a
// line may already carry lipgloss color codes (e.g. a colored intent tag)
// that a second Sanitize/PadDisplay pass would miscount (raw ANSI bytes
// aren't display-width-aware) or, worse, that Sanitize would strip outright
// (it deliberately removes ESC/CSI sequences from untrusted bus payloads).
// Every pane's content-line builder owns that width discipline; renderBox
// only fills any SHORT rows with blank interior lines and adds the border.
func renderBox(title string, focused bool, outerW, outerH int, lines []string) string {
	innerW := outerW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	innerH := outerH - boxBorderRows
	if innerH < 0 {
		innerH = 0
	}

	color := paneBorderDimColor
	bold := false
	if focused {
		color = paneBorderFocusedColor
		bold = true
	}
	border := lipgloss.NewStyle().Foreground(color)
	titleStyle := lipgloss.NewStyle().Foreground(color).Bold(bold)

	var b strings.Builder
	b.WriteString(topBorder(title, innerW, border, titleStyle))
	b.WriteString("\n")
	for i := 0; i < innerH; i++ {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		if line == "" {
			line = strings.Repeat(" ", innerW)
		}
		b.WriteString(border.Render("│"))
		b.WriteString(line)
		b.WriteString(border.Render("│"))
		if i < innerH-1 {
			b.WriteString("\n")
		}
	}
	if innerH > 0 {
		b.WriteString("\n")
	}
	b.WriteString(border.Render("╰" + strings.Repeat("─", innerW) + "╯"))
	return b.String()
}

// topBorder builds a box's top border row with title embedded (" TITLE ")
// — the technique this whole task hinges on for "real pane boxes with title
// labels", since lipgloss itself has no built-in bordered-box title. The
// title text is measured and, if necessary, truncated BEFORE any styling is
// applied (display.Sanitize/Width are not ANSI-aware), then the plain dash
// runs and the title are each wrapped in their own style — after their
// plain widths are fixed, never before — so the two Renders below can never
// throw off this row's total display width.
func topBorder(title string, innerW int, border, titleStyle lipgloss.Style) string {
	label := ""
	if title != "" {
		label = " " + title + " "
	}
	if display.Width(label) > innerW {
		label = display.Sanitize(strings.TrimSpace(title), innerW)
	}
	left := 1
	right := innerW - display.Width(label) - left
	if right < 0 {
		right = 0
		left = innerW - display.Width(label)
		if left < 0 {
			left = 0
		}
	}
	return border.Render("╭"+strings.Repeat("─", left)) + titleStyle.Render(label) + border.Render(strings.Repeat("─", right)+"╮")
}

// windowLines returns at most height entries from lines, sliding the window
// so the selected index (if >= 0) stays visible — the roster and threads
// panes' vertical scroll-to-selection, so a selection near the bottom of a
// long list is still on screen rather than the box always just showing the
// top height lines with the cursor scrolled off (spec §5 layout item 1:
// nothing may run past its pane, and that includes losing the selection off
// the bottom). selected < 0, or len(lines) <= height, is a no-op slice from
// the top.
func windowLines(lines []string, height, selected int) []string {
	if height <= 0 {
		return nil
	}
	if len(lines) <= height {
		return lines
	}
	top := 0
	if selected >= 0 {
		top = selected - height/2
	}
	if top < 0 {
		top = 0
	}
	if top > len(lines)-height {
		top = len(lines) - height
	}
	return lines[top : top+height]
}

// Threads pane column widths (spec §5 layout item 3: "columnized like the
// feed"). Knobs, not hardcoded math scattered inline — mirrors
// render.Renderer's whoMaxWidth/whoSenderWidth role for the feed pane.
const (
	threadIDWidth   = 6
	threadTagWidth  = 9
	threadWhoWidth  = 26
	threadLastWidth = 12
	threadAgeWidth  = 4
)

// threadsPlainFixedWidth is every threads-row column EXCEPT subject, plus
// its separators, in plain display columns — renderThreadLine's subject
// budget is innerW minus this, so the columns-plus-subject total always
// comes out to exactly innerW (renderBox's content-line contract).
const threadsPlainFixedWidth = 2 /* marker */ + threadIDWidth + 2 + threadTagWidth + 2 + threadWhoWidth + 2 + threadLastWidth + 2 + threadAgeWidth + 2

// threadsHeaderLine renders the threads pane's own column header, mirroring
// render.Renderer.Header's role for the feed.
func threadsHeaderLine() string {
	return "  " + render.PadDisplay("ID", threadIDWidth) + "  " + render.PadDisplay("TAG", threadTagWidth) + "  " +
		render.PadDisplay("WHO", threadWhoWidth) + "  " + render.PadDisplay("LAST", threadLastWidth) + "  " +
		render.PadDisplay("AGE", threadAgeWidth) + "  " + "SUBJECT"
}

// colorIntentTag wraps an already-padded tag column in its intent's style —
// called AFTER padding (never before), so the invisible ANSI it adds can
// never throw off the column's plain-width accounting (spec §5 layout item
// 3: "intent tag colored subtly, action distinct").
func colorIntentTag(intent, padded string) string {
	switch intent {
	case "action-requested":
		return tagStyleAction.Render(padded)
	case "reply-requested":
		return tagStyleReply.Render(padded)
	case "fyi":
		return tagStyleFYI.Render(padded)
	default:
		return padded
	}
}

// keysHintBase is the base pane view's bottom-line key hint (spec §5 layout
// item 6), right-aligned by joinStatusLine against the status/error text.
const keysHintBase = "tab focus · s send · r reply · n nudge · / filter · a aliases · q quit"

// statusIsError classifies m.status text for the bottom line's distinct
// error prefix (spec §5 layout item 6) — a pure text heuristic over
// already-assigned status strings (every one of which Update's apply*
// handlers set, never this rendering code), rather than a new model field,
// so the error/non-error distinction stays a render-only concern. The three
// markers cover every error-only status string in model.go/actions.go today
// (poll failures, nudge/composer op failures, and applyInboxAck's one
// error-only status, which carries no "failed" substring of its own).
func statusIsError(status string) bool {
	for _, marker := range []string{"failed", "inbox ack:", "journal reset ("} {
		if strings.Contains(status, marker) {
			return true
		}
	}
	return false
}

// joinStatusLine lays out the bottom line's left (status/error) and right
// (key hints) text within width display columns — the right side is
// dropped, never wrapped, if there isn't room for both (status/error text
// always wins the space).
func joinStatusLine(left, right string, width int) string {
	left = display.Sanitize(left, width)
	leftW := display.Width(left)
	rightW := display.Width(right)
	gap := width - leftW - rightW
	if gap < 2 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}
