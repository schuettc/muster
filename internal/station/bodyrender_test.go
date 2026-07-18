package station

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestStyleMarkdownLineBold covers the "**bold**" span: the asterisks are
// dropped from the visible text and the content is styled bold.
func TestStyleMarkdownLineBold(t *testing.T) {
	styled := styleMarkdownLine("this is **bold** text")
	if strings.Contains(styled, "**") {
		t.Fatalf("styled output still contains literal **: %q", styled)
	}
	if !strings.Contains(styled, "bold") {
		t.Fatalf("styled output lost the bold content: %q", styled)
	}
}

// TestStyleMarkdownLineCode covers the "`code`" span: the backticks are
// dropped and the content is styled distinctly from surrounding text.
func TestStyleMarkdownLineCode(t *testing.T) {
	styled := styleMarkdownLine("run `go test` now")
	if strings.Contains(styled, "`") {
		t.Fatalf("styled output still contains a literal backtick: %q", styled)
	}
	if !strings.Contains(styled, "go test") {
		t.Fatalf("styled output lost the code content: %q", styled)
	}
}

// TestStyleMarkdownLineBulletsPassThrough proves list markers ("- ", "* ")
// are left completely alone by the markdown scanner — they are handled at
// the wrap layer (bulletPrefix), not here, and a single leading "*" must
// never be confused with the "**" bold marker.
func TestStyleMarkdownLineBulletsPassThrough(t *testing.T) {
	for _, in := range []string{"- a plain bullet", "* a plain bullet"} {
		if got := styleMarkdownLine(in); got != in {
			t.Fatalf("styleMarkdownLine(%q) = %q, want unchanged", in, got)
		}
	}
}

// TestStyleMarkdownLinePassthroughEdgeCases is the safety net: markup this
// styler can't confidently pair must pass through as literal text rather
// than corrupting the line — an unterminated bold/code marker, an empty
// span ("****" or a doubled backtick), and a lone asterisk that isn't part
// of a "**" pair.
func TestStyleMarkdownLinePassthroughEdgeCases(t *testing.T) {
	cases := []string{
		"unterminated **bold with no close",
		"unterminated `code with no close",
		"empty bold marker ****  here",
		"empty code marker `` here",
		"a lone * asterisk mid sentence",
		"trailing star *",
	}
	for _, in := range cases {
		if got := styleMarkdownLine(in); got != in {
			t.Fatalf("styleMarkdownLine(%q) = %q, want passthrough unchanged", in, got)
		}
	}
}

// TestStyleMarkdownLineNestedPunctuation exercises markup sitting right next
// to ordinary punctuation — the exact "raw ** and ` punctuation litters the
// flattened text" failure mode the operator's screenshot showed — proving
// the styled result carries the words but never the markdown syntax bytes.
func TestStyleMarkdownLineNestedPunctuation(t *testing.T) {
	in := "see `internal/store.go`: it's **critical** (really)."
	styled := styleMarkdownLine(in)
	if strings.ContainsAny(styled, "`") {
		t.Fatalf("styled output still contains a backtick: %q", styled)
	}
	if strings.Contains(styled, "**") {
		t.Fatalf("styled output still contains literal **: %q", styled)
	}
	for _, want := range []string{"internal/store.go", "critical", "really"} {
		if !strings.Contains(styled, want) {
			t.Fatalf("styled output lost %q: %q", want, styled)
		}
	}
}

// TestBulletPrefixRecognizesTheThreeMarkers pins bulletPrefix's vocabulary:
// "- ", "* ", and an ordered "N. " (including multi-digit N), with anything
// else reporting no prefix.
func TestBulletPrefixRecognizesTheThreeMarkers(t *testing.T) {
	cases := []struct{ in, want string }{
		{"- dash bullet", "- "},
		{"* star bullet", "* "},
		{"1. first", "1. "},
		{"12. twelfth", "12. "},
		{"not a bullet", ""},
		{"*no space after star", ""},
		{"-no space after dash", ""},
		{"1.no space after dot", ""},
	}
	for _, c := range cases {
		if got := bulletPrefix(c.in); got != c.want {
			t.Fatalf("bulletPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWrapBodyLineHangIndentsBulletContinuations proves a wrapped bullet's
// continuation lines are indented under the marker (spec §2: "lines
// starting with '- '/'* '/'N. ' keep their bullet with a hanging indent")
// rather than reading as a second bare top-level line, and that every
// returned row — first and continuation alike — comes out at exactly the
// requested width (lipgloss.Width, ANSI-aware, since a row may carry
// markdown styling).
func TestWrapBodyLineHangIndentsBulletContinuations(t *testing.T) {
	const width = 12
	rows := wrapBodyLine("- one two three four five six seven eight", width)
	if len(rows) < 2 {
		t.Fatalf("expected the bullet to wrap onto more than one row at width %d, got %q", width, rows)
	}
	if !strings.HasPrefix(rows[0], "- ") {
		t.Fatalf("first row must keep the bullet marker, got %q", rows[0])
	}
	for i, r := range rows {
		if i > 0 && (!strings.HasPrefix(r, "  ") || strings.HasPrefix(strings.TrimPrefix(r, "  "), " ")) {
			t.Fatalf("continuation row %d must hang-indent by exactly the marker's width (2), got %q", i, r)
		}
		if w := lipgloss.Width(r); w != width {
			t.Fatalf("row %d is %d columns wide, want exactly %d: %q", i, w, width, r)
		}
	}
}

// TestWrapBodyLineStyledSpanSurvivesAWrapBoundary is the regression this
// task's design had to solve directly: a multi-word "`code span`" (or
// "**bold text**") that a naive wrap-then-style approach would split across
// two rows — leaving the markers stranded, un-paired, and printed literally
// on BOTH rows — must instead come out fully styled (no literal backticks
// or asterisks anywhere) because styling runs before lipgloss's own
// ANSI-aware wrap.
func TestWrapBodyLineStyledSpanSurvivesAWrapBoundary(t *testing.T) {
	rows := wrapBodyLine("- plain item two with `a code span` inside it", 20)
	joined := strings.Join(rows, "\n")
	if strings.Contains(joined, "`") {
		t.Fatalf("a backtick survived wrapping (span got split and left un-styled):\n%s", joined)
	}
	if !strings.Contains(joined, "a code span") {
		t.Fatalf("wrapped output lost the code span's text:\n%s", joined)
	}
	for i, r := range rows {
		if w := lipgloss.Width(r); w != 20 {
			t.Fatalf("row %d is %d columns wide, want exactly 20: %q", i, w, r)
		}
	}
}

// TestWrapBodyPreservesBlankLinesAndBullets is the structural-survival
// assertion the spec calls out directly: a markdown body with paragraphs, a
// blank line, and a bullet list must come back as separate lines with the
// bullets intact as their own rows, not flattened into one run-on line.
func TestWrapBodyPreservesBlankLinesAndBullets(t *testing.T) {
	body := "Please review:\n\n- fix the auth bug\n- update the docs\n\nThanks!"
	rows := wrapBody(body, 40)
	want := []string{"Please review:", "", "- fix the auth bug", "- update the docs", "", "Thanks!"}
	if len(rows) != len(want) {
		t.Fatalf("wrapBody produced %d rows %q, want %d rows %q", len(rows), rows, len(want), want)
	}
	for i := range want {
		// Non-blank rows come back padded to width by lipgloss; compare
		// after trimming trailing padding so the assertion pins content,
		// not incidental padding width.
		got := strings.TrimRight(rows[i], " ")
		if got != want[i] {
			t.Fatalf("row %d = %q, want %q (full: %q)", i, got, want[i], rows)
		}
	}
}

// TestWrapBodyHardCapsAbsurdBodies proves a runaway body (thousands of
// lines) is bounded rather than growing the pane — and this function's own
// working set — without limit.
func TestWrapBodyHardCapsAbsurdBodies(t *testing.T) {
	var b strings.Builder
	for i := 0; i < bodyHardCapLines*3; i++ {
		b.WriteString("line\n")
	}
	rows := wrapBody(b.String(), 40)
	if len(rows) > bodyHardCapLines+1 {
		t.Fatalf("wrapBody produced %d rows, want capped at %d (+1 truncation marker)", len(rows), bodyHardCapLines+1)
	}
	if rows[len(rows)-1] != "… (truncated)" {
		t.Fatalf("last row = %q, want a truncation marker", rows[len(rows)-1])
	}
}
