// Package display is the one canonical sanitizer for anything a hostile or
// careless bus payload (subject, body, reply preview) could put in front of
// an operator's terminal: the renderer, journal preview columns, and the
// station TUI all funnel through Sanitize rather than each rolling their own
// escaping. It has no dependency on internal/humancli or internal/daemon so
// the daemon can sanitize at journal time without importing the CLI.
package display

import (
	"strings"
	"unicode"
)

// Sanitize strips control characters that could corrupt a terminal or a TUI
// pane — C0/C1 controls, NUL, full ESC/CSI escape sequences, and Unicode bidi
// override characters (U+202A-202E, U+2066-2069) — collapses runs of
// tab/newline/CR into a single space, and truncates the result to maxWidth
// display columns (wide/combining runes counted properly, not rune count),
// appending '…' when truncation actually cuts content. maxWidth <= 0 yields
// "" (there is no room even for the ellipsis). This is the one-line contract:
// every caller that renders a single terminal row (journal preview columns,
// the station TUI's list/status rows) funnels through this. A caller that
// needs to keep a multi-line body's paragraph/list structure — the station
// conversation view's message bodies — wants SanitizeLines instead, which
// shares this function's exact character-level cleaning via cleanControls
// but preserves newlines rather than collapsing them to spaces.
func Sanitize(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	return truncateToWidth(cleanControls(s, cleanCollapseNewlines), maxWidth)
}

// SanitizeLines cleans a multi-line body with the SAME character-level
// scrubbing Sanitize uses — C0/C1 controls, NUL, full ESC/CSI escape
// sequences, 8-bit and UTF-8-encoded C1 introducers, and Unicode bidi
// override characters — but, unlike Sanitize, preserves newlines (and blank
// lines) instead of collapsing them to a single space, then splits the
// result into lines. Intra-line runs of tab/CR still collapse to a single
// space exactly as Sanitize's do; only '\n' survives as a literal line
// break. No width cap: callers (the station conversation body path) wrap
// each returned line to their pane's width themselves, since word-wrapping a
// multi-line body needs per-line control the single maxWidth-and-truncate
// contract above doesn't offer.
func SanitizeLines(s string) []string {
	return strings.Split(string(cleanControls(s, cleanPreserveNewlines)), "\n")
}

// cleanNewlineMode selects how cleanControls' shared character-level
// cleaning treats '\n' (and the whitespace runs it can be part of):
// cleanCollapseNewlines folds any run of tab/newline/CR into a single space
// (Sanitize's one-line contract); cleanPreserveNewlines keeps '\n' as a
// literal line break, only collapsing intra-line tab/CR runs to a single
// space (SanitizeLines' multi-line contract).
type cleanNewlineMode int

const (
	cleanCollapseNewlines cleanNewlineMode = iota
	cleanPreserveNewlines
)

// cleanControls is Sanitize's and SanitizeLines' shared char-level cleaner —
// the one canonical implementation of the control-character scrubbing both
// modes need, differing only in how '\n' is handled (see cleanNewlineMode).
// Work with bytes first to preserve 8-bit C1 introducers, then convert to
// runes.
func cleanControls(s string, mode cleanNewlineMode) []rune {
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

		// Raw (non-UTF-8-encoded) 8-bit C1 introducer bytes. 0x9B/0x9D/0x90/
		// 0x9E/0x9F are themselves invalid UTF-8 start bytes (they match the
		// continuation-byte pattern 10xxxxxx), so decodeUTF8Byte below would
		// never surface them as a decoded rune when they appear raw — this
		// is the "harmless, covers non-JSON callers" path from 5912665, kept
		// for callers that hand Sanitize a raw byte stream rather than a
		// JSON-decoded string.
		if b == 0x9B { // 8-bit CSI
			i = consumeCSIPayload(bytes, i+1)
			inWSRun = false
			continue
		}
		if b == 0x9D || b == 0x90 || b == 0x9E || b == 0x9F { // 8-bit OSC/DCS/PM/APC
			i = consumeStringPayload(bytes, i+1)
			inWSRun = false
			continue
		}

		// A literal newline: cleanPreserveNewlines keeps it as a real line
		// break (and never merges it into a pending space run — a run of
		// tabs immediately before a '\n' must not leave a trailing space on
		// the line it's closing out); cleanCollapseNewlines falls through to
		// the generic whitespace-run handling below, identical to before
		// this function existed.
		if mode == cleanPreserveNewlines && b == '\n' {
			inWSRun = false
			cleaned = append(cleaned, '\n')
			continue
		}
		// A bare CR, or the CR half of a CRLF pair: cleanPreserveNewlines
		// swallows it outright rather than collapsing it into a space — the
		// '\n' (handled above, whether it follows immediately or the CR was
		// alone) is the one literal break this mode ever emits.
		if mode == cleanPreserveNewlines && b == '\r' {
			continue
		}

		// Handle whitespace (both modes: tab always collapses; newline/CR
		// only reach here under cleanCollapseNewlines, having already been
		// special-cased above under cleanPreserveNewlines).
		if b == '\t' || b == '\n' || b == '\r' {
			inWSRun = true
			continue
		}

		// Try to decode a UTF-8 rune (decodeUTF8Byte rejects overlong
		// encodings, so an overlong-encoded ESC/C1 byte never reaches the
		// checks below as a live control rune).
		r, width := decodeUTF8Byte(bytes, i)
		if width == 0 {
			// Invalid start byte, truncated sequence, or overlong encoding —
			// drop just this byte; the next iteration retries byte-by-byte.
			continue
		}

		// A validly UTF-8-ENCODED C1 introducer (e.g. 0xC2 0x9B) is the
		// reachable shape for JSON-carried strings — json.Unmarshal never
		// yields a raw 0x80-0x9F byte, but it happily decodes a 2-byte C1
		// rune. Consume its structured payload the same way as the raw-byte
		// case above (same canonical consumers), rather than falling through
		// to isUnicodeControl below, which would silently drop only the
		// introducer rune and leak the payload as plain text.
		if r == 0x9B { // CSI
			i = consumeCSIPayload(bytes, i+width)
			inWSRun = false
			continue
		}
		if r == 0x9D || r == 0x90 || r == 0x9E || r == 0x9F { // OSC/DCS/PM/APC
			i = consumeStringPayload(bytes, i+width)
			inWSRun = false
			continue
		}

		// Emit space if we were in a whitespace run
		if inWSRun {
			cleaned = append(cleaned, ' ')
			inWSRun = false
		}

		// Check for bidi controls and other problematic runes
		if !isBidiControl(r) && !isUnicodeControl(r) {
			cleaned = append(cleaned, r)
		}
		i += width - 1 // -1 because the loop will increment i
	}

	if inWSRun {
		cleaned = append(cleaned, ' ')
	}
	return cleaned
}

// decodeUTF8Byte decodes a UTF-8 rune starting at bytes[i]. Returns the rune
// and the width in bytes (1-4), or 0, 0 if the byte is not a valid UTF-8
// start byte, the sequence is truncated/malformed, or the sequence is an
// overlong encoding (a multi-byte encoding of a value that fits in fewer
// bytes — e.g. 0xC0 0x9B decodes arithmetically to 0x1B/ESC, but 2-byte
// sequences must encode values >= 0x80). Overlong encodings are rejected
// rather than accepted, so an overlong-encoded ESC/C1 byte can never reach
// Sanitize's control-rune checks or the plain-text path by defeating this
// decoder.
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
		if r < 0x80 { // overlong
			return 0, 0
		}
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
		if r < 0x800 { // overlong
			return 0, 0
		}
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
		if r < 0x10000 { // overlong
			return 0, 0
		}
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
// - OSC/DCS/PM/APC (ESC ']'/P/^/_) consume until a terminator or end
// - Any other ESC is consumed along with the following byte
// - A trailing ESC with nothing after it consumes just itself
// An unterminated sequence consumes to the end of input. Delegates the actual
// payload scan to consumeCSIPayload/consumeStringPayload, the same consumers
// used by the raw-8-bit-byte and decoded-C1-rune introducer paths in
// Sanitize, so there is exactly one payload-consumption implementation.
func skipEscapeBytes(bytes []byte, i int) int {
	if i+1 >= len(bytes) {
		return i
	}

	c := bytes[i+1]

	// CSI
	if c == '[' {
		return consumeCSIPayload(bytes, i+2)
	}

	// OSC / DCS / PM / APC
	if c == ']' || c == 'P' || c == '^' || c == '_' {
		return consumeStringPayload(bytes, i+2)
	}

	// Any other ESC is consumed along with the following byte
	return i + 1
}

// consumeCSIPayload consumes CSI parameter/intermediate bytes then one final
// byte in 0x40-0x7E, starting at bytes[start] — the first byte after a CSI
// introducer, whether that introducer was ESC '[', the raw 8-bit CSI byte
// 0x9B, or a UTF-8-decoded CSI rune 0x9B. Returns the index of the final byte
// consumed, or len(bytes)-1 if the sequence runs off the end unterminated.
// This is the one canonical CSI-payload consumer for all three introducer
// shapes.
func consumeCSIPayload(bytes []byte, start int) int {
	if len(bytes) == 0 {
		return 0
	}
	j := start
	for j < len(bytes) {
		if bytes[j] >= 0x40 && bytes[j] <= 0x7E {
			return j
		}
		j++
	}
	return len(bytes) - 1
}

// consumeStringPayload consumes a string-type payload (OSC/DCS/PM/APC)
// starting at bytes[start] — the first byte after the introducer, whether
// that introducer was ESC ']'/P/^/_, a raw 8-bit byte (0x9D/0x90/0x9E/0x9F),
// or the corresponding UTF-8-decoded C1 rune. Consumes until a terminator or
// end of input; a terminator is BEL (0x07), ST as ESC '\' (0x1B 0x5C), or
// the C1 ST rune 0x9C in either encoding (raw byte 0x9C, or UTF-8-encoded as
// 0xC2 0x9C). This is the one canonical string-payload consumer for all
// three introducer shapes and all three terminator shapes.
func consumeStringPayload(bytes []byte, start int) int {
	if len(bytes) == 0 {
		return 0
	}
	j := start
	for j < len(bytes) {
		if bytes[j] == 0x07 { // BEL
			return j
		}
		if bytes[j] == 0x1B && j+1 < len(bytes) && bytes[j+1] == '\\' { // ST: ESC \
			return j + 1
		}
		if bytes[j] == 0x9C { // raw C1 ST byte
			return j
		}
		if bytes[j] == 0xC2 && j+1 < len(bytes) && bytes[j+1] == 0x9C { // UTF-8-encoded C1 ST rune
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
