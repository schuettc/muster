package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/schuettc/muster/internal/store"
)

func newStatusFlagsWithVals() (fs *flag.FlagSet, jsonOut *bool, alias *string) {
	fs = flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut = fs.Bool("json", false, "emit a JSON array of {alias, unread, action_required}")
	alias = fs.String("alias", "", "restrict the output to a single alias")
	return fs, jsonOut, alias
}

func newStatusFlags() *flag.FlagSet {
	fs, _, _ := newStatusFlagsWithVals()
	return fs
}

// cmdStatus prints every alias's side-effect-free inbox counts (unread +
// action_required) — a pure read that moves no watermark, so a picker can poll
// it on a cadence. --json emits the array a tool parses; the default is a
// human table.
func cmdStatus(args []string, out io.Writer) error {
	fs, jsonOut, alias := newStatusFlagsWithVals()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpFor("status", out)
		}
		return err
	}
	raw, err := callData("status", nil)
	if err != nil {
		return err
	}
	var res struct {
		Agents []store.AliasStatus `json:"agents"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	rows := res.Agents
	if *alias != "" {
		filtered := rows[:0:0]
		for _, r := range rows {
			if r.Alias == *alias {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}
	if *jsonOut {
		// Top-level array is the wire contract proj reads (thread 354).
		b, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(b))
		return err
	}
	if len(rows) == 0 {
		_, err = fmt.Fprintln(out, "no agents")
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(out, "%-6d %-6d %s\n", r.Unread, r.ActionRequired, r.Alias); err != nil {
			return err
		}
	}
	return nil
}
