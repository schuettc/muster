package display

import (
	"testing"
	"unicode"
)

// isControlRune reports whether r is something Sanitize's output must never
// contain: any Unicode control category rune (covers C0, C1, and DEL) or a
// bidi override/isolate control.
func isControlRune(r rune) bool {
	return unicode.IsControl(r) || isBidiControl(r)
}

// FuzzSanitize is the property test for Sanitize (spec §4): the output never
// contains a control rune, and its display width never exceeds maxWidth. The
// seed corpus below covers CSI escape sequences, a lone (non-CSI) escape, a
// bidi override/isolate pair, NUL and other C0/C1 controls, East Asian Wide
// runes, and a combining mark — the exact hazard classes Sanitize exists to
// neutralize.
func FuzzSanitize(f *testing.F) {
	seeds := []struct {
		s string
		w int
	}{
		{"hello world", 80},
		{"\x1b[31mred\x1b[0m text", 10},
		{"a\x1bcb", 80},                 // lone (non-CSI) escape
		{"hello\x1b", 80},               // trailing lone escape
		{"a\t\n\r\n\tb", 80},            // ws-run collapse
		{"\x00\x01\x02\x1f", 5},         // C0 controls including NUL
		{string(rune(0x85)) + "c1", 80}, // C1 control
		{"\u202eevil\u202c", 20},        // RLO/PDF bidi override
		{"\u2066iso\u2069", 20},         // LRI/PDI bidi isolate
		{"中文测试字符串超长文本", 4},              // wide runes, narrow budget
		{"é⃝", 80},                     // base + two combining marks
		{"", 0},
		{"", 80},
		{"plain ascii text that is quite long indeed for truncation", 10},
		{"\x1b[38;5;196mmulti-param CSI\x1b[0m", 15},
		// OSC/DCS/PM/APC sequences (ESC-prefixed)
		{"before\x1b]0;evil-title\x07after", 80},  // OSC with BEL
		{"text\x1b]0;evil-payload\x1b\\done", 80}, // OSC with ST
		{"before\x1b]unterminated", 80},           // unterminated OSC
		{"start\x1bPdcs-payload\x07end", 80},      // DCS with BEL
		{"start\x1b^pm-payload\x07end", 80},       // PM with BEL
		{"start\x1b_apc-payload\x07end", 80},      // APC with BEL
		// 8-bit C1 introducers
		{"a\x9b31mred", 80},                   // 8-bit CSI
		{"before\x9d0;8bit-osc\x07after", 80}, // 8-bit OSC
		{"before\x908bit-dcs\x07after", 80},   // 8-bit DCS
		{"before\x9e8bit-pm\x07after", 80},    // 8-bit PM
		{"before\x9f8bit-apc\x07after", 80},   // 8-bit APC

		// UTF-8-ENCODED C1 introducers — the reachable shape through the
		// daemon (json.Unmarshal decodes these to a rune; it never yields a
		// raw 0x80-0x9F byte). These are the reviewer's reproductions: the
		// pre-fix code decoded the introducer rune and dropped it as a
		// plain control WITHOUT consuming its payload, leaking the payload
		// as plain text.
		{"before" + string(rune(0x9d)) + "0;evil-title\x07after", 200},               // UTF-8 OSC
		{"a" + string(rune(0x9b)) + "31mred", 200},                                   // UTF-8 CSI
		{"start" + string(rune(0x90)) + "dcs-payload\x07end", 200},                   // UTF-8 DCS
		{"start" + string(rune(0x9e)) + "pm-payload\x07end", 200},                    // UTF-8 PM
		{"start" + string(rune(0x9f)) + "apc-payload\x07end", 200},                   // UTF-8 APC
		{"before" + string(rune(0x9d)) + "evil" + string(rune(0x9c)) + "after", 200}, // UTF-8 OSC terminated by UTF-8 C1 ST rune

		// Overlong UTF-8 encodings — must be rejected as invalid, not
		// decoded to the value they arithmetically produce.
		{"a" + string([]byte{0xC0, 0x9B}) + "b", 200},             // overlong 2-byte encoding of ESC (0x1B)
		{"a" + string([]byte{0xC0, 0x80}) + "b", 200},             // overlong 2-byte encoding of NUL
		{"a" + string([]byte{0xE0, 0x80, 0x80}) + "b", 200},       // overlong 3-byte encoding of NUL
		{"a" + string([]byte{0xF0, 0x80, 0x80, 0x80}) + "b", 200}, // overlong 4-byte encoding of NUL
	}
	for _, sd := range seeds {
		f.Add(sd.s, sd.w)
	}
	f.Fuzz(func(t *testing.T, s string, w int) {
		if w < 0 || w > 2000 {
			return // out of the documented/interesting range; avoid huge allocations
		}
		out := Sanitize(s, w)
		for _, r := range out {
			if isControlRune(r) {
				t.Fatalf("Sanitize(%q, %d) output contains control rune %U: %q", s, w, r, out)
			}
		}
		if got := Width(out); got > w {
			t.Fatalf("Sanitize(%q, %d) output width %d exceeds max %d: %q", s, w, got, w, out)
		}
	})
}
