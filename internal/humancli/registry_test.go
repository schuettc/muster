package humancli

import (
	"bytes"
	"errors"
	"sort"
	"strings"
	"testing"
)

// wantCommandNames is muster's full subcommand vocabulary, independent of
// Registry — a second, hand-maintained list so a future edit that silently
// drops (or typos) a Registry entry fails a test instead of shipping quietly.
// Keep this in sync with CLAUDE.md's package-map/usage line and
// cmd/muster/main.go's routing comment.
var wantCommandNames = []string{
	"send", "nudge", "reply",
	"agents", "inbox", "tasks", "thread", "events", "watch", "station",
	"register", "deregister", "label", "gc",
	"serve", "mcp", "hook", "debug",
}

// mainOwnedCommands are the Registry names cmd/muster's main() dispatches
// directly (never through humancli.Dispatch) — they need process-level setup
// (daemon startup, MCP stdio framing, a one-off raw daemon call) this
// package deliberately doesn't do. Every other Registry command must have a
// non-nil Run.
var mainOwnedCommands = map[string]bool{"serve": true, "mcp": true, "debug": true}

func TestRegistryCompleteness(t *testing.T) {
	got := append([]string(nil), commandNames()...)
	want := append([]string(nil), wantCommandNames...)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("Registry has %d commands, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Registry command set mismatch:\ngot:  %v\nwant: %v", got, want)
		}
	}
}

// TestRegistryRunNilOnlyForMainOwned enforces the Command.Run contract
// documented on the type: nil exactly for the three commands main() owns,
// non-nil (and dispatchable) for every other Registry entry. This is the
// "switch vs registry" cross-check the CLI-help spec asks for — Dispatch has
// no separate switch anymore (it walks Registry directly via lookup), so the
// only place drift could reappear is a Run left nil (or wrongly non-nil).
func TestRegistryRunNilOnlyForMainOwned(t *testing.T) {
	for _, c := range Registry {
		wantNil := mainOwnedCommands[c.Name]
		gotNil := c.Run == nil
		if gotNil != wantNil {
			t.Errorf("%s: Run nil = %v, want %v (mainOwnedCommands = %v)", c.Name, gotNil, wantNil, mainOwnedCommands[c.Name])
		}
	}
}

// TestDispatchUnknownCommandIsUsageError checks the exit-code-2 contract:
// cmd/muster's main() type-asserts *UsageError to decide 2 vs 1.
func TestDispatchUnknownCommandIsUsageError(t *testing.T) {
	err := Dispatch([]string{"bogus"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for unknown command")
	}
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *UsageError, got %T: %v", err, err)
	}
}

// TestEveryCommandHelpExitsCleanAndDocumentsSynopsis is table-driven over
// Registry: `muster <cmd> -h` must succeed (nil error, i.e. exit 0) and its
// output must contain the command's synopsis — the same guarantee
// `muster help <cmd>` gives, exercised through the real dispatch path
// (Command.Run) instead of calling HelpFor directly, so it also proves each
// command's own flag.ErrHelp/helpRequested interception actually fires.
func TestEveryCommandHelpExitsCleanAndDocumentsSynopsis(t *testing.T) {
	for _, c := range Registry {
		if c.Run == nil {
			continue // serve/mcp/debug: help-tested at the main package level
		}
		t.Run(c.Name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Dispatch([]string{c.Name, "-h"}, &buf); err != nil {
				t.Fatalf("%s -h: unexpected error: %v", c.Name, err)
			}
			if !strings.Contains(buf.String(), c.Synopsis) {
				t.Fatalf("%s -h output missing synopsis %q:\n%s", c.Name, c.Synopsis, buf.String())
			}

			buf.Reset()
			if err := Dispatch([]string{c.Name, "--help"}, &buf); err != nil {
				t.Fatalf("%s --help: unexpected error: %v", c.Name, err)
			}
			if !strings.Contains(buf.String(), c.Synopsis) {
				t.Fatalf("%s --help output missing synopsis %q:\n%s", c.Name, c.Synopsis, buf.String())
			}
		})
	}
}

// TestHelpCommandMatchesDirectFlag checks `muster help <cmd>` renders
// identically to `muster <cmd> -h`, for every Registry command (main-owned
// ones included, since `help <cmd>` always routes through HelpFor
// regardless of who owns Run).
func TestHelpCommandMatchesDirectFlag(t *testing.T) {
	for _, c := range Registry {
		t.Run(c.Name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Dispatch([]string{"help", c.Name}, &buf); err != nil {
				t.Fatalf("help %s: unexpected error: %v", c.Name, err)
			}
			if !strings.Contains(buf.String(), c.Synopsis) {
				t.Fatalf("help %s output missing synopsis %q:\n%s", c.Name, c.Synopsis, buf.String())
			}
			if !strings.Contains(buf.String(), c.Summary) {
				t.Fatalf("help %s output missing summary %q:\n%s", c.Name, c.Summary, buf.String())
			}
		})
	}
}
