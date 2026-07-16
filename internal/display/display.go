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
	// Work with bytes first to preserve 8-bit C1 introducers, then convert to runes
	bytes := []byte(s)
	cleaned := make([]rune, 0, len(bytes))
	inWSRun := false

	for i := 0; i < len(bytes); i++ {
		b := bytes[i]

		// Handle ESC sequences (0x1B)
		if b == 0x1B {
			i = skipEscapeBytes(bytes, i)
			inWSRun = false
			continue
		}

		// Handle 8-bit C1 introducers (structured payloads, not plain controls)
		if b == 0x9B { // 8-bit CSI
			i = skip8BitCSIBytes(bytes, i)
			inWSRun = false
			continue
		}
		if b == 0x9D || b == 0x90 || b == 0x9E || b == 0x9F { // 8-bit OSC/DCS/PM/APC
			i = skip8BitPayloadBytes(bytes, i)
			inWSRun = false
			continue
		}

		// Handle whitespace
		if b == '\t' || b == '\n' || b == '\r' {
			inWSRun = true
			continue
		}

		// Emit space if we were in a whitespace run
		if inWSRun {
			cleaned = append(cleaned, ' ')
			inWSRun = false
		}

		// Handle C0/C1 control bytes directly
		if b < 0x20 || b == 0x7F || (b >= 0x80 && b <= 0x9F) {
			continue
		}

		// For valid UTF-8 starting bytes (0x00-0x7F handled above, 0x80-0xBF are continuation)
		// Try to decode a UTF-8 rune
		r, width := decodeUTF8Byte(bytes, i)
		if width > 0 {
			// Check for bidi controls and other problematic runes
			if !isBidiControl(r) && !isUnicodeControl(r) {
				cleaned = append(cleaned, r)
			}
			i += width - 1 // -1 because the loop will increment i
			continue
		}

		// If we couldn't decode UTF-8, skip this byte (it's an invalid continuation byte)
	}

	if inWSRun {
		cleaned = append(cleaned, ' ')
	}
	return truncateToWidth(cleaned, maxWidth)
}

// decodeUTF8Byte decodes a UTF-8 rune starting at bytes[i]. Returns the rune and the width
// in bytes (1-4), or 0, 0 if the byte is not a valid UTF-8 start byte.
func decodeUTF8Byte(bytes []byte, i int) (rune, int) {
	b := bytes[i]

	// ASCII (0xxxxxxx)
	if b < 0x80 {
		return rune(b), 1
	}

	// Continuation bytes (10xxxxxx) are not valid start bytes
	if b < 0xC0 {
		return 0, 0
	}

	// 2-byte sequence (110xxxxx)
	if b < 0xE0 {
		if i+1 >= len(bytes) {
			return 0, 0
		}
		b2 := bytes[i+1]
		if b2&0xC0 != 0x80 {
			return 0, 0
		}
		r := rune(b&0x1F)<<6 | rune(b2&0x3F)
		return r, 2
	}

	// 3-byte sequence (1110xxxx)
	if b < 0xF0 {
		if i+2 >= len(bytes) {
			return 0, 0
		}
		b2 := bytes[i+1]
		b3 := bytes[i+2]
		if b2&0xC0 != 0x80 || b3&0xC0 != 0x80 {
			return 0, 0
		}
		r := rune(b&0x0F)<<12 | rune(b2&0x3F)<<6 | rune(b3&0x3F)
		return r, 3
	}

	// 4-byte sequence (11110xxx)
	if b < 0xF8 {
		if i+3 >= len(bytes) {
			return 0, 0
		}
		b2 := bytes[i+1]
		b3 := bytes[i+2]
		b4 := bytes[i+3]
		if b2&0xC0 != 0x80 || b3&0xC0 != 0x80 || b4&0xC0 != 0x80 {
			return 0, 0
		}
		r := rune(b&0x07)<<18 | rune(b2&0x3F)<<12 | rune(b3&0x3F)<<6 | rune(b4&0x3F)
		return r, 4
	}

	return 0, 0
}

// isUnicodeControl reports whether r is a Unicode control character.
func isUnicodeControl(r rune) bool {
	return r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F)
}

// skipEscapeBytes returns the index of the last byte consumed by the escape sequence
// starting at bytes[i] (bytes[i] itself is ESC, 0x1B):
// - CSI (ESC '[') consumes parameter/intermediate bytes then one final byte in 0x40-0x7E
// - OSC/DCS/PM/APC (ESC ']'/P/^/_) consume until BEL (0x07) or ST (ESC \) or end
// - Any other ESC is consumed along with the following byte
// - A trailing ESC with nothing after it consumes just itself
// An unterminated sequence consumes to the end of input.
func skipEscapeBytes(bytes []byte, i int) int {
	if i+1 >= len(bytes) {
		return i
	}

	c := bytes[i+1]

	// CSI
	if c == '[' {
		j := i + 2
		for j < len(bytes) {
			if bytes[j] >= 0x40 && bytes[j] <= 0x7E {
				return j
			}
			j++
		}
		return len(bytes) - 1
	}

	// OSC / DCS / PM / APC: consume until BEL (0x07) or ST (ESC \) or end
	if c == ']' || c == 'P' || c == '^' || c == '_' {
		j := i + 2
		for j < len(bytes) {
			if bytes[j] == 0x07 { // BEL
				return j
			}
			// ST: ESC \
			if bytes[j] == 0x1B && j+1 < len(bytes) && bytes[j+1] == '\\' {
				return j + 1
			}
			j++
		}
		return len(bytes) - 1
	}

	// Any other ESC is consumed along with the following byte
	return i + 1
}

// skip8BitCSIBytes consumes a CSI sequence starting with the 8-bit introducer (0x9B).
// Similar to ESC '[', it consumes parameter/intermediate bytes then one final byte in 0x40-0x7E
func skip8BitCSIBytes(bytes []byte, i int) int {
	j := i + 1
	for j < len(bytes) {
		if bytes[j] >= 0x40 && bytes[j] <= 0x7E {
			return j
		}
		j++
	}
	return len(bytes) - 1
}

// skip8BitPayloadBytes consumes an OSC/DCS/PM/APC sequence starting with an 8-bit introducer
// (0x9D, 0x90, 0x9E, 0x9F respectively).
// Consumes until BEL (0x07) or ST (ESC \) or end of input
func skip8BitPayloadBytes(bytes []byte, i int) int {
	j := i + 1
	for j < len(bytes) {
		if bytes[j] == 0x07 { // BEL
			return j
		}
		// ST: ESC \
		if bytes[j] == 0x1B && j+1 < len(bytes) && bytes[j+1] == '\\' {
			return j + 1
		}
		j++
	}
	return len(bytes) - 1
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
