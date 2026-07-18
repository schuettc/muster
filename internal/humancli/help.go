package humancli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// UsageError marks a Dispatch error as an operator mistake (unknown command,
// missing required argument) rather than a runtime failure (a daemon call
// that failed, a bad flag value). cmd/muster's main() checks for it to
// decide between exit code 2 (usage) and 1 (everything else) — the
// conventional split, and the same code bare-invocation-with-no-args already
// used.
type UsageError struct{ msg string }

// Error implements the error interface.
func (e *UsageError) Error() string { return e.msg }

// usageErrorf builds a UsageError with a formatted message.
func usageErrorf(format string, a ...any) error {
	return &UsageError{msg: fmt.Sprintf(format, a...)}
}

// IsHelpArg reports whether s is a help flag/word muster recognizes at the
// front of a command's arguments.
func IsHelpArg(s string) bool { return s == "-h" || s == "--help" }

// helpRequested reports whether any argument is a help flag — used by the
// handful of commands (agents, inbox, tasks, deregister) that take no real
// flags and so have no flag.FlagSet of their own to catch -h/--help via
// flag.ErrHelp.
func helpRequested(args []string) bool {
	for _, a := range args {
		if IsHelpArg(a) {
			return true
		}
	}
	return false
}

// Usage writes muster's grouped command listing (bare `muster`, `muster
// help`) to out: one padded line per command, grouped under the four
// headers in groupOrder, plumbing last.
func Usage(out io.Writer) {
	_, _ = fmt.Fprintln(out, "muster — local multi-agent coordination bus")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Usage:")
	_, _ = fmt.Fprintln(out, "  muster <command> [args]")
	_, _ = fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	for _, g := range groupOrder {
		_, _ = fmt.Fprintf(tw, "%s:\n", groupHeading[g])
		for _, c := range Registry {
			if c.Group != g {
				continue
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\n", c.Name, c.Summary)
		}
		_, _ = fmt.Fprintln(tw)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(out, "Run 'muster help <command>' for details on a command.")
	_, _ = fmt.Fprintln(out, "Docs: https://muster.tools")
}

// HelpFor writes one command's full usage (synopsis, description, flags with
// defaults) to out. An unknown name is a UsageError listing valid commands.
func HelpFor(name string, out io.Writer) error {
	cmd, ok := lookup(name)
	if !ok {
		return usageErrorf("unknown command %q\n\nvalid commands: %s", name, strings.Join(commandNames(), ", "))
	}
	_, _ = fmt.Fprintf(out, "muster %s — %s\n\n", cmd.Name, cmd.Summary)
	_, _ = fmt.Fprintf(out, "Usage:\n  muster %s\n", cmd.Synopsis)
	if cmd.Help != "" {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, cmd.Help)
	}
	if cmd.NewFlags != nil {
		fs := cmd.NewFlags()
		hasFlags := false
		fs.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			_, _ = fmt.Fprintln(out)
			_, _ = fmt.Fprintln(out, "Flags:")
			fs.SetOutput(out)
			fs.PrintDefaults()
		}
	}
	return nil
}

// dispatchHelp implements `muster help`, `muster help --man`, and `muster
// help <cmd>`.
func dispatchHelp(args []string, out io.Writer) error {
	if len(args) == 0 {
		Usage(out)
		return nil
	}
	if args[0] == "--man" {
		_, err := io.WriteString(out, ManPage())
		return err
	}
	return HelpFor(args[0], out)
}
