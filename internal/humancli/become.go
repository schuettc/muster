package humancli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// newBecomeFlagsWithVals declares become's flags and returns typed access to
// their values — shared by cmdBecome (real parsing) and newBecomeFlags
// (registry help/man rendering).
func newBecomeFlagsWithVals() (fs *flag.FlagSet, from *string) {
	fs = flag.NewFlagSet("become", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from = fs.String("from", "", "the alias to claim from, required when this session has more than one live alias")
	return fs, from
}

// newBecomeFlags builds become's flag.FlagSet for registry-driven help/man
// rendering.
func newBecomeFlags() *flag.FlagSet {
	fs, _ := newBecomeFlagsWithVals()
	return fs
}

// cmdBecome claims a durable name for this tmux session: a new alias
// inherits the calling session's identity and inbox read watermark, and the
// seed alias it claimed from retires (the daemon's "become" op, store.Become).
// The seed is resolved automatically when this session has exactly one live
// alias; a split identity (more than one) requires --from so the claim never
// guesses which one is being renamed.
func cmdBecome(args []string, out io.Writer) error {
	fs, from := newBecomeFlagsWithVals()
	flagArgs, rest := splitFlagsAndPositional(args, map[string]bool{})
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpFor("become", out)
		}
		return err
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: muster become <name> [--from <alias>]")
	}
	to := rest[0]
	if strings.TrimSpace(to) == "" {
		// Mirrors cmdRegister's empty-alias rejection (finding F4): reject
		// before dialing the daemon rather than letting an empty/blank name
		// round-trip into a claim nobody could address afterward.
		return fmt.Errorf("cannot become an empty alias")
	}

	c := hookCapture()

	live, err := becomeLiveAliases(c.SocketPath, c.SessionID, c.SessionCreated)
	if err != nil {
		return err
	}

	fromAlias := *from
	if fromAlias == "" {
		switch len(live) {
		case 0:
			return fmt.Errorf("nothing to become from on this session; register first")
		case 1:
			fromAlias = live[0]
		default:
			return fmt.Errorf("this session has aliases %s; pass --from <alias>", strings.Join(live, ", "))
		}
	}

	raw, err := callData("become", map[string]any{"from": fromAlias, "to": to})
	if err != nil {
		return err
	}
	var res struct {
		Unread int `json:"unread"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "you are now '%s' (was '%s') — %d unread thread(s)\n", to, fromAlias, res.Unread)
	return err
}

// becomeLiveAliases lists the session's live (non-departed) aliases. The
// session_aliases op includes departed aliases on purpose (history), so each
// candidate is confirmed live via hookGetAgent here. sessionCreated is the
// capture's incarnation, forwarded so the op scopes the lineage walk to THIS
// tmux-session incarnation (spec §5.1) — a legacy row stranded on a recycled
// session ID is not a name this session may become from.
func becomeLiveAliases(socketPath, sessionID string, sessionCreated int64) ([]string, error) {
	raw, err := callData("session_aliases", map[string]any{"socket_path": socketPath, "session_id": sessionID, "session_created": sessionCreated})
	if err != nil {
		return nil, err
	}
	var res struct {
		Aliases []string `json:"aliases"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	live := make([]string, 0, len(res.Aliases))
	for _, a := range res.Aliases {
		if ag, ok := hookGetAgent(a); ok && !ag.Departed {
			live = append(live, a)
		}
	}
	return live, nil
}
