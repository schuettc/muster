// Package display is the one canonical sanitizer for anything a hostile or
// careless bus payload (subject, body, reply preview) could put in front of
// an operator's terminal: the renderer, journal preview columns, and the
// station TUI all funnel through Sanitize rather than each rolling their own
// escaping. It has no dependency on internal/humancli or internal/daemon so
// the daemon can sanitize at journal time without importing the CLI.
package display

import "unicode"

// Sanitize strips control characters that could corrupt a terminal or a TUI
// pane — C0/C1 controls, NUL, full ESC/CSI escape sequences, and Unicode bidi
// override characters (U+202A-202E, U+2066-2069) — collapses runs of
// tab/newline/CR into a single space, and truncates the result to maxWidth
// display columns (wide/combining runes counted properly, not rune count),
// appending '…' when truncation actually cuts content. maxWidth <= 0 yields
// "" (there is no room even for the ellipsis).
func Sanitize(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	runes := []rune(s)
	cleaned := make([]rune, 0, len(runes))
	inWSRun := false
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == 0x1B { // ESC: consume the whole sequence, never emit anything for it
			i = skipEscape(runes, i)
			inWSRun = false
			continue
		}
		if r == '\t' || r == '\n' || r == '\r' {
			inWSRun = true
			continue
		}
		if inWSRun {
			cleaned = append(cleaned, ' ')
			inWSRun = false
		}
		switch {
		case r < 0x20 || r == 0x7F: // other C0 controls + DEL
			continue
		case r >= 0x80 && r <= 0x9F: // C1 controls
			continue
		case isBidiControl(r):
			continue
		default:
			cleaned = append(cleaned, r)
		}
	}
	if inWSRun {
		cleaned = append(cleaned, ' ')
	}
	return truncateToWidth(cleaned, maxWidth)
}

// skipEscape returns the index of the LAST rune consumed by the escape
// sequence starting at runes[i] (runes[i] itself is ESC, 0x1B): a CSI
// sequence (ESC '[' followed by any number of parameter/intermediate bytes,
// then one final byte in 0x40-0x7E) consumes through that final byte; any
// other ESC is a two-rune sequence (ESC + the one rune following it); a
// trailing ESC with nothing after it consumes just itself. An unterminated
// CSI (no final byte before the string ends) consumes to the end of input —
// better to drop a truncated escape entirely than leak its parameter bytes.
func skipEscape(runes []rune, i int) int {
	if i+1 >= len(runes) {
		return i
	}
	if runes[i+1] == '[' {
		j := i + 2
		for j < len(runes) {
			c := runes[j]
			if c >= 0x40 && c <= 0x7E {
				return j
			}
			j++
		}
		return len(runes) - 1
	}
	return i + 1
}

// isBidiControl reports whether r is one of the Unicode bidi override/isolate
// controls that can be used to visually reorder or hide text in a terminal.
func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// wideRanges are the East Asian Wide/Fullwidth blocks counted as display
// width 2. This is a small explicit table rather than a golang.org/x/text
// dependency (muster stays dependency-light); it is not a complete East
// Asian Width implementation, just the common blocks operators actually see.
var wideRanges = [][2]rune{
	{0x1100, 0x115F},   // Hangul Jamo
	{0x2E80, 0xA4CF},   // CJK Radicals through Yi (covers CJK Unified Ideographs)
	{0xAC00, 0xD7A3},   // Hangul Syllables
	{0xF900, 0xFAFF},   // CJK Compatibility Ideographs
	{0xFE30, 0xFE4F},   // CJK Compatibility Forms
	{0xFF00, 0xFF60},   // Fullwidth Forms
	{0xFFE0, 0xFFE6},   // Fullwidth Signs
	{0x20000, 0x2FFFD}, // CJK Unified Ideographs Extension B..
	{0x30000, 0x3FFFD}, // CJK Unified Ideographs Extension G..
}

// runeWidth returns r's terminal display width: 0 for combining marks
// (unicode.Mn, Me — they render stacked on the previous rune), 2 for East
// Asian Wide/Fullwidth (wideRanges), 1 for everything else printable.
func runeWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return 0
	}
	for _, rg := range wideRanges {
		if r >= rg[0] && r <= rg[1] {
			return 2
		}
	}
	return 1
}

// Width returns the total terminal display width of s (see runeWidth).
func Width(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// truncateToWidth returns runes as a string if its total display width
// already fits maxWidth, otherwise cuts it to the longest prefix whose width
// plus the ellipsis's width (1) still fits, and appends '…'.
func truncateToWidth(runes []rune, maxWidth int) string {
	widths := make([]int, len(runes))
	total := 0
	for i, r := range runes {
		widths[i] = runeWidth(r)
		total += widths[i]
	}
	if total <= maxWidth {
		return string(runes)
	}
	budget := maxWidth - 1
	if budget < 0 {
		budget = 0
	}
	w := 0
	cut := 0
	for cut < len(runes) {
		next := w + widths[cut]
		if next > budget {
			break
		}
		w = next
		cut++
	}
	return string(runes[:cut]) + "…"
}
