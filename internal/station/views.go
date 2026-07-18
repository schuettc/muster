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

// This file is every L0/L1/L2-content View()-side rendering function (spec
// §5-LOCK): the project list, the agents list + departed bar, the agent
// header band + vitals slot, the threads table (plain-word intents,
// label-collision-aware WHO), thread reading, the '?' help overlay, and the
// bottom line (composer/nudge confirm/filter/status). The nav-stack spine
// itself — View()'s entry point, renderBody's per-screen dispatch, the
// breadcrumb + canonical header-badge renderer, and the mailbox page — lives
// in header.go. Nothing here mutates Model — every function here is a pure
// function of the model's current state, consumed only from View().

// agentByAlias looks up alias among the currently loaded roster.
func (m Model) agentByAlias(alias string) (agentEnriched, bool) {
	for _, a := range m.agents {
		if a.Alias == alias {
			return a, true
		}
	}
	return agentEnriched{}, false
}

// renderAgentHeaderBandBox builds screenAgent's header band (spec §5-LOCK
// screen 4): "● live · <model> · <role> · active <t>" plus the 0.6.1 vitals
// slot — a marked container that renders nothing while hasVitals is false
// (see renderVitalsLines), ready to light up once 0.6.1 wires real data onto
// agentEnriched without any layout change here.
func (m Model) renderAgentHeaderBandBox(outerW int, a agentEnriched) string {
	innerW := outerW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	dot, state := "✗", "quit"
	if a.Live {
		dot, state = "●", "live"
	}
	line := fmt.Sprintf("%s %s", dot, state)
	if a.ModelType != "" {
		line += " · " + a.ModelType
	}
	if a.Role != "" {
		line += " · " + a.Role
	}
	if ts, ok := m.lastActive[a.Alias]; ok && ts > 0 {
		line += " · active " + relativeAge(time.Now(), ts)
	}
	lines := []string{render.PadDisplay(display.Sanitize(line, innerW), innerW)}
	for _, vl := range renderVitalsLines(agentVitals{}, hasVitals, time.Now()) {
		lines = append(lines, render.PadDisplay(display.Sanitize(vl, innerW), innerW))
	}
	return renderBox(m.dispLabel(a.Alias), false, outerW, len(lines)+boxBorderRows, lines)
}

// renderVitalsLines renders the 0.6.1 vitals slot's two lines from v — the
// container's own rendering code, gated on hasVitals explicitly passed in
// (rather than reading the package const directly) so this function's own
// test can prove the path renders correctly even while the const stays
// false for all of 0.6.0 (spec §5-LOCK decision C: "the container exists,
// renders nothing today, ready to light up when 0.6.1 adds the data").
func renderVitalsLines(v agentVitals, on bool, now time.Time) []string {
	if !on {
		return nil
	}
	return []string{
		"working on  " + v.WorkingOn,
		fmt.Sprintf("usage       ctx ~%dk / %dk  %d%%  · out %dk last turn · ended %s",
			v.CtxUsedK, v.CtxWindowK, v.CtxPercent, v.OutTokensK, relativeAge(now, v.LastTurnEndedAt)),
	}
}

// renderLeftColumn renders the current screen's list: the project list (L0),
// a project's agents (L1 — or, the ONE exception, the "(unassigned)"
// bucket's ORPHANED THREADS list), or the agent's thread list (L2).
func (m Model) renderLeftColumn(dims layoutDims) string {
	switch m.screen {
	case screenAgent:
		return m.renderConvListBox(dims.leftW, dims.convListH, llAgentThreads, "THREADS")
	case screenProject:
		if m.l1IsOrphaned() {
			return m.renderConvListBox(dims.leftW, dims.convListH, llProjectItems, "ORPHANED THREADS")
		}
		return m.renderAgentsBox(dims.leftW, dims.convListH)
	default:
		return m.renderProjectsBox(dims.leftW, dims.bodyH)
	}
}

// renderRightColumn renders the current screen's PREVIEW-only right pane —
// browse mode never focuses this pane (reading is a separate full-width
// screen), so every branch here always renders unfocused: a project's
// rollup preview (L0), the selected agent's own page (L1's agents list:
// their threads + recent activity) or the selected thread's preview (L1's
// ONE exception, the unassigned bucket's ORPHANED THREADS list), or the
// agent's selected thread preview (L2).
func (m Model) renderRightColumn(dims layoutDims) string {
	switch m.screen {
	case screenAgent:
		return m.renderConversationBox(dims.rightW, dims.rightColumnHeight(), false)
	case screenProject:
		if m.l1IsOrphaned() {
			return m.renderConversationBox(dims.rightW, dims.rightColumnHeight(), false)
		}
		return m.renderAgentPagePreviewBox(dims.rightW, dims.bodyH)
	default:
		return m.renderProjectPreviewBox(dims.rightW, dims.bodyH)
	}
}

// renderProjectsBox builds L0's project list: name, live/total agents,
// unread rollup (+action), last-activity age.
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
	return renderBox("PROJECTS", true, outerW, outerH, lines)
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
		rows := unreadThreadsForProject(m.threads, aliasProjectMap(m.agents), p.Name)
		unread = fmt.Sprintf(" (%d%s%s)", p.Unread, marker, unreadAgeSuffix(rows))
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

// renderRosterRow renders one agent-list row's plain text: live dot, label
// (resolved via dispLabel, so the 'a' aliases toggle affects the list
// exactly like every other row), and per-session unread count — "!" marks a
// session whose unread includes an action-requested thread. This is the
// SAME predicate text moveCurrentList/handleEnterKey/handleNudgeKey resolve
// filter/selection through — renderRosterLine below is the padded VISUAL
// variant.
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

// renderRosterLine renders one LIVE agent-list row's VISIBLE text — a single
// line, clipped (never wrapped) to innerW: the unread block's oldest-unread
// AGE ("(N · age)"), and a trailing "last active: <relative>" once
// fetchLastActiveCmd has resolved this alias.
func (m Model) renderRosterLine(cursorMark string, a agentEnriched, innerW int) string {
	suffix := ""
	if a.Unread > 0 {
		marker := ""
		if a.Action {
			marker = "!"
		}
		suffix = fmt.Sprintf(" (%d%s%s)", a.Unread, marker, unreadAgeSuffix(unreadThreadsFor(m.threads, a.Alias)))
	}
	if ts, ok := m.lastActive[a.Alias]; ok && ts > 0 {
		suffix += "  last active: " + relativeAge(time.Now(), ts)
	}
	prefix := cursorMark + "● "
	avail := innerW - display.Width(prefix) - display.Width(suffix)
	if avail < 1 {
		avail = 1
	}
	label := display.Sanitize(m.dispLabel(a.Alias), avail)
	return render.PadDisplay(prefix+label+suffix, innerW)
}

// unreadAgeSuffix renders the "· age" fragment ("(1 · 25m)") for an unread
// block already known to be non-empty (a.Unread > 0 / p.Unread > 0) — "" when
// rows carries no derivable age.
func unreadAgeSuffix(rows []listThreadRow) string {
	oldest := oldestUnreadAt(rows)
	if oldest <= 0 {
		return ""
	}
	return " · " + relativeAge(time.Now(), oldest)
}

// sectionHeaderStyle styles renderAgentPagePreviewBox's "THREADS"/"ACTIVITY"
// section-header rows.
var sectionHeaderStyle = lipgloss.NewStyle().Faint(true).Bold(true)

// renderAgentsBox builds screenProject's L1 list (spec §5-LOCK screen 3):
// live agents on top (● green, unread badge + last-active), then — only
// when at least one agent has quit — a PLAIN divider bar (no label text at
// all: the bar and the ✗ mark are self-explaining), then quit agents by
// their REAL NAMES (the alias, not a possibly-stale display label — a
// deregistered session's tmux-derived label can never be refreshed again),
// dimmed, with a thread-count + age instead of an unread badge. The
// "(unassigned)" bucket never reaches here (l1IsOrphaned routes it to
// renderConvListBox's ORPHANED THREADS list instead, see renderLeftColumn).
func (m Model) renderAgentsBox(outerW, outerH int) string {
	innerW := outerW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	innerH := outerH - boxBorderRows
	if innerH < 0 {
		innerH = 0
	}
	q, f := m.filterQueryFor(llProjectItems)

	var live, quit []agentEnriched
	for _, a := range m.agentStripRows() {
		if !rowVisible(a, m.renderRosterRow, q, f) {
			continue
		}
		if a.Live {
			live = append(live, a)
		} else {
			quit = append(quit, a)
		}
	}

	var lines []string
	selectedLine := -1
	for _, a := range live {
		cursorMark := "  "
		if a.Alias == m.agent {
			cursorMark = "> "
			selectedLine = len(lines)
		}
		lines = append(lines, m.renderRosterLine(cursorMark, a, innerW))
	}
	if len(quit) > 0 {
		lines = append(lines, quitDividerStyle.Render(strings.Repeat("─", innerW)))
		for _, a := range quit {
			cursorMark := "  "
			if a.Alias == m.agent {
				cursorMark = "> "
				selectedLine = len(lines)
			}
			lines = append(lines, m.renderQuitAgentLine(cursorMark, a, innerW))
		}
	}

	lines = windowLines(lines, innerH, selectedLine)
	return renderBox("AGENTS", true, outerW, outerH, lines)
}

// renderQuitAgentLine renders one departed agent's row: "✗ <alias>  N
// threads  age" — dimmed in full (spec §5-LOCK decision A).
func (m Model) renderQuitAgentLine(cursorMark string, a agentEnriched, innerW int) string {
	n := len(conversationsForAgent(m.threads, a.Alias))
	age := relativeAge(time.Now(), lastActivityAt(m.threads, a.Alias))
	suffix := fmt.Sprintf("  %d threads  %s", n, age)
	prefix := cursorMark + "✗ "
	avail := innerW - display.Width(prefix) - display.Width(suffix)
	if avail < 1 {
		avail = 1
	}
	label := display.Sanitize(a.Alias, avail) // REAL NAME: the alias, never a stale label
	padded := render.PadDisplay(prefix+label+suffix, innerW)
	return quitAgentLineStyle.Render(padded)
}

// renderAgentPagePreviewBox builds screenProject's right-pane preview for
// L1's selected agent (their threads + recent activity) — a compact
// combined preview reusing conversationsForAgent and agentActivity/
// render.Renderer verbatim, distinct from the fuller screenAgent drill-down
// Enter on this row leads to.
func (m Model) renderAgentPagePreviewBox(outerW, outerH int) string {
	innerW := outerW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	innerH := outerH - boxBorderRows
	if innerH < 0 {
		innerH = 0
	}
	alias := m.agent
	title := "AGENT: " + m.dispLabel(alias)
	if ts, ok := m.lastActive[alias]; ok && ts > 0 {
		title += " — last active " + relativeAge(time.Now(), ts) + " ago"
	}
	if alias == "" {
		return renderBox(title, false, outerW, outerH, []string{render.PadDisplay("no agent selected", innerW)})
	}

	var lines []string
	lines = append(lines, sectionHeaderStyle.Render(render.PadDisplay("THREADS", innerW)))
	threadRows := groupThreads(conversationsForAgent(m.threads, alias))
	if len(threadRows) == 0 {
		lines = append(lines, render.PadDisplay("no threads", innerW))
	}
	for _, r := range threadRows {
		lines = append(lines, render.PadDisplay(display.Sanitize(m.renderThreadRow(r), innerW), innerW))
	}

	lines = append(lines, sectionHeaderStyle.Render(render.PadDisplay("ACTIVITY", innerW)))
	m.activity.SetWidth(innerW)
	activityRows := agentActivity(m.events, alias)
	for _, e := range activityRows {
		var lb bytes.Buffer
		m.activity.Line(&lb, e)
		line := strings.TrimRight(lb.String(), "\n")
		lines = append(lines, render.PadDisplay(display.Sanitize(line, innerW), innerW))
	}

	lines = windowLines(lines, innerH, len(lines)-1) // keep the TAIL (most recent activity) in view
	return renderBox(title, false, outerW, outerH, lines)
}

// renderWho renders a thread's participant field as "<from><arrow><to>",
// collapsing to "<name> · to self" whenever from and to resolve to the SAME
// agent identity (threadIsSelfSend, nav.go) — a literal "x→x" reads like a
// duplicate-send bug, not a message an agent sent itself. arrow is the
// caller's own separator (tightly packed "→" in the columnized THREADS
// table, spaced " → " in the plain filter/preview text) — the ONE shared
// helper both call sites go through, so self-send collapses identically
// everywhere a thread's WHO is rendered.
func (m Model) renderWho(row listThreadRow, arrow string) string {
	from := m.dispLabel(row.FromAgent)
	if threadIsSelfSend(row) {
		return from + " · to self"
	}
	return from + arrow + m.dispToTarget(row)
}

// renderThreadRow renders one thread row's PLAIN text — the filter/
// selection predicate's text, reused verbatim by both screenProject's
// "(unassigned)" thread list and screenAgent's thread list
// (conversationRows() already picks the right underlying rows for whichever
// screen is active).
func (m Model) renderThreadRow(row listThreadRow) string {
	marker := "  "
	if row.ID == m.conversation {
		marker = "> "
	}
	word := intentWord(row.Intent)
	if word != "" {
		word = " " + word
	}
	participants := m.renderWho(row, " → ")
	last := m.dispLabel(row.LastFrom)
	age := relativeAge(time.Now(), row.LastAt)
	subject := display.Sanitize(row.Subject, 200)
	return fmt.Sprintf("%s#%d%s %s | %s %s | %s", marker, row.ID, word, participants, last, age, subject)
}

// threadWhoContentWidth is WHO's column-width fix (operator finding: a flat
// 1/6-of-innerW share truncated long labels on a wide terminal while SUBJECT
// padded out empty columns). It measures the WIDEST rendered WHO string
// (display.Width, the same units render.PadDisplay uses) across ALL of
// rows — not just whichever ones the '/' filter or the current scroll
// window happen to show — so the column's width is stable for the whole
// table render: neither filtering nor scrolling ever re-jigs it mid-session,
// which either would if this were measured over a post-filter or
// post-window subset instead.
func (m Model) threadWhoContentWidth(rows []conversationRow) int {
	widest := 0
	for _, c := range rows {
		if w := display.Width(m.renderWho(c.listThreadRow, "→")); w > widest {
			widest = w
		}
	}
	return widest
}

// renderConversationLine renders one thread row COLUMNIZED: `#ID intent
// who → who  AGE  subject`, appending a cross-project marker to the subject
// when the row touches another project (computed against the AGENT viewing
// it, see conversationsForAgentAnnotated). WHO resolves both ends through
// dispLabel, which appends the alias itself whenever a label collides (spec
// §5-LOCK item 7) — so a cross-project or ambiguous-label thread never
// reads like nonsense. maxWhoContent is the table's own WHO-width input (see
// threadWhoContentWidth) — every row in one table render must be passed the
// SAME value (renderConvListBox computes it once and shares it with the
// header), or WHO won't line up between rows.
func (m Model) renderConversationLine(c conversationRow, innerW, maxWhoContent int) string {
	return m.renderConversationLineMarked(c, innerW, maxWhoContent, c.ID == m.conversation)
}

// renderConversationLineMarked is renderConversationLine with an EXPLICIT
// selected flag rather than one implied by c.ID == m.conversation.
func (m Model) renderConversationLineMarked(c conversationRow, innerW, maxWhoContent int, selected bool) string {
	marker := "  "
	if selected {
		marker = "> "
	}
	whoW, fixedWidth := threadsColumnWidths(innerW, maxWhoContent)
	idCol := render.PadDisplay(display.Sanitize(fmt.Sprintf("#%d", c.ID), threadIDWidth), threadIDWidth)
	wordPlain := intentWord(c.Intent)
	intentCol := colorIntentTag(c.Intent, render.PadDisplay(wordPlain, threadTagWidth))
	who := m.renderWho(c.listThreadRow, "→")
	whoCol := render.PadDisplay(display.Sanitize(who, whoW), whoW)
	ageCol := render.PadDisplay(relativeAge(time.Now(), c.LastAt), threadAgeWidth)

	subject := c.Subject
	if len(c.OtherProjects) > 0 {
		names := make([]string, len(c.OtherProjects))
		for i, p := range c.OtherProjects {
			names[i] = projectDisplayName(p)
		}
		subject += "  ↔ " + strings.Join(names, ",")
	}
	subjectBudget := innerW - fixedWidth
	if subjectBudget < 0 {
		subjectBudget = 0
	}
	subjectCol := render.PadDisplay(display.Sanitize(subject, subjectBudget), subjectBudget)

	return marker + idCol + "  " + intentCol + "  " + whoCol + "  " + ageCol + "  " + subjectCol
}

// renderConvListBox builds a thread-list box (shared by screenProject's
// "(unassigned)" ORPHANED THREADS list and screenAgent's THREADS) — rows
// already grouped action-requested-first by conversationRows(), each
// columnized (renderConversationLine), the selection marker following
// m.conversation by ID so it always lands on the right row regardless of
// this poll's grouping. Rows the active '/' filter hides are skipped via
// rowVisible — the SAME predicate moveCurrentList/handleEnterKey resolve
// through — and the visible rows are vertically windowed around the
// selection. Always the sole browsing target at its level (spec §5-LOCK
// decision B: no second focus target left anywhere), so always rendered
// "focused".
func (m Model) renderConvListBox(outerW, outerH int, list llList, title string) string {
	innerW := outerW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	rowsHeight := outerH - boxBorderRows - 1 // -1 for the header row below
	if rowsHeight < 0 {
		rowsHeight = 0
	}

	rows := m.conversationRows()
	// maxWhoContent is measured over rows — the table's FULL, pre-filter row
	// set — once per render, then shared by the header and every data row
	// below, so WHO's width can never depend on (or shift with) the '/'
	// filter or the scroll window.
	maxWhoContent := m.threadWhoContentWidth(rows)
	header := render.PadDisplay(display.Sanitize(threadsHeaderLine(innerW, maxWhoContent), innerW), innerW)

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
		body = append(body, m.renderConversationLine(c, innerW, maxWhoContent))
	}
	body = windowLines(body, rowsHeight, selectedLine)
	lines := append([]string{header}, body...)
	return renderBox(title, true, outerW, outerH, lines)
}

// conversationAuthorStyle styles a thread entry's header line ("author ·
// relative time"), distinctly from the plain (or markdown-styled) body text
// under it.
var conversationAuthorStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("117"))

// conversationLines renders every loaded thread entry into flat, already-
// wrapped display lines (header + wrapped/styled body + a blank separator),
// and returns entryStart[i]..entryStart[i+1] as entry i's line range within
// lines — entryStart has len(viewEntries)+1 elements, the last one the total
// line count.
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
// reader is FOCUSED (screenRead): the window always ends right at the
// cursor entry's own lines. Stateless by design: the cursor's own index is
// enough to re-derive the correct window on every render.
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
// in entryStart — the passive preview "should start at a MESSAGE boundary...
// not mid-word" even when a straight tail-height slice would otherwise land
// inside a message's wrapped body. If every real boundary is already behind
// top, it falls back to that last entry's own start instead — showing all of
// one long message from its header rather than an empty or mid-body window.
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

// renderConversationBox builds the right pane's thread content — the
// passive "last messages" preview (focused=false: just the tail, no cursor
// marks, no load-older/newer hints) or screenRead's focused reader
// (focused=true: cursor-windowed, "load older"/"N newer" hints).
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
		// but snapped forward to the nearest message boundary so it never
		// opens mid-paragraph/mid-word.
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

// llListName renders a llList value for the '/' filter's "filter (X): …"
// prompt. llProjectItems is model-dependent: the "(unassigned)" bucket's
// ORPHANED THREADS exception filters threads, every other project's L1
// filters agents.
func (m Model) llListName(l llList) string {
	switch l {
	case llProjects:
		return "projects"
	case llProjectItems:
		if m.l1IsOrphaned() {
			return "orphaned threads"
		}
		return "agents"
	case llAgentThreads:
		return "threads"
	case llMailbox:
		return "mailbox"
	default:
		return ""
	}
}

// helpKeyLines is the '?' overlay's key reference.
var helpKeyLines = []string{
	"enter    on an agent: descend into their threads · on a thread: read it full-width",
	"esc      climb back up one level (or leave full-width reading)",
	"g        home — jump straight back to the projects list",
	"j/k, ↑/↓ move the cursor in the current list",
	"end, G   jump the focused thread reader to its newest entry",
	"s        send a message from anywhere (roster-filtered picker)",
	"r        reply to the currently selected/open thread",
	"n        nudge (the agents list, or an agent's own page)",
	"m        jump to your mailbox — every thread addressed to you, read and unread",
	"/        filter the current left list",
	"a        toggle raw aliases vs. current labels",
	"?        toggle this help",
	"q        quit (deregisters this station)",
}

// helpLegendLines is the glyph legend.
var helpLegendLines = []string{
	"●        agent is live",
	"✗        agent has quit (dimmed; history stays in place, still drillable)",
	"(n)      n unread messages for that session",
	"(n · age) n unread, oldest waiting since age",
	"!        the unread includes an action-requested thread",
	"needs action / wants reply / fyi   a thread's intent, in plain words",
	"↔ proj   this thread also touches another project",
	"📬 N for you   station's own unread mail (gray 0 when clear)",
	"ORPHANED THREADS   the (unassigned) bucket: threads with no living agent to file under",
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
		return fmt.Sprintf("filter (%s): %s", m.llListName(m.filter.list), m.filter.input.View())
	default:
		return m.renderStatus()
	}
}

// renderComposerPicker renders the 's' target-picker line: the filter input
// plus the (label-resolved) candidates it currently matches, the highlighted
// one marked. Two candidates sharing the same display label are already
// disambiguated by dispLabel itself (spec §5-LOCK item 7: the ONE shared
// collision helper's alias-suffix form, not a second picker-local
// vocabulary).
func (m Model) renderComposerPicker() string {
	cands := m.composerCandidates()
	names := make([]string, 0, len(cands))
	for i, a := range cands {
		marker := ""
		if i == m.composer.pickerIdx {
			marker = ">"
		}
		names = append(names, marker+m.dispLabel(a.Alias))
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
	if m.screen == screenRead {
		right = fmt.Sprintf("%s scroll · %s reply · %s back · g home", keys.Down.Help().Key, keys.Reply.Help().Key, keys.Esc.Help().Key)
	}
	return joinStatusLine(left, right, width)
}

// escIsNoop reports whether Esc does nothing at the model's CURRENT level —
// true at L0 (screenProjects: nothing to climb back to) and at L1 when the
// bus auto-skipped straight past L0 (m.singleProject: there is no L0 to
// climb back to either) — the exact inverse of canPop.
func (m Model) escIsNoop() bool {
	return !m.canPop()
}

// gIsNoop reports whether 'g' does nothing right now: already at home (the
// true root, or — on a single-project bus — the auto-skipped L1 that stands
// in for home).
func (m Model) gIsNoop() bool {
	if m.singleProject {
		return len(m.stack) <= 2
	}
	return len(m.stack) <= 1
}

// levelKeysHint is the DEFAULT (non-screenRead) bottom-line key hint: built
// from keysHintBase, the single source of truth for each verb's wording,
// with "esc back"/"g home" dropped whenever escIsNoop/gIsNoop reports that
// key does nothing at the current level — advertising a dead key is exactly
// the kind of footer confusion that once left an operator unable to find
// navigation.
func (m Model) levelKeysHint() string {
	var out []string
	for _, p := range strings.Split(keysHintBase, " · ") {
		if p == "esc back" && m.escIsNoop() {
			continue
		}
		if p == "g home" && m.gIsNoop() {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, " · ")
}
