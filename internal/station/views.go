package station

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/schuettc/muster/internal/display"
	"github.com/schuettc/muster/internal/render"
)

// This file is every View()-side rendering function for the IA-redesign
// (spec §5-REVISED): the breadcrumb, the two-column body (projects list /
// agent strip + conversations / agent threads + activity / conversation
// reader), the '?' help overlay, and the bottom line (composer/nudge
// confirm/filter/status). Nothing here mutates Model — every function here
// is a pure function of the model's current state, consumed only from
// View().

// View implements tea.Model.
func (m Model) View() string {
	breadcrumb := m.renderBreadcrumb()
	if m.helpOpen {
		return breadcrumb + "\n" + m.renderHelpOverlay() + "\n" + m.renderBottomLine()
	}
	dims := m.layout()
	return breadcrumb + "\n" + m.renderBody(dims) + "\n" + m.renderBottomLine()
}

// renderBreadcrumb renders the current path (spec §5-REVISED: "breadcrumb
// header always shows the path") — ALL PROJECTS at L0, the project name once
// drilled in, the agent's label at L1.5, and the focused conversation's
// "#id subject" once the reader is focused (L2).
func (m Model) renderBreadcrumb() string {
	var parts []string
	switch m.screen {
	case screenProjects:
		parts = append(parts, "ALL PROJECTS")
	case screenProject:
		parts = append(parts, projectDisplayName(m.project))
	case screenAgent:
		parts = append(parts, projectDisplayName(m.project), m.dispLabel(m.agent))
	}
	if m.focus == focusConvRight && m.viewThreadID != 0 {
		parts = append(parts, fmt.Sprintf("#%d %s", m.viewThreadID, m.conversationSubject(m.viewThreadID)))
	}
	text := strings.Join(parts, " › ")
	width := m.termWidth
	if width <= 0 {
		width = fallbackTermWidth
	}
	return breadcrumbStyle.Render(render.PadDisplay(display.Sanitize(text, width), width))
}

// conversationSubject looks up threadID's subject among the currently loaded
// threads — "" if it isn't (yet) known.
func (m Model) conversationSubject(threadID int64) string {
	if idx := indexOfThread(m.threads, threadID); idx >= 0 {
		return m.threads[idx].Subject
	}
	return ""
}

// renderBody renders the two-column layout, or — in narrow mode (spec
// §5-REVISED: "< ~110 cols") — whichever SINGLE column the current focus
// implies: focusConvRight shows the detail (right) column full width;
// everything else shows the list (left) column full width. Enter/Esc's
// EXISTING focus transitions are what drives this — narrow mode adds no
// separate interaction logic of its own, only this rendering choice.
func (m Model) renderBody(dims layoutDims) string {
	if dims.narrow {
		if m.focus == focusConvRight {
			return m.renderRightColumn(dims)
		}
		return m.renderLeftColumn(dims)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, m.renderLeftColumn(dims), m.renderRightColumn(dims))
}

// renderLeftColumn renders the current screen's list(s): the project list
// (L0), the agent strip stacked over the conversation list (L1), or the
// agent's thread list (L1.5).
func (m Model) renderLeftColumn(dims layoutDims) string {
	switch m.screen {
	case screenAgent:
		return m.renderConvListBox(dims.leftW, dims.convListH, llAgentThreads, "THREADS", m.focus == focusAgentThreads)
	case screenProject:
		agentBox := m.renderAgentStripBox(dims.leftW, dims.agentStripH)
		convBox := m.renderConvListBox(dims.leftW, dims.convListH, llConvList, "THREADS", m.focus == focusConvList)
		return lipgloss.JoinVertical(lipgloss.Left, agentBox, convBox)
	default:
		return m.renderProjectsBox(dims.leftW, dims.bodyH)
	}
}

// renderRightColumn renders the current screen's detail/preview: a
// project's rollup preview (L0), the selected conversation's preview or
// focused reader (L1/L2), or the agent's activity feed unless the reader is
// focused (L1.5/L2).
func (m Model) renderRightColumn(dims layoutDims) string {
	switch m.screen {
	case screenAgent:
		if m.focus == focusConvRight {
			return m.renderConversationBox(dims.rightW, dims.bodyH, true)
		}
		return m.renderActivityBox(dims.rightW, dims.bodyH)
	case screenProject:
		return m.renderConversationBox(dims.rightW, dims.bodyH, m.focus == focusConvRight)
	default:
		return m.renderProjectPreviewBox(dims.rightW, dims.bodyH)
	}
}

// renderProjectsBox builds L0's project list (spec §5-REVISED: "name,
// live/total agents, unread rollup (+action), last-activity age").
func (m Model) renderProjectsBox(outerW, outerH int) string {
	innerW := outerW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	innerH := outerH - boxBorderRows
	if innerH < 0 {
		innerH = 0
	}
	rows := m.projectRows()
	q, f := m.filterQueryFor(llProjects)

	var lines []string
	selectedLine := -1
	for _, p := range rows {
		if !rowVisible(p, m.plainProjectRow, q, f) {
			continue
		}
		cursorMark := "  "
		if p.Name == m.project {
			cursorMark = "> "
			selectedLine = len(lines)
		}
		lines = append(lines, m.renderProjectLine(cursorMark, p, innerW))
	}
	lines = windowLines(lines, innerH, selectedLine)
	return renderBox("PROJECTS", m.focus == focusProjectList, outerW, outerH, lines)
}

// renderProjectLine renders one L0 row's VISIBLE text — a single line,
// clipped (never wrapped) to innerW.
func (m Model) renderProjectLine(cursorMark string, p projectSummary, innerW int) string {
	name := projectDisplayName(p.Name)
	stats := fmt.Sprintf("%d/%d live", p.Live, p.Total)
	unread := ""
	if p.Unread > 0 {
		marker := ""
		if p.ActionUnread > 0 {
			marker = fmt.Sprintf("!%d", p.ActionUnread)
		}
		unread = fmt.Sprintf(" (%d%s)", p.Unread, marker)
	}
	age := relativeAge(time.Now(), p.LastAt)
	suffix := fmt.Sprintf("  %s%s  %s", stats, unread, age)
	avail := innerW - display.Width(cursorMark) - display.Width(suffix)
	if avail < 1 {
		avail = 1
	}
	label := display.Sanitize(name, avail)
	return render.PadDisplay(cursorMark+label+suffix, innerW)
}

// renderProjectPreviewBox builds L0's right-pane preview of the selected
// project.
func (m Model) renderProjectPreviewBox(outerW, outerH int) string {
	innerW := outerW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	rows := m.projectRows()
	idx := keyIndex(rows, projKey, m.project)
	var lines []string
	if idx < 0 {
		lines = append(lines, "no project selected")
	} else {
		p := rows[idx]
		lines = append(lines, projectDisplayName(p.Name))
		lines = append(lines, fmt.Sprintf("%d/%d agents live", p.Live, p.Total))
		unread := fmt.Sprintf("%d unread", p.Unread)
		if p.ActionUnread > 0 {
			unread += fmt.Sprintf(", %d action-requested", p.ActionUnread)
		}
		lines = append(lines, unread)
		if p.LastAt > 0 {
			lines = append(lines, "last activity "+relativeAge(time.Now(), p.LastAt)+" ago")
		}
		lines = append(lines, "", "enter to open")
	}
	padded := make([]string, len(lines))
	for i, l := range lines {
		padded[i] = render.PadDisplay(display.Sanitize(l, innerW), innerW)
	}
	return renderBox("PREVIEW", false, outerW, outerH, padded)
}

// renderRosterRow renders one agent-strip row's plain text: live dot, label
// (resolved via dispLabel, so the 'a' aliases toggle affects the strip
// exactly like every other row), and per-session unread count — "!" marks a
// session whose unread includes an action-requested thread. This is the
// SAME predicate text moveFocused/handleEnterKey/handleNudgeKey resolve
// filter/selection through (spec §5 carried-over fix: filter/selection
// desync) — renderRosterLine below is the padded VISUAL variant.
func (m Model) renderRosterRow(a agentEnriched) string {
	dot := "✗"
	if a.Live {
		dot = "●"
	}
	line := fmt.Sprintf("%s %s", dot, m.dispLabel(a.Alias))
	if a.Unread > 0 {
		marker := ""
		if a.Action {
			marker = "!"
		}
		line += fmt.Sprintf(" (%d%s)", a.Unread, marker)
	}
	return line
}

// renderRosterLine renders one agent-strip row's VISIBLE text — a single
// line, clipped (never wrapped) to innerW.
func (m Model) renderRosterLine(cursorMark string, a agentEnriched, innerW int) string {
	dot := "✗"
	if a.Live {
		dot = "●"
	}
	suffix := ""
	if a.Unread > 0 {
		marker := ""
		if a.Action {
			marker = "!"
		}
		suffix = fmt.Sprintf(" (%d%s)", a.Unread, marker)
	}
	prefix := cursorMark + dot + " "
	avail := innerW - display.Width(prefix) - display.Width(suffix)
	if avail < 1 {
		avail = 1
	}
	label := display.Sanitize(m.dispLabel(a.Alias), avail)
	return render.PadDisplay(prefix+label+suffix, innerW)
}

// renderAgentStripBox builds L1's agent strip: one flat, unread-annotated
// row per agent in the active project (spec §5-REVISED: "agent strip on
// top... n nudges from the strip") — no project-group headers, since the
// strip is already scoped to a single project.
func (m Model) renderAgentStripBox(outerW, outerH int) string {
	innerW := outerW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	innerH := outerH - boxBorderRows
	if innerH < 0 {
		innerH = 0
	}
	rows := m.agentStripRows()
	q, f := m.filterQueryFor(llAgentStrip)

	var lines []string
	selectedLine := -1
	for _, a := range rows {
		if !rowVisible(a, m.renderRosterRow, q, f) {
			continue
		}
		cursorMark := "  "
		if a.Alias == m.agent {
			cursorMark = "> "
			selectedLine = len(lines)
		}
		lines = append(lines, m.renderRosterLine(cursorMark, a, innerW))
	}
	lines = windowLines(lines, innerH, selectedLine)
	return renderBox("AGENTS", m.focus == focusAgentStrip, outerW, outerH, lines)
}

// renderThreadRow renders one conversation row's PLAIN text — the filter/
// selection predicate's text (spec §5 carried-over fix: filter/selection
// desync), reused verbatim by both screenProject's conversation list and
// screenAgent's thread list (conversationRows() already picks the right
// underlying rows for whichever screen is active).
func (m Model) renderThreadRow(row listThreadRow) string {
	marker := "  "
	if row.ID == m.conversation {
		marker = "> "
	}
	tag := intentRowTag(row.Intent)
	if tag != "" {
		tag = " " + tag
	}
	participants := fmt.Sprintf("%s → %s", m.dispLabel(row.FromAgent), m.dispToTarget(row))
	last := m.dispLabel(row.LastFrom)
	age := relativeAge(time.Now(), row.LastAt)
	subject := display.Sanitize(row.Subject, 200)
	return fmt.Sprintf("%s#%d%s %s | %s %s | %s", marker, row.ID, tag, participants, last, age, subject)
}

// renderConversationLine renders one conversation row COLUMNIZED: `#ID
// [tag]  who → who  AGE  subject`, appending a cross-project marker to the
// subject when the row touches another project (spec §5-REVISED:
// "cross-project threads marked '↔ otherproj'"). No separate LAST-speaker
// column (unlike the pre-redesign threads pane) — the left column's fixed
// width has no room for a third identity column once WHO already conveys
// the participants.
func (m Model) renderConversationLine(c conversationRow, innerW int) string {
	marker := "  "
	if c.ID == m.conversation {
		marker = "> "
	}
	idCol := render.PadDisplay(display.Sanitize(fmt.Sprintf("#%d", c.ID), threadIDWidth), threadIDWidth)
	tagPlain := intentRowTag(c.Intent)
	tagCol := colorIntentTag(c.Intent, render.PadDisplay(tagPlain, threadTagWidth))
	who := fmt.Sprintf("%s→%s", m.dispLabel(c.FromAgent), m.dispToTarget(c.listThreadRow))
	whoCol := render.PadDisplay(display.Sanitize(who, threadWhoWidth), threadWhoWidth)
	ageCol := render.PadDisplay(relativeAge(time.Now(), c.LastAt), threadAgeWidth)

	subject := c.Subject
	if len(c.OtherProjects) > 0 {
		names := make([]string, len(c.OtherProjects))
		for i, p := range c.OtherProjects {
			names[i] = projectDisplayName(p)
		}
		subject += "  ↔ " + strings.Join(names, ",")
	}
	subjectBudget := innerW - threadsPlainFixedWidth
	if subjectBudget < 0 {
		subjectBudget = 0
	}
	subjectCol := render.PadDisplay(display.Sanitize(subject, subjectBudget), subjectBudget)

	return marker + idCol + "  " + tagCol + "  " + whoCol + "  " + ageCol + "  " + subjectCol
}

// renderConvListBox builds a conversation-list box (spec §5-REVISED: shared
// by screenProject's CONVERSATIONS and screenAgent's THREADS) — rows already
// grouped action-requested-first by conversationRows(), each columnized
// (renderConversationLine), the selection marker following m.conversation by
// ID so it always lands on the right row regardless of this poll's
// grouping. Rows the active '/' filter hides are skipped via rowVisible —
// the SAME predicate moveFocused/handleEnterKey resolve through — and the
// visible rows are vertically windowed around the selection.
func (m Model) renderConvListBox(outerW, outerH int, list llList, title string, focused bool) string {
	innerW := outerW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	rowsHeight := outerH - boxBorderRows - 1 // -1 for the header row below
	if rowsHeight < 0 {
		rowsHeight = 0
	}
	header := render.PadDisplay(display.Sanitize(threadsHeaderLine(), innerW), innerW)

	rows := m.conversationRows()
	q, f := m.filterQueryFor(list)
	var body []string
	selectedLine := -1
	for _, c := range rows {
		if !rowVisible(c, m.plainConvRow, q, f) {
			continue
		}
		if c.ID == m.conversation {
			selectedLine = len(body)
		}
		body = append(body, m.renderConversationLine(c, innerW))
	}
	body = windowLines(body, rowsHeight, selectedLine)
	lines := append([]string{header}, body...)
	return renderBox(title, focused, outerW, outerH, lines)
}

// conversationAuthorStyle styles a conversation entry's header line — spec
// item 3 of the iteration-three body-structure fix: "author header line
// styled (author · relative time), body indented two spaces under it" —
// distinctly from the plain (or markdown-styled) body text under it.
var conversationAuthorStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("117"))

// conversationLines renders every loaded conversation entry into flat,
// already-wrapped display lines (header + wrapped/styled body + a blank
// separator), and returns entryStart[i]..entryStart[i+1] as entry i's line
// range within lines — entryStart has len(viewEntries)+1 elements, the last
// one the total line count.
//
// Every returned line is EXACTLY width display columns wide already
// (render.PadDisplay/display.Sanitize for the header, wrapBody's own
// lipgloss-Width padding plus the bodyBulletIndent prefix for the body)
// except the blank inter-entry/inter-paragraph separator ("": renderBox's
// own contract fills a bare "" line with innerW spaces, so it does not need
// padding here) — renderConversationBox must not re-Sanitize/re-pad this
// slice, since a body line may already carry lipgloss ANSI from
// styleMarkdownLine that a second pass would miscount (display.Width is not
// ANSI-aware) or strip outright (Sanitize deletes ESC sequences).
//
// The '>' cursor tracks the MESSAGE (its header line only), never a raw
// wrapped body line, so scrolling always lands the reader on a message
// boundary rather than mid-paragraph.
func (m Model) conversationLines(width int) (lines []string, entryStart []int) {
	if width < 1 {
		width = 1
	}
	bodyWidth := width - display.Width(bodyBulletIndent)
	if bodyWidth < 1 {
		bodyWidth = 1
	}

	entryStart = make([]int, len(m.viewEntries)+1)
	for i, e := range m.viewEntries {
		entryStart[i] = len(lines)
		marker := "  "
		if i == m.viewCursor {
			marker = "> "
		}
		age := relativeAge(time.Now(), e.CreatedAt)
		header := display.Sanitize(fmt.Sprintf("%s%s · %s", marker, m.dispLabel(e.FromAgent), age), width)
		lines = append(lines, conversationAuthorStyle.Render(render.PadDisplay(header, width)))

		for _, bl := range wrapBody(e.Body, bodyWidth) {
			if bl == "" {
				lines = append(lines, "") // a blank paragraph break: let renderBox blank-fill it, same as the inter-entry separator below
				continue
			}
			lines = append(lines, bodyBulletIndent+bl)
		}
		lines = append(lines, "")
	}
	entryStart[len(m.viewEntries)] = len(lines)
	return lines, entryStart
}

// conversationWindowTop picks which line to start rendering from when the
// reader is FOCUSED (spec §5 carried-over fix: render windowing): the
// window always ends right at the cursor entry's own lines. Stateless by
// design: the cursor's own index is enough to re-derive the correct window
// on every render.
func (m Model) conversationWindowTop(lines []string, entryStart []int, height int) int {
	if height <= 0 || len(lines) <= height || len(entryStart) <= 1 {
		return 0
	}
	cursor := m.viewCursor
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(entryStart)-2 {
		cursor = len(entryStart) - 2
	}
	top := entryStart[cursor+1] - height
	if top < 0 {
		top = 0
	}
	if maxTop := len(lines) - height; top > maxTop {
		top = maxTop
	}
	return top
}

// snapToEntryBoundary rounds top forward to the nearest real entry boundary
// in entryStart (spec item 4: the passive preview "should start at a
// MESSAGE boundary... not mid-word") — the smallest entryStart[i] (i over
// real entries, excluding the trailing total-line-count sentinel) that is
// >= top. If every real boundary is already behind top (the tail's own
// entry starts earlier than a straight tail-height slice would), it falls
// back to that last entry's own start instead — showing all of one
// long message from its header rather than an empty or mid-body window.
func snapToEntryBoundary(entryStart []int, top int) int {
	if len(entryStart) < 2 {
		return top
	}
	realStarts := entryStart[:len(entryStart)-1]
	for _, s := range realStarts {
		if s >= top {
			return s
		}
	}
	return realStarts[len(realStarts)-1]
}

// renderConversationBox builds the right pane's conversation content — L1's
// passive "last messages" preview (focused=false: just the tail, no cursor
// marks, no load-older/newer hints) or L2's focused reader (focused=true:
// cursor-windowed, "load older"/"N newer" hints, spec §5 carried-over fixes:
// render windowing + the newest-entries gap).
func (m Model) renderConversationBox(outerW, outerH int, focused bool) string {
	innerW := outerW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	title := "THREAD"
	if focused && m.viewThreadID != 0 {
		title = fmt.Sprintf("THREAD #%d", m.viewThreadID)
	}

	if m.viewThreadID == 0 {
		return renderBox(title, focused, outerW, outerH, []string{render.PadDisplay("no thread selected", innerW)})
	}
	if len(m.viewEntries) == 0 {
		msg := "no messages yet"
		if m.viewLoading {
			msg = "loading…"
		}
		return renderBox(title, focused, outerW, outerH, []string{render.PadDisplay(msg, innerW)})
	}

	height := outerH - boxBorderRows
	if height < 1 {
		height = 1
	}
	// content is renderBox-ready: every entry here is already exactly innerW
	// display columns (or the bare "" renderBox itself blank-fills — see
	// conversationLines' doc). The two hint lines below are sanitized/padded
	// right here, at construction, rather than in a later pass over the
	// whole slice — a later pass would re-run display.Sanitize/PadDisplay
	// over conversationLines' output too, corrupting any body line that
	// already carries lipgloss ANSI from markdown styling.
	var content []string
	if focused && m.viewOffset > 0 {
		content = append(content, render.PadDisplay(display.Sanitize("↑ more above — k/↑ to load older", innerW), innerW))
		height--
	}
	var newerHint string
	if focused {
		if n := m.viewNewerCount(); n > 0 {
			newerHint = render.PadDisplay(display.Sanitize(fmt.Sprintf("↓ %d newer — press G to load", n), innerW), innerW)
			height--
		}
	}
	if height < 1 {
		height = 1
	}

	lines, entryStart := m.conversationLines(innerW)
	var top, end int
	if focused {
		top = m.conversationWindowTop(lines, entryStart, height)
		end = top + height
	} else {
		// Passive preview: always show the TAIL, no cursor-based windowing —
		// but snapped forward to the nearest message boundary (spec item 4)
		// so it never opens mid-paragraph/mid-word.
		end = len(lines)
		top = end - height
		if top < 0 {
			top = 0
		}
		top = snapToEntryBoundary(entryStart, top)
		if top > end {
			top = end
		}
	}
	if end > len(lines) {
		end = len(lines)
	}
	if top > end {
		top = end
	}
	content = append(content, lines[top:end]...)
	if newerHint != "" {
		content = append(content, newerHint)
	}

	return renderBox(title, focused, outerW, outerH, content)
}

// renderActivityBox builds L1.5's right pane: the agent's recent journal
// activity (spec §5-REVISED: "their recent journal activity (list_events
// agent filter)"), rendered through render.Renderer verbatim (spec: "the
// render.Renderer stays — agent-activity lines reuse it").
func (m Model) renderActivityBox(outerW, outerH int) string {
	innerW := outerW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	rowsHeight := outerH - boxBorderRows - 1 // -1 for the header row below
	if rowsHeight < 0 {
		rowsHeight = 0
	}

	m.activity.SetWidth(innerW)
	var hb bytes.Buffer
	m.activity.Header(&hb)
	header := render.PadDisplay(display.Sanitize(strings.TrimRight(hb.String(), "\n"), innerW), innerW)

	rows := agentActivity(m.events, m.agent)
	start := 0
	if len(rows) > rowsHeight {
		start = len(rows) - rowsHeight
	}
	lines := []string{header}
	for _, e := range rows[start:] {
		var lb bytes.Buffer
		m.activity.Line(&lb, e)
		line := strings.TrimRight(lb.String(), "\n")
		lines = append(lines, render.PadDisplay(display.Sanitize(line, innerW), innerW))
	}
	return renderBox("ACTIVITY: "+m.dispLabel(m.agent), false, outerW, outerH, lines)
}

// llListName renders a llList value for the '/' filter's "filter (X): …"
// prompt.
func llListName(l llList) string {
	switch l {
	case llProjects:
		return "projects"
	case llAgentStrip:
		return "agents"
	case llConvList:
		return "threads"
	case llAgentThreads:
		return "threads"
	default:
		return ""
	}
}

// helpKeyLines is the '?' overlay's key reference (spec §5-REVISED: "keys +
// glyph legend").
var helpKeyLines = []string{
	"enter    drill into the selection / focus the right pane",
	"esc      climb back up one level",
	"tab      cycle this screen's sub-lists / the right pane",
	"j/k, ↑/↓ move the selection (or scroll the focused reader)",
	"end, G   jump the focused thread reader to its newest entry",
	"s        send a message from anywhere (roster-filtered picker)",
	"r        reply (only while a thread is focused)",
	"n        nudge (agent strip, or an agent's own page)",
	"/        filter the current left list",
	"a        toggle raw aliases vs. current labels",
	"?        toggle this help",
	"q        quit (deregisters this station)",
}

// helpLegendLines is the glyph legend (spec §5-REVISED: "● ✗ (n) ! [action]
// ↔").
var helpLegendLines = []string{
	"●        agent's tmux session is live",
	"✗        agent's tmux session is dead",
	"(n)      n unread messages for that session",
	"!        the unread includes an action-requested thread",
	"[action] [reply?] [fyi]   a thread's intent tag",
	"↔ proj   this thread also touches another project",
}

// renderHelpOverlay renders the '?' overlay: a single bordered box with the
// key reference followed by the glyph legend — closed by any keypress (see
// handleKey's modal-priority switch).
func (m Model) renderHelpOverlay() string {
	width := m.termWidth
	if width <= 0 {
		width = fallbackTermWidth
	}
	if width > 72 {
		width = 72
	}
	innerW := width - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	var lines []string
	lines = append(lines, "KEYS", "")
	lines = append(lines, helpKeyLines...)
	lines = append(lines, "", "LEGEND", "")
	lines = append(lines, helpLegendLines...)
	padded := make([]string, len(lines))
	for i, l := range lines {
		padded[i] = render.PadDisplay(display.Sanitize(l, innerW), innerW)
	}
	h := len(padded) + boxBorderRows
	return renderBox("HELP (any key closes)", true, width, h, padded)
}

// renderBottomLine renders whichever of the composer, the nudge y/n
// confirmation, the '/' filter edit box, or the plain status line currently
// owns the bottom of the screen — mirroring handleKey's same modal-priority
// order (composer > nudge confirm > filter > plain status).
func (m Model) renderBottomLine() string {
	switch {
	case m.composer.phase == composerPickingTarget:
		return m.renderComposerPicker()
	case m.composer.phase == composerEditingBody:
		return m.renderComposerBody()
	case m.nudgeConfirmAlias != "":
		return fmt.Sprintf("nudge %s? y/n", m.dispLabel(m.nudgeConfirmAlias))
	case m.filter.editing:
		return fmt.Sprintf("filter (%s): %s", llListName(m.filter.list), m.filter.input.View())
	default:
		return m.renderStatus()
	}
}

// renderComposerPicker renders the 's' target-picker line: the filter input
// plus the (label-resolved) candidates it currently matches, the highlighted
// one marked. Two candidates sharing the same display label are
// disambiguated with their project prefixed ("project:label").
func (m Model) renderComposerPicker() string {
	cands := m.composerCandidates()
	labels := make([]string, len(cands))
	counts := make(map[string]int, len(cands))
	for i, a := range cands {
		l := m.dispLabel(a.Alias)
		labels[i] = l
		counts[l]++
	}
	names := make([]string, 0, len(cands))
	for i, a := range cands {
		marker := ""
		if i == m.composer.pickerIdx {
			marker = ">"
		}
		label := labels[i]
		if counts[label] > 1 && a.Project != "" {
			label = a.Project + ":" + label
		}
		names = append(names, marker+label)
	}
	line := "to: " + m.composer.filter.View()
	if len(names) == 0 {
		return line + "  (no match)"
	}
	return line + "  [" + strings.Join(names, " ") + "]"
}

// renderComposerBody renders the body-editing line: the F/R/A intent
// indicator, the resolved target (send) or thread (reply), the input, and
// any op error from a previous submit attempt.
func (m Model) renderComposerBody() string {
	var target string
	switch m.composer.kind {
	case composerKindReply:
		target = fmt.Sprintf("reply #%d", m.composer.threadID)
	default:
		target = "to " + m.dispLabel(m.composer.target)
	}
	line := fmt.Sprintf("[%s] %s: %s", m.composer.intent.tag(), target, m.composer.input.View())
	if m.composer.err != "" {
		line += "  (" + m.composer.err + ")"
	}
	return line
}

// renderStatus renders the single bottom status line: the operator's
// status/error text on the left, key hints right-aligned against the
// terminal's own width — errors get a distinct prefix + style
// (statusIsError) rather than reading identically to routine status notes.
func (m Model) renderStatus() string {
	width := m.termWidth
	if width <= 0 {
		width = fallbackTermWidth
	}

	left := m.status
	if statusIsError(left) {
		left = statusErrStyle.Render("✗ " + left)
	}

	right := m.levelKeysHint()
	if m.focus == focusConvRight {
		right = fmt.Sprintf("%s scroll · %s reply · %s back", keys.Down.Help().Key, keys.Reply.Help().Key, keys.Esc.Help().Key)
	}
	return joinStatusLine(left, right, width)
}

// escIsNoop reports whether Esc does nothing at the model's CURRENT
// level — true at L0 (screenProjects: nothing to climb back to) and at L1
// when the bus auto-skipped straight past L0 (m.singleProject: there is no
// L0 to climb back to either, see handleEscKey). Never consulted while
// focus == focusConvRight — Esc always un-focuses the reader there,
// handled by renderStatus's separate branch above.
func (m Model) escIsNoop() bool {
	return m.screen == screenProjects || (m.screen == screenProject && m.singleProject)
}

// tabIsNoop reports whether Tab does nothing at the model's CURRENT level —
// true only at L0 (screenProjects has a single list; cycleFocus's switch
// carries no case for it, so focus never changes).
func (m Model) tabIsNoop() bool {
	return m.screen == screenProjects
}

// levelKeysHint is the DEFAULT (non-focusConvRight) bottom-line key hint,
// level-aware (spec item 5, iteration-three fix: "the footer hints name
// esc/enter/tab per level"): built from keysHintBase, the single source of
// truth for each verb's wording, with "esc back"/"tab cycle" dropped
// whenever escIsNoop/tabIsNoop reports that key does nothing at the current
// level — advertising a dead key is exactly the kind of footer confusion
// that left an operator unable to find navigation.
func (m Model) levelKeysHint() string {
	var out []string
	for _, p := range strings.Split(keysHintBase, " · ") {
		if p == "esc back" && m.escIsNoop() {
			continue
		}
		if p == "tab cycle" && m.tabIsNoop() {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, " · ")
}
