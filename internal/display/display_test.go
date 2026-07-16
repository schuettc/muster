package display

import (
	"strings"
	"testing"
)

// TestRuneWidthTable pins the width table's documented behavior: combining
// marks are 0, the declared East Asian Wide/Fullwidth ranges are 2, and
// ordinary printable runes (including ones just outside the wide ranges) are
// 1.
func TestRuneWidthTable(t *testing.T) {
	cases := []struct {
		name string
		r    rune
		want int
	}{
		{"ascii letter", 'a', 1},
		{"ascii digit", '7', 1},
		{"space", ' ', 1},
		{"combining acute accent (Mn)", 0x0301, 0},
		{"combining enclosing circle (Me)", 0x20DD, 0},
		{"hangul jamo start", 0x1100, 2},
		{"hangul jamo end", 0x115F, 2},
		{"cjk unified ideograph (zhong)", 0x4E2D, 2}, // 中
		{"hangul syllable (ga)", 0xAC00, 2},          // 가
		{"cjk compatibility ideograph", 0xF900, 2},
		{"fullwidth latin A", 0xFF21, 2}, // Ａ
		{"fullwidth sign", 0xFFE5, 2},    // ￥
		{"cjk ext b plane start", 0x20000, 2},
		{"just below hangul jamo range", 0x10FF, 1},
		{"just above hangul jamo range", 0x1160, 1},
	}
	for _, c := range cases {
		if got := runeWidth(c.r); got != c.want {
			t.Errorf("%s: runeWidth(%U) = %d, want %d", c.name, c.r, got, c.want)
		}
	}
}

// TestWidthSumsRunes exercises Width() end to end over a mixed string.
func TestWidthSumsRunes(t *testing.T) {
	// "a" (1) + "中" (2) + combining acute (0) + "b" (1) = 4
	s := "a中́b"
	if got := Width(s); got != 4 {
		t.Fatalf("Width(%q) = %d, want 4", s, got)
	}
}

func TestSanitizeStripsC0AndC1Controls(t *testing.T) {
	in := "a\x01\x02b\x1f" + string(rune(0x85)) + "c"
	got := Sanitize(in, 80)
	want := "abc"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
}

func TestSanitizeStripsNUL(t *testing.T) {
	in := "a\x00b"
	if got := Sanitize(in, 80); got != "ab" {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, "ab")
	}
}

func TestSanitizeStripsCSISequence(t *testing.T) {
	in := "\x1b[31mred\x1b[0m text"
	got := Sanitize(in, 80)
	want := "red text"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
}

func TestSanitizeStripsLoneEscapePlusOneChar(t *testing.T) {
	in := "a\x1bcb" // ESC 'c' is a common terminal RESET, not CSI
	got := Sanitize(in, 80)
	want := "ab"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
}

func TestSanitizeTrailingLoneEscape(t *testing.T) {
	in := "hello\x1b"
	got := Sanitize(in, 80)
	want := "hello"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
}

func TestSanitizeStripsBidiControls(t *testing.T) {
	// U+202E RLO, U+202C PDF, U+2066 LRI, U+2069 PDI — escape sequences, not
	// literal Unicode format characters, so the source file stays readable
	// and lint-clean (staticcheck ST1018).
	in := "\u202eevil\u202c\u2066iso\u2069"
	got := Sanitize(in, 80)
	want := "eviliso"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
}

func TestSanitizeCollapsesTabNewlineCRRuns(t *testing.T) {
	in := "a\t\n\r\n\tb"
	got := Sanitize(in, 80)
	want := "a b"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
}

func TestSanitizeCollapsedRunAtEndIsDropped(t *testing.T) {
	in := "trailing\t\n"
	got := Sanitize(in, 80)
	want := "trailing "
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
}

func TestSanitizeDoesNotCollapsePlainSpaces(t *testing.T) {
	in := "a  b" // two literal spaces, not tab/newline/CR
	got := Sanitize(in, 80)
	if got != in {
		t.Fatalf("Sanitize(%q) = %q, want unchanged %q", in, got, in)
	}
}

func TestSanitizeTruncatesByWidthWithEllipsis(t *testing.T) {
	in := strings.Repeat("a", 100)
	got := Sanitize(in, 10)
	if Width(got) > 10 {
		t.Fatalf("Sanitize width = %d, want <= 10 (%q)", Width(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("Sanitize(%q) = %q, want ellipsis suffix", in, got)
	}
	if got != strings.Repeat("a", 9)+"…" {
		t.Fatalf("Sanitize truncation = %q, want %q", got, strings.Repeat("a", 9)+"…")
	}
}

func TestSanitizeNoTruncationWhenItFits(t *testing.T) {
	in := "short"
	got := Sanitize(in, 80)
	if got != in {
		t.Fatalf("Sanitize(%q, 80) = %q, want unchanged", in, got)
	}
	if strings.Contains(got, "…") {
		t.Fatalf("Sanitize(%q) should not be truncated, got %q", in, got)
	}
}

func TestSanitizeWideRuneTruncation(t *testing.T) {
	// Each CJK ideograph below is width 2; five of them is width 10.
	in := strings.Repeat("中", 5)
	got := Sanitize(in, 7)
	if Width(got) > 7 {
		t.Fatalf("Sanitize width = %d, want <= 7 (%q)", Width(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis, got %q", got)
	}
}

func TestSanitizeMaxWidthZeroOrNegative(t *testing.T) {
	if got := Sanitize("hello", 0); got != "" {
		t.Fatalf("Sanitize(_, 0) = %q, want empty", got)
	}
	if got := Sanitize("hello", -5); got != "" {
		t.Fatalf("Sanitize(_, -5) = %q, want empty", got)
	}
}

func TestSanitizeEmptyString(t *testing.T) {
	if got := Sanitize("", 80); got != "" {
		t.Fatalf("Sanitize(\"\", 80) = %q, want empty", got)
	}
}

func TestSanitizeStripsOSCSequenceWithBEL(t *testing.T) {
	// OSC sequences are terminated by BEL (0x07)
	// Payload "evil-title" must NOT appear in output
	in := "before\x1b]0;evil-title\x07after"
	got := Sanitize(in, 200)
	want := "beforeafter"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
	if strings.Contains(got, "evil-title") {
		t.Fatalf("Sanitize(%q) leaked payload in output: %q", in, got)
	}
}

func TestSanitizeStripsOSCSequenceWithST(t *testing.T) {
	// OSC sequences can also be terminated by ST (ESC \)
	// Payload "evil-payload" must NOT appear in output
	in := "text\x1b]0;evil-payload\x1b\\done"
	got := Sanitize(in, 200)
	want := "textdone"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
	if strings.Contains(got, "evil-payload") {
		t.Fatalf("Sanitize(%q) leaked payload in output: %q", in, got)
	}
}

func TestSanitizeStripsUnterminatedOSCSequence(t *testing.T) {
	// An unterminated OSC consumes to end of input
	// Payload "unterminated" must NOT appear in output
	in := "before\x1b]unterminated"
	got := Sanitize(in, 200)
	want := "before"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
	if strings.Contains(got, "unterminated") {
		t.Fatalf("Sanitize(%q) leaked unterminated OSC payload in output: %q", in, got)
	}
}

func TestSanitizeStripsDCSSequence(t *testing.T) {
	// DCS sequences are ESC 'P' ... BEL/ST
	// Payload "dcs-payload" must NOT appear in output
	in := "start\x1bPdcs-payload\x07end"
	got := Sanitize(in, 200)
	if strings.Contains(got, "dcs-payload") {
		t.Fatalf("Sanitize(%q) leaked DCS payload in output: %q", in, got)
	}
}

func TestSanitizeStripsPMSequence(t *testing.T) {
	// PM sequences are ESC '^' ... BEL/ST
	// Payload "pm-payload" must NOT appear in output
	in := "start\x1b^pm-payload\x07end"
	got := Sanitize(in, 200)
	if strings.Contains(got, "pm-payload") {
		t.Fatalf("Sanitize(%q) leaked PM payload in output: %q", in, got)
	}
}

func TestSanitizeStripsAPCSequence(t *testing.T) {
	// APC sequences are ESC '_' ... BEL/ST
	// Payload "apc-payload" must NOT appear in output
	in := "start\x1b_apc-payload\x07end"
	got := Sanitize(in, 200)
	if strings.Contains(got, "apc-payload") {
		t.Fatalf("Sanitize(%q) leaked APC payload in output: %q", in, got)
	}
}

func TestSanitizeStrips8BitOSCSequence(t *testing.T) {
	// 8-bit OSC introducer is U+009D
	// Payload "8bit-osc" must NOT appear in output
	in := "before\x9d0;8bit-osc\x07after"
	got := Sanitize(in, 200)
	if strings.Contains(got, "8bit-osc") {
		t.Fatalf("Sanitize(%q) leaked 8-bit OSC payload in output: %q", in, got)
	}
}

func TestSanitizeStrips8BitCSISequence(t *testing.T) {
	// 8-bit CSI introducer is U+009B
	// The "31m" parameter must NOT appear in output
	in := "a\x9b31mred"
	got := Sanitize(in, 200)
	if strings.Contains(got, "31m") {
		t.Fatalf("Sanitize(%q) leaked 8-bit CSI parameters in output: %q", in, got)
	}
	want := "ared"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
}

func TestSanitizeStrips8BitDCSSequence(t *testing.T) {
	// 8-bit DCS introducer is U+0090
	// Payload "8bit-dcs" must NOT appear in output
	in := "before\x908bit-dcs\x07after"
	got := Sanitize(in, 200)
	if strings.Contains(got, "8bit-dcs") {
		t.Fatalf("Sanitize(%q) leaked 8-bit DCS payload in output: %q", in, got)
	}
}

func TestSanitizeStrips8BitPMSequence(t *testing.T) {
	// 8-bit PM introducer is U+009E
	// Payload "8bit-pm" must NOT appear in output
	in := "before\x9e8bit-pm\x07after"
	got := Sanitize(in, 200)
	if strings.Contains(got, "8bit-pm") {
		t.Fatalf("Sanitize(%q) leaked 8-bit PM payload in output: %q", in, got)
	}
}

func TestSanitizeStrips8BitAPCSequence(t *testing.T) {
	// 8-bit APC introducer is U+009F
	// Payload "8bit-apc" must NOT appear in output
	in := "before\x9f8bit-apc\x07after"
	got := Sanitize(in, 200)
	if strings.Contains(got, "8bit-apc") {
		t.Fatalf("Sanitize(%q) leaked 8-bit APC payload in output: %q", in, got)
	}
}
