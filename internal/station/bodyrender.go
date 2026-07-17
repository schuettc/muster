package station

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/schuettc/muster/internal/display"
)

// This file is the conversation view's body-rendering path (iteration-three
// fix, driven by an operator screenshot: bodies flattened by the journal's
// one-line display.Sanitize came out as unreadable bricks, raw "**"/"`"
// punctuation and all). It is deliberately separate from the one-line
// journal path (display.Sanitize, still used verbatim by every list/status
// row in views.go) — conversation bodies get their own newline-preserving
// clean (display.SanitizeLines), markdown styling applied BEFORE wrapping,
// and lipgloss's own ANSI-aware word-wrap to lay the result out, none of
// which the single-row contract can accommodate.

// mdBoldStyle and mdCodeStyle are the two markdown spans styleMarkdownLine
// recognizes — minimal and safe (spec: "a ~50-line styler with tests, not a
// markdown engine"). Colors are picked to read distinctly from the intent
// tag colors in layout.go (196 action, 221 reply) without competing with
// them.
var (
	mdBoldStyle = lipgloss.NewStyle().Bold(true)
	mdCodeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("109"))
)

// bodyBulletIndent is the two-space chrome indent every message body line
// sits under its author header (spec §3: "body indented two spaces under
// it").
const bodyBulletIndent = "  "

// bodyHardCapLines bounds how many wrapped lines a single message body can
// ever contribute to the conversation view — pagination already bounds how
// many ENTRIES load (threadViewPageSize), but nothing previously bounded a
// single absurdly long entry's own line count, which could otherwise blow
// through the pane (and the render loop's own working set) on a hostile or
// runaway body.
const bodyHardCapLines = 200

// styleMarkdownLine applies muster's minimal, safe markdown styling to one
// control-clean line (no embedded newlines): "**bold**" renders bold with
// the asterisks dropped, "`code`" renders in a distinct color with the
// backticks dropped. Anything it doesn't recognize — unterminated markers,
// a stray lone "*", "****" (empty bold), nested punctuation it can't
// confidently pair — passes through as plain literal text rather than
// guessing; this is a styler, not a parser, and a wrong guess (eating
// punctuation that wasn't markup) is worse than leaving it alone.
//
// Styling runs BEFORE wrapping (see wrapBodyLine): a multi-word span (e.g.
// "**quite     important**") survives being split across a wrapped line
// boundary because lipgloss's own Width-based Render is itself ANSI-aware —
// it reflows the ALREADY-styled string, reopening the span's escape codes on
// whichever wrapped row it lands on — whereas wrapping first and styling
// each wrapped fragment independently would miss a span whose open/close
// markers end up on different rows.
func styleMarkdownLine(s string) string {
	var b strings.Builder
	runes := []rune(s)
	n := len(runes)
	for i := 0; i < n; {
		if i+1 < n && runes[i] == '*' && runes[i+1] == '*' {
			if end := findMarkerClose(runes, i+2, "**"); end >= 0 {
				b.WriteString(mdBoldStyle.Render(string(runes[i+2 : end])))
				i = end + 2
				continue
			}
		}
		if runes[i] == '`' {
			if end := findMarkerClose(runes, i+1, "`"); end >= 0 {
				b.WriteString(mdCodeStyle.Render(string(runes[i+1 : end])))
				i = end + 1
				continue
			}
		}
		b.WriteRune(runes[i])
		i++
	}
	return b.String()
}

// findMarkerClose scans runes for the next occurrence of marker at or after
// from, returning its start index or -1 if marker never recurs before the
// line ends (an unterminated marker) or recurs immediately (from == the
// match, i.e. empty content like "****" or a doubled backtick) — both left
// for the caller
// to treat as literal text rather than a zero-width styled span.
func findMarkerClose(runes []rune, from int, marker string) int {
	m := []rune(marker)
	for i := from; i+len(m) <= len(runes); i++ {
		match := true
		for j, mr := range m {
			if runes[i+j] != mr {
				match = false
				break
			}
		}
		if match {
			if i == from {
				return -1
			}
			return i
		}
	}
	return -1
}

// bulletPrefix returns the leading marker text (including its trailing
// space) for a line recognized as a list item — "- ", "* ", or an ordered
// "N. " marker (one or more digits, then ". ") — and "" for anything else.
// wrapBodyLine uses its display width as the hanging indent for continuation
// lines, so a wrapped bullet still reads as one item.
func bulletPrefix(line string) string {
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
		return line[:2]
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(line) && line[i] == '.' && line[i+1] == ' ' {
		return line[:i+2]
	}
	return ""
}

// wrapBodyLine styles (styleMarkdownLine), then word-wraps, one control-
// clean, newline-free source line to width display columns, via lipgloss's
// own Width-based Render: it word-wraps (hard-splitting a single token
// wider than a line rather than ever overflowing), stays ANSI-aware across
// the wrap, and pads every resulting row to the wrap width — the exact
// machinery the pre-fix code already trusted for plain text, now handed
// styled (markdown-rendered) text too. A recognized list marker
// (bulletPrefix) hang-indents continuation rows by the marker's width: the
// wrap runs at (width - indentWidth) throughout, and continuation rows get
// the indent prepended while row 0 gets the same indentWidth appended as
// trailing padding, so every returned row is exactly width columns wide.
func wrapBodyLine(line string, width int) []string {
	if width < 1 {
		width = 1
	}
	indentW := 0
	if prefix := bulletPrefix(line); prefix != "" {
		if w := display.Width(prefix); width-w >= 1 {
			indentW = w
		}
	}
	wrapWidth := width - indentW
	styled := styleMarkdownLine(line)
	wrapped := lipgloss.NewStyle().Width(wrapWidth).Render(styled)
	rows := strings.Split(wrapped, "\n")
	if indentW == 0 {
		return rows
	}
	indent := strings.Repeat(" ", indentW)
	pad := strings.Repeat(" ", indentW)
	for i := range rows {
		if i == 0 {
			rows[i] += pad
		} else {
			rows[i] = indent + rows[i]
		}
	}
	return rows
}

// wrapBody cleans body (display.SanitizeLines — the newline-preserving
// cousin of the one-line journal Sanitize) and word-wraps every resulting
// line to width, preserving blank lines as empty output lines (renderBox
// blank-fills a bare "" to the pane's width — see conversationLines' doc)
// and hard-capping the total at bodyHardCapLines (an absurdly long single
// body gets truncated with a marker line rather than ever growing the pane
// — or this function's own working set — without bound).
func wrapBody(body string, width int) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	for _, rl := range display.SanitizeLines(body) {
		if rl == "" {
			out = append(out, "")
		} else {
			out = append(out, wrapBodyLine(rl, width)...)
		}
		if len(out) >= bodyHardCapLines {
			out = out[:bodyHardCapLines]
			out = append(out, "… (truncated)")
			break
		}
	}
	return out
}
