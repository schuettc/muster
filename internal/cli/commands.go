package cli

import (
	"errors"
	"flag"
	"io"

	tools "github.com/schuettc/tools-common"
)

// newCommandsFlagsWithVals declares commands' flags and returns typed access
// to their values — shared by cmdCommands (real parsing) and newCommandsFlags
// (registry help/man rendering).
func newCommandsFlagsWithVals() (fs *flag.FlagSet, jsonOut *bool) {
	fs = flag.NewFlagSet("commands", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut = fs.Bool("json", false, "emit the machine-readable command index instead of the grouped listing")
	return fs, jsonOut
}

// newCommandsFlags builds commands' flag.FlagSet for registry-driven
// help/man rendering.
func newCommandsFlags() *flag.FlagSet {
	fs, _ := newCommandsFlagsWithVals()
	return fs
}

// cmdCommands prints every muster command. Bare, it's the same grouped
// listing as bare `muster`/`muster help`; --json instead emits the
// .tools-family machine-readable command index (tools.CommandsJSON) that
// kempt, tackle, and galley already expose via their own `commands --json` —
// the one missing piece for a coding agent to discover muster's surface
// without shelling out to `muster help` and scraping text.
func cmdCommands(args []string, out io.Writer) error {
	fs, jsonOut := newCommandsFlagsWithVals()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpFor("commands", out)
		}
		return err
	}
	if !*jsonOut {
		Usage(out)
		return nil
	}
	adapted := make([]tools.Command, 0, len(Registry))
	for _, c := range Registry {
		tc := tools.Command{
			Name:     c.Name,
			Synopsis: c.Synopsis,
			Summary:  c.Summary,
			Help:     c.Help,
			Group:    groupHeading[c.Group],
			NewFlags: c.NewFlags,
		}
		if c.Run != nil {
			// tools.CommandsJSON derives selfRouted from Run == nil, so a
			// dispatchable muster command (Run != nil here) must also carry a
			// non-nil tools.Command.Run, even though nothing ever calls it —
			// this adapter only feeds CommandsJSON, never Dispatch. serve/
			// mcp/debug/lambda/channel have Run == nil in the muster Registry
			// (cmd/muster's main() owns them directly) and are left nil here
			// too, so they report selfRouted: true, matching reality.
			tc.Run = func([]string, io.Writer, io.Writer) error { return nil }
		}
		adapted = append(adapted, tc)
	}
	b, err := tools.CommandsJSON("muster", adapted)
	if err != nil {
		return err
	}
	_, err = out.Write(append(b, '\n'))
	return err
}
