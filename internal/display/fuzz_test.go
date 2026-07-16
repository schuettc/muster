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
