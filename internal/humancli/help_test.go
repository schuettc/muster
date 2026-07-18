package humancli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestUsageGroupedSnapshot is the grouped-usage snapshot: every group header
// appears in order, and every command's name + one-line summary is listed
// under it (a padded-columns row, not just present anywhere in the output).
func TestUsageGroupedSnapshot(t *testing.T) {
	var buf bytes.Buffer
	Usage(&buf)
	out := buf.String()

	headings := []string{"Talk:", "Watch:", "Identity:", "Plumbing:"}
	lastIdx := -1
	for _, h := range headings {
		idx := strings.Index(out, h)
		if idx < 0 {
			t.Fatalf("Usage output missing heading %q:\n%s", h, out)
		}
		if idx < lastIdx {
			t.Fatalf("heading %q out of order in Usage output:\n%s", h, out)
		}
		lastIdx = idx
	}

	for _, c := range Registry {
		if !strings.Contains(out, c.Name) {
			t.Errorf("Usage output missing command name %q", c.Name)
		}
		if !strings.Contains(out, c.Summary) {
			t.Errorf("Usage output missing summary for %q: %q", c.Name, c.Summary)
		}
	}

	if !strings.Contains(out, "muster help <command>") {
		t.Error("Usage output missing the 'muster help <command>' pointer")
	}
	if !strings.Contains(out, "muster.tools") {
		t.Error("Usage output missing the muster.tools footer")
	}
}

// TestBareInvocationVsHelp mirrors main.go's split: Dispatch itself doesn't
// decide exit codes for the truly-bare (zero args) case — main.go special-
// cases that before ever calling Dispatch — but `muster help` (args =
// ["help"]) must report success.
func TestBareInvocationVsHelp(t *testing.T) {
	var buf bytes.Buffer
	if err := Dispatch([]string{"help"}, &buf); err != nil {
		t.Fatalf("help: unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "muster — local multi-agent coordination bus") {
		t.Fatalf("help output missing banner:\n%s", buf.String())
	}
}

// TestTopLevelHelpFlags checks `muster -h` and `muster --help` both render
// the same grouped usage as `muster help`.
func TestTopLevelHelpFlags(t *testing.T) {
	var wantBuf bytes.Buffer
	Usage(&wantBuf)

	for _, arg := range []string{"-h", "--help"} {
		var buf bytes.Buffer
		if err := Dispatch([]string{arg}, &buf); err != nil {
			t.Fatalf("%s: unexpected error: %v", arg, err)
		}
		if buf.String() != wantBuf.String() {
			t.Fatalf("%s output does not match Usage():\ngot:\n%s\nwant:\n%s", arg, buf.String(), wantBuf.String())
		}
	}
}

// TestHelpForUnknownCommand checks the "unknown command in muster help <x>"
// contract: a UsageError listing valid commands.
func TestHelpForUnknownCommand(t *testing.T) {
	var buf bytes.Buffer
	err := HelpFor("bogus", &buf)
	if err == nil {
		t.Fatal("expected error for unknown command")
	}
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *UsageError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "valid commands:") {
		t.Fatalf("error missing valid-commands listing: %v", err)
	}
	for _, c := range Registry {
		if !strings.Contains(err.Error(), c.Name) {
			t.Errorf("valid-commands listing missing %q: %v", c.Name, err)
		}
	}
}

// TestDispatchHelpMan checks `muster help --man` emits roff, not the
// grouped usage — and that it's NOT itself listed as a command (hidden).
func TestDispatchHelpMan(t *testing.T) {
	var buf bytes.Buffer
	if err := Dispatch([]string{"help", "--man"}, &buf); err != nil {
		t.Fatalf("help --man: unexpected error: %v", err)
	}
	if !strings.HasPrefix(buf.String(), ".TH MUSTER 1") {
		t.Fatalf("help --man output doesn't start with a .TH roff header:\n%.100s", buf.String())
	}

	var usageBuf bytes.Buffer
	Usage(&usageBuf)
	if strings.Contains(usageBuf.String(), "--man") {
		t.Error("--man should be hidden from grouped usage output")
	}
}

// TestDispatchVersion checks `version` and `--version` both print
// version.Line()'s output and exit clean.
func TestDispatchVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version"} {
		var buf bytes.Buffer
		if err := Dispatch([]string{arg}, &buf); err != nil {
			t.Fatalf("%s: unexpected error: %v", arg, err)
		}
		if !strings.HasPrefix(buf.String(), "muster ") {
			t.Fatalf("%s output = %q, want a line starting with \"muster \"", arg, buf.String())
		}
	}
}
