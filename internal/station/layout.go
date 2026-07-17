package station

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/schuettc/muster/internal/display"
	"github.com/schuettc/muster/internal/render"
)

// This file is the IA-redesign's own box-math concern (spec §5-REVISED):
// real lipgloss-bordered pane boxes sized from tea.WindowSizeMsg, the
// two-column (miller-style) split, and narrow-mode's single-column collapse
// — nothing here touches the Model's navigation, data, or polling logic; it
// is consumed entirely from views.go's rendering functions.

// Box-math knobs. narrowWidthThreshold is spec §5-REVISED's "< ~110 cols"
// single-column cutoff. leftColWidth is the two-column split's fixed left
// width (wider than the pre-redesign roster's ~34 cols, since the left
// column now also carries conversation subjects/participants, not just a
// one-line-per-agent roster). fallback* constants reproduce a sane
// footprint for any test — or the real program's very first frame, before
// Init's tea.WindowSizeMsg has arrived — that never sees a window size.
const (
	narrowWidthThreshold = 110
	leftColWidth         = 60

	breadcrumbRows = 1 // the breadcrumb line above the two-column body
	statusLineRows = 1 // the bottom line View() appends outside the box layout
	boxBorderRows  = 2 // top+bottom border rows every box spends
	boxBorderCols  = 2 // left+right border columns every box spends
	minPaneOuter   = 5 // smallest sane box (border rows/cols + >=1-3 interior) — a floor, not a target, for a degenerate terminal size

	minForYouRows = 4 // screenProjects' pinned FOR YOU box floor (spec iteration-5) before it eats into the project list's share

	fallbackTermWidth  = 120 // >= narrowWidthThreshold, so an unsized caller (incl. every test that never sends a WindowSizeMsg) sees the WIDE two-column layout
	fallbackTermHeight = breadcrumbRows + statusLineRows + 2*boxBorderRows + 2*defaultRows
)

// layoutDims is the box math result driving View()'s body — computed fresh
// every render from the model's last-known terminal size (tea.WindowSizeMsg)
// or, before one has arrived, the fallback footprint above. All *W/*H fields
// are OUTER box sizes (border chars included). narrow is true when the
// single-column collapse applies (spec §5-REVISED: "Narrow terminals (<
// ~110 cols): single-column mode").
type layoutDims struct {
	narrow        bool
	leftW, rightW int
	bodyH         int
	// convListH is the left column's single list box height — screenProject's
	// agents list, the "(unassigned)" bucket's ORPHANED THREADS list, or
	// screenAgent's thread list; always equal to bodyH EXCEPT when
	// threadsHorizontal is true, in which case it's the top table's own
	// (smaller) share — see splitThreadsRows.
	convListH int
	// forYouH is screenProjects' pinned FOR YOU sub-box's height (spec
	// iteration-5) — 0 whenever it isn't showing (station has no unread
	// mail), in which case renderLeftColumn renders the project list at the
	// full bodyH exactly as before this feature.
	forYouH int
	// threadsHorizontal is true when the CURRENT screen's left list is the
	// THREADS table (spec iteration-8: "threads-level layout goes
	// horizontal") — an agent's own thread list (screenAgent) or the
	// "(unassigned)" bucket's ORPHANED THREADS exception (screenProject +
	// l1IsOrphaned). At this level the table spans the FULL terminal width
	// on top (convListH tall) and the selected thread's preview spans full
	// width below (previewH tall) — parent-above-child instead of the
	// vertical two-column split every other level keeps. See
	// isThreadsTableLevel.
	threadsHorizontal bool
	// previewH is the bottom preview box's height when threadsHorizontal is
	// true — meaningless (left 0) otherwise, since every other level's
	// preview spans the full bodyH (see rightColumnHeight).
	previewH int
}

// isThreadsTableLevel reports whether the CURRENT screen's left list is the
// wide, columnized THREADS table (spec iteration-8's horizontal-layout
// trigger) rather than a short-label roster: an agent's own thread list
// (screenAgent), or the "(unassigned)" bucket's ORPHANED THREADS exception
// (screenProject's one l1IsOrphaned case). Projects and agents levels
// (screenProjects, and every OTHER project's L1 agents list) keep the
// vertical two-column split — their left lists are short live-dot/label
// rows, not this ID/TAG/WHO/AGE/SUBJECT table, so they read fine narrow.
// screenProjects' pinned FOR YOU section would also qualify here if it ever
// became table-shaped like this one; today renderForYouBox renders a plain
// subject/from/age line (renderForYouLine), not the columnized table, so it
// stays vertical along with the rest of screenProjects.
func (m Model) isThreadsTableLevel() bool {
	return m.screen == screenAgent || (m.screen == screenProject && m.l1IsOrphaned())
}

// threadsListShareNum/threadsListShareDen is the threads-level horizontal
// split's default list/preview ratio (spec iteration-8: "60/40 by default")
// — knobs, not a hardcoded fraction, so a future operator preference could
// retune the split without touching the layout math itself.
const (
	threadsListShareNum = 6
	threadsListShareDen = 10

	minThreadsListRows    = 5 // floor so the table (header + a few rows) stays usable even in a short terminal
	minThreadsPreviewRows = 3 // floor so the preview keeps at least a line or two of content
)

// splitThreadsRows divides bodyH between the threads-level table (top) and
// its selected thread's preview (bottom) — the default 60/40 split, each
// side floored so a short terminal never squeezes one pane to nothing.
func splitThreadsRows(bodyH int) (listH, previewH int) {
	listH = bodyH * threadsListShareNum / threadsListShareDen
	if listH < minThreadsListRows {
		listH = minThreadsListRows
	}
	if listH > bodyH-minThreadsPreviewRows {
		listH = bodyH - minThreadsPreviewRows
	}
	if listH < 0 {
		listH = 0
	}
	previewH = bodyH - listH
	if previewH < 0 {
		previewH = 0
	}
	return listH, previewH
}

// rightColumnHeight is the right pane's height: previewH when the current
// level lays out horizontally (dims.threadsHorizontal — see
// isThreadsTableLevel), bodyH otherwise (the vertical two-column split's
// right column always spans the whole body height).
func (dims layoutDims) rightColumnHeight() int {
	if dims.threadsHorizontal {
		return dims.previewH
	}
	return dims.bodyH
}

// layout computes this render's box dimensions from the model's last-known
// terminal size, clamped so nothing ever goes negative or wider than the
// terminal even at a degenerate size.
func (m Model) layout() layoutDims {
	w := m.termWidth
	if w <= 0 {
		w = fallbackTermWidth
	}
	h := m.termHeight
	if h <= 0 {
		h = fallbackTermHeight
	}

	bodyH := h - breadcrumbRows - statusLineRows
	if bodyH < minPaneOuter {
		bodyH = minPaneOuter
	}

	dims := layoutDims{bodyH: bodyH, convListH: bodyH}
	switch {
	case w < narrowWidthThreshold:
		// Narrow mode's single-column collapse (spec §5-REVISED) takes
		// priority over the threads-level horizontal split below — narrow
		// mode already shows exactly one list, full width, with no preview
		// pane at all (see renderBody), so there is nothing for
		// threadsHorizontal to add here.
		dims.narrow = true
		dims.leftW, dims.rightW = w, w
	case m.isThreadsTableLevel():
		dims.threadsHorizontal = true
		dims.leftW, dims.rightW = w, w
		dims.convListH, dims.previewH = splitThreadsRows(bodyH)
	default:
		left := leftColWidth
		if left > w-minPaneOuter {
			left = w - minPaneOuter
		}
		if left < minPaneOuter {
			left = minPaneOuter
		}
		right := w - left
		if right < minPaneOuter {
			right = minPaneOuter
		}
		dims.leftW, dims.rightW = left, right
	}

	if m.screen == screenProjects {
		if m.operatorInboxCount() > 0 {
			forYouH := bodyH / 4
			if forYouH < minForYouRows {
				forYouH = minForYouRows
			}
			if forYouH > bodyH-minPaneOuter {
				forYouH = bodyH - minPaneOuter
			}
			if forYouH < 0 {
				forYouH = 0
			}
			dims.forYouH = forYouH
		}
	}
	return dims
}

// readingBoxDims reports the full-width reading view's box dimensions (spec
// iteration-6 item 2: "Enter on a thread... replaces the whole layout with
// the thread view") — the entire terminal's body area, regardless of the
// two-column split or narrow-mode's own collapse threshold: reading is
// always full width, on every terminal size.
func (m Model) readingBoxDims() (width, height int) {
	w := m.termWidth
	if w <= 0 {
		w = fallbackTermWidth
	}
	h := m.termHeight
	if h <= 0 {
		h = fallbackTermHeight
	}
	bodyH := h - breadcrumbRows - statusLineRows
	if bodyH < minPaneOuter {
		bodyH = minPaneOuter
	}
	return w, bodyH
}

// Pane border styles: the FOCUSED box's border reads distinctly (bright +
// bold title) from an unfocused box's dim border, so the operator's eye
// finds the active box at a glance.
var (
	paneBorderFocusedColor = lipgloss.Color("212")
	paneBorderDimColor     = lipgloss.Color("240")

	tagStyleFYI    = lipgloss.NewStyle().Faint(true)
	tagStyleReply  = lipgloss.NewStyle().Foreground(lipgloss.Color("221"))
	tagStyleAction = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)

	statusErrStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)

	breadcrumbStyle = lipgloss.NewStyle().Bold(true)

	// mailBadgeStyle renders the header's 📬 badge when station HAS unread
	// mail (spec iteration-5: "styled prominently") — bold and in the same
	// accent color as a focused pane's border, so it reads as a distinct,
	// attention-grabbing element rather than blending into the plain
	// breadcrumb text.
	mailBadgeStyle = lipgloss.NewStyle().Bold(true).Foreground(paneBorderFocusedColor)

	// mailBadgeDimStyle renders the badge when station has NO unread mail
	// (spec iteration-6 item 4: "📬 always visible in the header: dim '📬 0'
	// when clear, bright '📬 N for you' when not"). An explicit mid-gray, NOT
	// Faint(true): many terminal themes render faint text invisibly, which
	// made the "always visible" badge vanish at zero (operator-reported).
	mailBadgeDimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

// renderBox draws a rounded, bordered box titled in its own top border
// around exactly outerH-2 rows of content. lines must already be
// width-correct: each entry exactly outerW-2 display columns wide
// (render.PadDisplay/display.Sanitize having already run) — renderBox never
// re-truncates or re-pads a content line itself, because a line may already
// carry lipgloss color codes (e.g. a colored intent tag) that a second
// Sanitize/PadDisplay pass would miscount (raw ANSI bytes aren't
// display-width-aware) or, worse, that Sanitize would strip outright (it
// deliberately removes ESC/CSI sequences from untrusted bus payloads). Every
// box's content-line builder owns that width discipline; renderBox only
// fills any SHORT rows with blank interior lines and adds the border.
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

// topBorder builds a box's top border row with title embedded (" TITLE ").
// The title text is measured and, if necessary, truncated BEFORE any
// styling is applied (display.Sanitize/Width are not ANSI-aware), then the
// plain dash runs and the title are each wrapped in their own style — after
// their plain widths are fixed, never before — so the two Renders below can
// never throw off this row's total display width.
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
// so the selected index (if >= 0) stays visible — a list box's vertical
// scroll-to-selection, so a selection near the bottom of a long list is
// still on screen. selected < 0, or len(lines) <= height, is a no-op slice
// from the top.
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

// Conversation-row column widths (spec §5 layout carried-over item:
// "columnized like the feed"; spec iteration-8: "ID/TAG/WHO/AGE/SUBJECT
// finally get room — widen the column budgets accordingly"). ID/TAG/AGE stay
// fixed — TAG must fit the longest intent tag verbatim ("[action]"/
// "[reply?]" = 8 chars, PadDisplay only PADS a short column, it never
// truncates a longer one, so this must be >= the longest tag or the column
// silently overflows; ID/AGE never need more than their pre-iteration-8
// widths. WHO is the one column that scales with available room — see
// threadWhoWidth. LAST (the pre-redesign "last speaker" column) stays
// dropped: WHO's own arrow already conveys the participants.
const (
	threadIDWidth  = 5
	threadTagWidth = 9
	threadAgeWidth = 4

	// threadWhoMinWidth is WHO's floor — the pre-iteration-8 fixed width,
	// still what a narrow-mode terminal (or a degenerately small one) gets.
	// threadWhoMaxWidth caps how much of a very wide terminal's extra room
	// WHO is allowed to claim, so one row's "long-label→another-long-label"
	// can't swallow the whole table when most rows are short and SUBJECT
	// still wants a real budget.
	threadWhoMinWidth = 14
	threadWhoMaxWidth = 32
)

// threadWhoWidth picks WHO's column width for a table rendered at innerW
// display columns (spec iteration-8: "WHO shows both full labels where they
// fit"). The threads-level table now spans the full terminal width in
// non-narrow mode (see isThreadsTableLevel/layout), so WHO scales with it —
// a flat 1/6 share of innerW, floored at threadWhoMinWidth (so a narrow or
// small terminal renders exactly as before this feature) and capped at
// threadWhoMaxWidth.
func threadWhoWidth(innerW int) int {
	who := innerW / 6
	if who < threadWhoMinWidth {
		who = threadWhoMinWidth
	}
	if who > threadWhoMaxWidth {
		who = threadWhoMaxWidth
	}
	return who
}

// threadsColumnWidths computes WHO's width for a table rendered at innerW,
// plus fixedWidth — every column (ID/TAG/WHO/AGE) and its separators, in
// plain display columns — so the SUBJECT budget is innerW minus fixedWidth,
// always coming out to exactly innerW total (renderBox's content-line
// contract). Every caller building a table row at some innerW must derive
// WHO's width through this (never threadWhoWidth alone), so the header row
// and every conversation row size WHO identically.
func threadsColumnWidths(innerW int) (whoW, fixedWidth int) {
	whoW = threadWhoWidth(innerW)
	fixedWidth = 2 /* marker */ + threadIDWidth + 2 + threadTagWidth + 2 + whoW + 2 + threadAgeWidth + 2
	return whoW, fixedWidth
}

// threadsHeaderLine renders a conversation list's own column header at
// innerW, mirroring render.Renderer.Header's role for the activity feed —
// WHO's width matches renderConversationLineMarked's exactly (both derive it
// via threadsColumnWidths), so the header stays aligned with every row under
// it regardless of the table's width.
func threadsHeaderLine(innerW int) string {
	whoW, _ := threadsColumnWidths(innerW)
	return "  " + render.PadDisplay("ID", threadIDWidth) + "  " + render.PadDisplay("TAG", threadTagWidth) + "  " +
		render.PadDisplay("WHO", whoW) + "  " + render.PadDisplay("AGE", threadAgeWidth) + "  " + "SUBJECT"
}

// colorIntentTag wraps an already-padded tag column in its intent's style —
// called AFTER padding (never before), so the invisible ANSI it adds can
// never throw off the column's plain-width accounting.
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

// keysHintBase is the bottom line's key hint (spec §5-REVISED keys),
// right-aligned by joinStatusLine against the status/error text.
const keysHintBase = "enter drill · esc back · tab cycle · s send · r reply · n nudge · m mail · / filter · a aliases · ? help · q quit"

// statusIsError classifies m.status text for the bottom line's distinct
// error prefix — a pure text heuristic over already-assigned status strings
// (every one of which Update's apply* handlers set, never this rendering
// code), rather than a new model field, so the error/non-error distinction
// stays a render-only concern.
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
