package humancli

import (
	"strings"
	"testing"
)

// TestManPageHeaders checks the generated roff has the headers man(1)
// expects (.TH, .SH NAME/SYNOPSIS/DESCRIPTION/COMMANDS/FILES/SEE ALSO).
func TestManPageHeaders(t *testing.T) {
	page := ManPage()
	for _, want := range []string{
		".TH MUSTER 1",
		".SH NAME",
		".SH SYNOPSIS",
		".SH DESCRIPTION",
		".SH COMMANDS",
		".SH FILES",
		".SH SEE ALSO",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("man page missing %q", want)
		}
	}
}

// TestManPageOneEntryPerCommand checks every Registry command gets its own
// .TP entry (with its synopsis, roff-escaped) and every group its own .SS
// heading — the man page can't silently drop a command or a group.
func TestManPageOneEntryPerCommand(t *testing.T) {
	page := ManPage()
	for _, g := range groupOrder {
		if !strings.Contains(page, ".SS "+groupHeading[g]) {
			t.Errorf("man page missing .SS heading for group %q", groupHeading[g])
		}
	}
	tpCount := strings.Count(page, ".TP\n.B muster ")
	if tpCount != len(Registry) {
		t.Errorf("man page has %d command .TP entries, want %d (len(Registry))", tpCount, len(Registry))
	}
	for _, c := range Registry {
		if !strings.Contains(page, roffEscape(c.Summary)) {
			t.Errorf("man page missing summary for %q", c.Name)
		}
	}
}

// TestManPageFilesSectionUsesPaths checks the FILES section names the
// actual db/socket leaf filenames internal/paths constructs, not a
// hardcoded second copy.
func TestManPageFilesSectionUsesPaths(t *testing.T) {
	page := ManPage()
	for _, want := range []string{"bus.db", "sock", "MUSTER_HOME"} {
		if !strings.Contains(page, want) {
			t.Errorf("man page FILES section missing %q", want)
		}
	}
}

// TestRoffEscapeNeutralizesLeadingDotAndQuotes checks the escaper the man
// renderer relies on: a leading "." would otherwise be read as a macro
// request, and a bare "quoted word" would have its quotes silently eaten by
// .B/.TP's argument parser.
func TestRoffEscapeNeutralizesLeadingDotAndQuotes(t *testing.T) {
	got := roffEscape(`.dangerous line with "a quote" and a \backslash`)
	if strings.HasPrefix(got, ".") {
		t.Errorf("roffEscape left a leading '.': %q", got)
	}
	if strings.Contains(got, `"`) {
		t.Errorf("roffEscape left a literal double quote: %q", got)
	}
	if !strings.Contains(got, `\\backslash`) {
		t.Errorf("roffEscape did not double the backslash: %q", got)
	}
}
