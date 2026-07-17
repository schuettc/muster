package station

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/schuettc/muster/internal/display"
	"github.com/schuettc/muster/internal/render"
)

// This file is the nav-stack's own rendering surface (spec §5-LOCK): the
// top-level View() entry point and its per-screen dispatch (renderBody), the
// breadcrumb (derived directly from the nav stack) plus the ONE canonical
// header-badge renderer used by EVERY screen (item 3), and the mailbox page
// (screen 2) — the three pieces of the "spine" this redesign's navigation
// rests on. Everything L0/L1/L2-content-specific (the project list, the
// agents list + departed bar, the agent header band + vitals slot, the
// threads table, thread reading) lives in views.go instead.

// View implements tea.Model.
func (m Model) View() string {
	breadcrumb := m.renderBreadcrumb()
	if m.helpOpen {
		return breadcrumb + "\n" + m.renderHelpOverlay() + "\n" + m.renderBottomLine()
	}
	return breadcrumb + "\n" + m.renderBody() + "\n" + m.renderBottomLine()
}

// renderBreadcrumb renders the current path (breadcrumb IS the nav stack,
// spec §5-LOCK decision B) on the left, and the header's mail badge on the
// right — ALWAYS present, on EVERY screen (spec item 3: "ONE renderer used
// by EVERY screen").
func (m Model) renderBreadcrumb() string {
	text := m.renderBreadcrumbPath()
	width := m.termWidth
	if width <= 0 {
		width = fallbackTermWidth
	}
	plainText := display.Sanitize(text, width)
	count := m.operatorInboxCount()
	badge := headerBadgeText(count, m.screen == screenProjects)
	badgeStyle := mailBadgeDimStyle
	if count > 0 {
		badgeStyle = mailBadgeStyle
	}
	// Width accounting happens on the PLAIN text (same discipline as
	// colorIntentTag/topBorder: style only AFTER every width decision is
	// final, since display.Width doesn't understand ANSI) — the badge is
	// dropped rather than corrupting the line's width when there isn't room
	// for both, mirroring joinStatusLine's rule for the bottom line.
	leftW := display.Width(plainText)
	badgeW := mailBadgeDisplayWidth(badge)
	gap := width - leftW - badgeW
	if gap < 2 {
		return breadcrumbStyle.Render(render.PadDisplay(plainText, width))
	}
	left := breadcrumbStyle.Render(plainText)
	right := badgeStyle.Render(badge)
	return left + strings.Repeat(" ", gap) + right
}

// headerBadgeText is the ONE canonical header-badge renderer (spec §5-LOCK
// item 3): gray "📬 0" when count is 0, amber "📬 N for you" on the projects
// screen or plain "📬 N" everywhere else when it's not — every screen calls
// through this same function (renderBreadcrumb), so the badge can never read
// differently depending on where the operator is standing.
func headerBadgeText(count int, onProjectsScreen bool) string {
	if count <= 0 {
		return "📬 0"
	}
	if onProjectsScreen {
		return fmt.Sprintf("📬 %d for you", count)
	}
	return fmt.Sprintf("📬 %d", count)
}

// mailBadgeDisplayWidth reports the 📬 badge's true rendered display width.
// display.Width's East Asian Wide/Fullwidth table doesn't know the mailbox
// emoji renders 2 columns wide in a real terminal — it has no matching
// codepoint range — so undercounts the badge by one column; renderBreadcrumb's
// gap math corrects for that here rather than teaching the shared display
// package about one specific emoji it was never meant to cover.
func mailBadgeDisplayWidth(badge string) int {
	return display.Width(badge) + 1
}

// renderBreadcrumbPath walks the ENTIRE nav stack (spec §5-LOCK decision B:
// "breadcrumb IS the stack, always visible"), joining each frame's own crumb
// text — the direct, mechanical consequence of a real stack: a frame pushed
// from the mailbox contributes "your mailbox", one pushed from a project's
// agent contributes that project/agent's own path, with no special-casing
// needed to keep them from bleeding into each other.
func (m Model) renderBreadcrumbPath() string {
	parts := make([]string, 0, len(m.stack))
	for _, f := range m.stack {
		if c := m.frameCrumb(f); c != "" {
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, " › ")
}

// frameCrumb renders one navFrame's own breadcrumb segment. screenRead's
// crumb uses the LIVE m.viewThreadID/conversationSubject rather than
// anything stored on the frame itself — safe because a Read frame is always
// the stack's top (Enter is a no-op at screenRead, the deepest level: there
// is never more than one on the stack at once), so it always names whatever
// is actually loaded.
func (m Model) frameCrumb(f navFrame) string {
	switch f.screen {
	case screenProjects:
		return "all projects"
	case screenProject:
		return projectDisplayName(f.project)
	case screenAgent:
		return m.dispLabel(f.agent)
	case screenMailbox:
		return "your mailbox"
	case screenRead:
		return fmt.Sprintf("#%d %s", m.viewThreadID, m.conversationSubject(m.viewThreadID))
	}
	return ""
}

// conversationSubject looks up threadID's subject among the currently loaded
// threads — "" if it isn't (yet) known.
func (m Model) conversationSubject(threadID int64) string {
	if idx := indexOfThread(m.threads, threadID); idx >= 0 {
		return m.threads[idx].Subject
	}
	return ""
}

// renderBody renders whichever screen is current: full-width thread reading
// or the mailbox (both always claim the FULL terminal, on every size — see
// readingBoxDims), the narrow-mode single list column, the threads-level
// horizontal split (header band + table stacked above a full-width
// preview), or the ordinary two-column split.
func (m Model) renderBody() string {
	if m.screen == screenRead {
		w, h := m.readingBoxDims()
		return m.renderConversationBox(w, h, true)
	}
	if m.screen == screenMailbox {
		w, h := m.readingBoxDims()
		return m.renderMailboxBox(w, h)
	}
	dims := m.layout()
	if dims.narrow {
		return m.renderLeftColumn(dims)
	}
	if dims.threadsHorizontal {
		// Threads-level layout goes horizontal: the hierarchy principle is
		// unchanged — parent above child at this level instead of left of
		// right — so the full-width THREADS table stacks above the selected
		// thread's full-width preview, rather than the vertical two-column
		// split every other level uses below. screenAgent additionally gets
		// an agent HEADER BAND above the table (spec §5-LOCK screen 4).
		if m.screen == screenAgent {
			if a, ok := m.agentByAlias(m.agent); ok {
				band := m.renderAgentHeaderBandBox(dims.leftW, a)
				return lipgloss.JoinVertical(lipgloss.Left, band, m.renderLeftColumn(dims), m.renderRightColumn(dims))
			}
		}
		return lipgloss.JoinVertical(lipgloss.Left, m.renderLeftColumn(dims), m.renderRightColumn(dims))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, m.renderLeftColumn(dims), m.renderRightColumn(dims))
}

// renderMailboxBox builds the full-width mailbox page (spec §5-LOCK screen
// 2): every thread addressed to station, newest first — unread rows bright
// (a leading "•" + the intent word, colored), read rows dimmed but kept in
// place, always re-readable.
func (m Model) renderMailboxBox(outerW, outerH int) string {
	innerW := outerW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	innerH := outerH - boxBorderRows
	if innerH < 0 {
		innerH = 0
	}
	rows := m.mailboxRows()
	q, f := m.filterQueryFor(llMailbox)

	var lines []string
	selectedLine := -1
	for _, row := range rows {
		if !rowVisible(row, m.plainMailboxRow, q, f) {
			continue
		}
		cursorMark := "  "
		if row.ID == m.mailboxSel {
			cursorMark = "> "
			selectedLine = len(lines)
		}
		lines = append(lines, m.renderMailboxLine(cursorMark, row, innerW))
	}
	if len(lines) == 0 {
		lines = append(lines, render.PadDisplay("nothing addressed to you yet", innerW))
	}
	lines = windowLines(lines, innerH, selectedLine)
	return renderBox("MAILBOX", true, outerW, outerH, lines)
}

// renderMailboxLine renders one mailbox row: unread rows get a leading "•"
// and their intent word prepended to the subject, then the WHOLE line is
// colored by intent (bright); read rows render with a blank leading marker
// and the whole line dimmed instead — re-reading one changes nothing about
// how it looks or behaves (spec §5-LOCK screen 2).
func (m Model) renderMailboxLine(cursorMark string, row listThreadRow, innerW int) string {
	unread := m.isMailUnread(row)
	from := m.dispLabel(row.FromAgent)
	age := relativeAge(time.Now(), row.LastAt)
	suffix := fmt.Sprintf("  %s · %s", from, age)

	bullet := "  "
	subject := row.Subject
	if unread {
		bullet = "• "
		if word := intentWord(row.Intent); word != "" {
			subject = word + "  " + subject
		}
	}
	prefix := cursorMark + bullet
	avail := innerW - display.Width(prefix) - display.Width(suffix)
	if avail < 1 {
		avail = 1
	}
	text := display.Sanitize(subject, avail)
	padded := render.PadDisplay(prefix+text+suffix, innerW)
	if !unread {
		return mailboxReadLineStyle.Render(padded)
	}
	return colorIntentLine(row.Intent, padded)
}
