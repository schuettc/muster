package display

import (
	"encoding/json"
	"strings"
	"testing"
)

// wireRoundTrip proves a given raw string is the reachable shape for daemon
// callers: encode it into a JSON string field and decode it back out via
// json.Unmarshal, exactly as the daemon does for every arg (see
// internal/daemon/daemon.go's str(a, "body") -> display.Sanitize path). A
// raw C1 byte (0x80-0x9F) cannot survive this round trip un-mutated —
// json.Unmarshal only ever yields valid UTF-8, replacing malformed bytes
// with U+FFFD — so a test that only exercises the raw byte is testing an
// unreachable input shape.
func wireRoundTrip(t *testing.T, s string) string {
	t.Helper()
	type payload struct {
		Body string `json:"body"`
	}
	encoded, err := json.Marshal(payload{Body: s})
	if err != nil {
		t.Fatalf("json.Marshal(%q) failed: %v", s, err)
	}
	var decoded payload
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(%q) failed: %v", encoded, err)
	}
	return decoded.Body
}

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

// --- Fix 2: rune-level C1 + overlong rejection ---------------------------
//
// The 5912665 fix special-cased C1 introducers by raw byte value, which is
// unreachable through the daemon: json.Unmarshal never yields a raw
// 0x80-0x9F byte from a JSON string (malformed bytes become U+FFFD). The
// reachable shape is a validly UTF-8-ENCODED C1 rune (string(rune(0x9B)),
// i.e. bytes 0xC2 0x9B) — which decoded to the rune, got dropped by
// isUnicodeControl as a plain control, and silently left its payload behind
// in the plain-text path. These tests pin both the direct-Sanitize
// reproduction AND a wire-shaped round trip (json.Marshal/Unmarshal) proving
// the exact bytes tested are what the daemon actually sees on this path.

func TestSanitizeConsumesUTF8EncodedOSCIntroducerPayload(t *testing.T) {
	// Reviewer repro: string(rune(0x9D)) is the UTF-8-encoded OSC introducer
	// (bytes 0xC2 0x9D), not the raw byte. Its "0;evil-title" payload must
	// be consumed, not leaked, and BEL still terminates it.
	in := "before" + string(rune(0x9D)) + "0;evil-title\x07after"
	got := Sanitize(in, 200)
	want := "beforeafter"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
	if strings.Contains(got, "evil-title") {
		t.Fatalf("Sanitize(%q) leaked UTF-8-encoded OSC payload: %q", in, got)
	}
}

func TestSanitizeConsumesUTF8EncodedOSCIntroducerPayloadOverWire(t *testing.T) {
	// Same reproduction as above, but round-tripped through
	// json.Marshal/Unmarshal first — proving these are the exact bytes a
	// daemon caller (e.g. reply body) actually receives.
	in := "before" + string(rune(0x9D)) + "0;evil-title\x07after"
	wire := wireRoundTrip(t, in)
	if wire != in {
		t.Fatalf("wire round trip changed the input: got %q, want unchanged %q", wire, in)
	}
	got := Sanitize(wire, 200)
	want := "beforeafter"
	if got != want {
		t.Fatalf("Sanitize(wireRoundTrip(%q)) = %q, want %q", in, got, want)
	}
	if strings.Contains(got, "evil-title") {
		t.Fatalf("Sanitize(wireRoundTrip(%q)) leaked OSC payload: %q", in, got)
	}
}

func TestSanitizeConsumesUTF8EncodedCSIIntroducerPayload(t *testing.T) {
	// Reviewer repro: string(rune(0x9B)) is the UTF-8-encoded CSI introducer
	// (bytes 0xC2 0x9B), not the raw byte. Its "31m" parameter bytes must be
	// consumed up through the final byte 'm', not leaked.
	in := "a" + string(rune(0x9B)) + "31mred"
	got := Sanitize(in, 200)
	want := "ared"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
	if strings.Contains(got, "31m") {
		t.Fatalf("Sanitize(%q) leaked UTF-8-encoded CSI parameters: %q", in, got)
	}
}

func TestSanitizeConsumesUTF8EncodedCSIIntroducerPayloadOverWire(t *testing.T) {
	in := "a" + string(rune(0x9B)) + "31mred"
	wire := wireRoundTrip(t, in)
	if wire != in {
		t.Fatalf("wire round trip changed the input: got %q, want unchanged %q", wire, in)
	}
	got := Sanitize(wire, 200)
	want := "ared"
	if got != want {
		t.Fatalf("Sanitize(wireRoundTrip(%q)) = %q, want %q", in, got, want)
	}
	if strings.Contains(got, "31m") {
		t.Fatalf("Sanitize(wireRoundTrip(%q)) leaked CSI parameters: %q", in, got)
	}
}

func TestSanitizeConsumesUTF8EncodedDCSPMAPCIntroducerPayload(t *testing.T) {
	// The other three C1 introducers (DCS 0x90, PM 0x9E, APC 0x9F) leak the
	// same way when UTF-8-encoded; cover all three in their reachable shape.
	cases := []struct {
		name    string
		intro   rune
		payload string
	}{
		{"DCS", 0x90, "dcs-payload"},
		{"PM", 0x9E, "pm-payload"},
		{"APC", 0x9F, "apc-payload"},
	}
	for _, c := range cases {
		in := "start" + string(c.intro) + c.payload + "\x07end"
		got := Sanitize(in, 200)
		want := "startend"
		if got != want {
			t.Fatalf("%s: Sanitize(%q) = %q, want %q", c.name, in, got, want)
		}
		if strings.Contains(got, c.payload) {
			t.Fatalf("%s: Sanitize(%q) leaked payload: %q", c.name, in, got)
		}
	}
}

func TestSanitizeUTF8EncodedStringPayloadTerminatedByC1STRune(t *testing.T) {
	// ST (string terminator) can be either ESC '\' or the C1 ST rune 0x9C,
	// and 0x9C can itself appear raw or UTF-8-encoded. Cover the
	// UTF-8-encoded ST rune terminating a UTF-8-encoded OSC introducer.
	in := "before" + string(rune(0x9D)) + "evil" + string(rune(0x9C)) + "after"
	got := Sanitize(in, 200)
	want := "beforeafter"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
	}
}

func TestSanitizeRejectsOverlongEncodedESC(t *testing.T) {
	// 0xC0 0x9B is an overlong 2-byte encoding of 0x1B (ESC): arithmetically
	// (0xC0&0x1F)<<6 | (0x9B&0x3F) == 0x1B, but a real UTF-8 encoder would
	// never emit a 2-byte sequence for a value below 0x80. Before the
	// overlong check, this decoded to a plain ESC control rune, which
	// isUnicodeControl drops WITHOUT consuming anything — so any CSI-shaped
	// text immediately following it (e.g. "[31m") reached the plain-text
	// path completely unconsumed, defeating CSI recognition entirely.
	//
	// After the fix, decodeUTF8Byte rejects the overlong pair outright: the
	// leading byte (0xC0) is dropped as invalid, and the scanner resyncs
	// byte-by-byte, landing on the trailing byte (0x9B) as its own token —
	// which is exactly the raw 8-bit CSI introducer byte, so it (correctly,
	// safely) consumes a CSI-shaped payload rather than leaking it. The
	// property that matters here is not "the trailing bytes survive
	// untouched" (they don't, they're swallowed as a payload) — it's that
	// no literal ESC rune (0x1B) ever reaches the output, and content
	// preceding the malformed bytes is untouched.
	in := "a" + string([]byte{0xC0, 0x9B}) + "b"
	got := Sanitize(in, 200)
	if strings.ContainsRune(got, 0x1B) {
		t.Fatalf("Sanitize(overlong ESC) leaked a live ESC rune: %q", got)
	}
	want := "a"
	if got != want {
		t.Fatalf("Sanitize(%q) = %q, want %q (prefix preserved, overlong bytes + swallowed CSI-shaped suffix dropped)", in, got, want)
	}
}

func TestDecodeUTF8ByteRejectsOverlongEncodings(t *testing.T) {
	cases := []struct {
		name  string
		bytes []byte
	}{
		{"2-byte overlong ESC (0xC0 0x9B -> 0x1B)", []byte{0xC0, 0x9B}},
		{"2-byte overlong NUL (0xC0 0x80 -> 0x00)", []byte{0xC0, 0x80}},
		{"3-byte overlong ASCII (0xE0 0x80 0x80 -> 0x00)", []byte{0xE0, 0x80, 0x80}},
		{"3-byte overlong just-below-0x800 boundary (0xE0 0x9F 0xBF -> 0x7FF)", []byte{0xE0, 0x9F, 0xBF}},
		{"4-byte overlong (0xF0 0x80 0x80 0x80 -> 0x00)", []byte{0xF0, 0x80, 0x80, 0x80}},
	}
	for _, c := range cases {
		r, width := decodeUTF8Byte(c.bytes, 0)
		if width != 0 || r != 0 {
			t.Errorf("%s: decodeUTF8Byte(%v) = (%U, %d), want (0, 0) [rejected]", c.name, c.bytes, r, width)
		}
	}
}

func TestDecodeUTF8ByteAcceptsMinimalNonOverlongEncodings(t *testing.T) {
	// The boundary values just above each overlong cutoff must still decode
	// normally — the overlong check must not be off-by-one.
	cases := []struct {
		name      string
		bytes     []byte
		wantRune  rune
		wantWidth int
	}{
		{"2-byte minimal (0xC2 0x80 -> 0x80)", []byte{0xC2, 0x80}, 0x80, 2},
		{"3-byte minimal (0xE0 0xA0 0x80 -> 0x800)", []byte{0xE0, 0xA0, 0x80}, 0x800, 3},
		{"4-byte minimal (0xF0 0x90 0x80 0x80 -> 0x10000)", []byte{0xF0, 0x90, 0x80, 0x80}, 0x10000, 4},
	}
	for _, c := range cases {
		r, width := decodeUTF8Byte(c.bytes, 0)
		if r != c.wantRune || width != c.wantWidth {
			t.Errorf("%s: decodeUTF8Byte(%v) = (%U, %d), want (%U, %d)", c.name, c.bytes, r, width, c.wantRune, c.wantWidth)
		}
	}
}

// TestSanitizePinsPlainTextBehavior locks down that ordinary content —
// ASCII, valid multi-byte UTF-8, wide runes, combining marks, and
// wide-rune truncation — is byte-identical to pre-fix behavior. None of
// these inputs involve C0/C1 controls or escape sequences, so the rune-
// level C1 handling and overlong rejection added in this fix must not
// change a single one of them.
func TestSanitizePinsPlainTextBehavior(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int // maxWidth
	}{
		{"plain ascii", "hello world", 80},
		{"cafe with combining acute", "café", 80},
		{"precomposed cafe", "café", 80},
		{"chinese", "中文", 80},
		{"emoji", "hello 👋 world 🌍", 80},
		{"combining marks stacked", "é̀", 80}, // e + acute + grave
		{"wide rune truncation", strings.Repeat("中", 5), 7},
		{"mixed ascii+wide+combining", "a中b́c", 80},
	}
	for _, c := range cases {
		got := Sanitize(c.in, c.want)
		// Pin against what HEAD~1 (pre-fix) would produce: for these plain
		// inputs (no C0/C1/escape content), that is simply the input
		// truncated to maxWidth display columns, which is exactly what
		// truncateToWidth(cleanedRunes, maxWidth) computes when cleaning is
		// a no-op. We assert the invariant directly instead of a fragile
		// golden string: valid UTF-8 in, no control runes stripped
		// (there are none to strip), width respected.
		if Width(got) > c.want {
			t.Fatalf("%s: Sanitize(%q, %d) width %d exceeds max", c.name, c.in, c.want, Width(got))
		}
		wantOut := truncateToWidth([]rune(c.in), c.want)
		if got != wantOut {
			t.Fatalf("%s: Sanitize(%q, %d) = %q, want %q (unchanged plain-text behavior)", c.name, c.in, c.want, got, wantOut)
		}
	}
}

// TestSanitizeLinesPreservesNewlinesAndBlankLines is the body-mode contract
// (unlike Sanitize, which collapses every '\n' to a space): ordinary
// multi-line markdown-shaped text — paragraphs, a blank line between them,
// a bullet list — must come back as one slice element per source line,
// verbatim, with blank lines kept as empty elements rather than merged away.
func TestSanitizeLinesPreservesNewlinesAndBlankLines(t *testing.T) {
	in := "first paragraph\n\n- item one\n- item two\n\nlast line"
	want := []string{"first paragraph", "", "- item one", "- item two", "", "last line"}
	got := SanitizeLines(in)
	if len(got) != len(want) {
		t.Fatalf("SanitizeLines(%q) = %q (%d lines), want %d lines %q", in, got, len(got), len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SanitizeLines(%q)[%d] = %q, want %q", in, i, got[i], want[i])
		}
	}
}

// TestSanitizeLinesStripsControlsButKeepsStructure is the hostile-input
// case: a body carrying an ESC/CSI escape sequence, a lone bidi override,
// and a bare CR (the CR half of a stray CRLF) mixed in with real newlines —
// SanitizeLines must strip the same control-rune hazards Sanitize does
// (proven via isControlRune, the same predicate FuzzSanitize in
// fuzz_test.go pins Sanitize to) while still splitting on the real line
// breaks and leaving the rest of each line's plain text untouched.
func TestSanitizeLinesStripsControlsButKeepsStructure(t *testing.T) {
	in := "line one\x1b[31m colored\x1b[0m\nline two\r\n\u202eevil\u202c line three\n\ttab\tline"
	got := SanitizeLines(in)
	for _, line := range got {
		for _, r := range line {
			if isControlRune(r) {
				t.Fatalf("SanitizeLines(%q) line %q contains control rune %U", in, line, r)
			}
		}
	}
	want := []string{"line one colored", "line two", "evil line three", " tab line"}
	if len(got) != len(want) {
		t.Fatalf("SanitizeLines(%q) = %q (%d lines), want %d lines %q", in, got, len(got), len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SanitizeLines(%q)[%d] = %q, want %q", in, i, got[i], want[i])
		}
	}
}
