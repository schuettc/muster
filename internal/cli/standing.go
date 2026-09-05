package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/schuettc/muster/internal/store"
)

// standingBoolFlags: only --json takes no value; --key/--from are value flags.
var standingBoolFlags = map[string]bool{"json": true}

func newStandingFlagsWithVals() (*flag.FlagSet, *bool, *string, *string) {
	fs := flag.NewFlagSet("standing", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "with the list form: print the orders as JSON (the audit/verify seam)")
	key := fs.String("key", "", "the order's key within the project (default 'invariants')")
	from := fs.String("from", "human", "authoring/retracting agent alias (set/retract)")
	return fs, jsonOut, key, from
}

func newStandingFlags() *flag.FlagSet {
	fs, _, _, _ := newStandingFlagsWithVals()
	return fs
}

// cmdStanding manages a project's keyed standing orders:
//
//	muster standing <project> [--json]              list live orders
//	muster standing set <project> [--key k] "body"  create-or-replace
//	muster standing retract <project> [--key k]     retract
func cmdStanding(args []string, out io.Writer) error {
	fs, jsonOut, key, from := newStandingFlagsWithVals()
	flagArgs, rest := splitFlagsAndPositional(args, standingBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpFor("standing", out)
		}
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: muster standing <project> [--json] | set <project> [--key k] \"body\" | retract <project> [--key k]")
	}

	switch rest[0] {
	case "set":
		if len(rest) < 3 {
			return fmt.Errorf("usage: muster standing set <project> [--key <k>] \"body\" [--from <alias>]")
		}
		project := rest[1]
		body := strings.Join(rest[2:], " ")
		raw, err := callData("standing_set", map[string]any{
			"from": expandAlias(*from, rosterAliasExists()), "project": project, "key": *key, "body": body,
		})
		if err != nil {
			return err
		}
		var res struct {
			ThreadID int64 `json:"thread_id"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "standing order set (thread %d)\n", res.ThreadID)
		return err
	case "retract":
		if len(rest) < 2 {
			return fmt.Errorf("usage: muster standing retract <project> [--key <k>]")
		}
		raw, err := callData("standing_retract", map[string]any{
			"from": expandAlias(*from, rosterAliasExists()), "project": rest[1], "key": *key,
		})
		if err != nil {
			return err
		}
		var res struct {
			Changed bool `json:"changed"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return err
		}
		if res.Changed {
			_, err = fmt.Fprintln(out, "standing order retracted")
		} else {
			_, err = fmt.Fprintln(out, "no live standing order to retract")
		}
		return err
	default:
		// Bare project: list.
		return standingList(rest[0], *jsonOut, out)
	}
}

func standingList(project string, jsonOut bool, out io.Writer) error {
	raw, err := callData("standing_list", map[string]any{"project": project})
	if err != nil {
		return err
	}
	var res struct {
		Orders []store.StandingOrder `json:"orders"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	if jsonOut {
		b, err := json.MarshalIndent(res.Orders, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(b))
		return err
	}
	if len(res.Orders) == 0 {
		_, err = fmt.Fprintf(out, "no standing orders for %q\n", project)
		return err
	}
	for _, o := range res.Orders {
		if _, err := fmt.Fprintf(out, "[%s] %s\n", o.Key, o.Body); err != nil {
			return err
		}
	}
	return nil
}
