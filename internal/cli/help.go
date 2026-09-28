package cli

import (
	"fmt"
	"io"

	tools "github.com/schuettc/tools-common"
)

// usageErrorf is a usage error (exit 2) in the family format.
func usageErrorf(format string, a ...any) error {
	return tools.UsageError{Msg: fmt.Sprintf(format, a...)}
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

// HelpFor writes one command's help to out in the family format
// (tools.HelpFor). An unknown name is a usage error.
func HelpFor(name string, out io.Writer) error {
	cmd, ok := lookup(name)
	if !ok {
		return usageErrorf("unknown command %q (see 'muster help')", name)
	}
	tools.HelpFor(out, "muster", cmd)
	return nil
}
