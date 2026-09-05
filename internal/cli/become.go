package cli

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
func newBecomeFlagsWithVals() (fs *flag.FlagSet, from *string, noInject *bool) {
	fs = flag.NewFlagSet("become", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from = fs.String("from", "", "the alias to claim from, required when this session has more than one live alias")
	noInject = fs.Bool("no-inject", false, "skip typing /rename into the live agent pane (for callers whose name ALREADY came from the harness side, e.g. the statusline promoting a /rename)")
	return fs, from, noInject
}

// newBecomeFlags builds become's flag.FlagSet for registry-driven help/man
// rendering.
func newBecomeFlags() *flag.FlagSet {
	fs, _, _ := newBecomeFlagsWithVals()
	return fs
}

// cmdBecome claims a durable name for this tmux session: a new alias
// inherits the calling session's identity and inbox read watermark, and the
// seed alias it claimed from retires (the daemon's "become" op, store.Become).
// The seed is resolved automatically when this session has exactly one live
// alias; a split identity (more than one) requires --from so the claim never
// guesses which one is being renamed.
func cmdBecome(args []string, out io.Writer) error {
	fs, from, noInject := newBecomeFlagsWithVals()
	flagArgs, rest := splitFlagsAndPositional(args, map[string]bool{"no-inject": true})
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
	} else {
		// A typed --from may be short; expand it local-first against this
		// session's own live aliases, exactly the same operation resolveVia
		// applies to every other input site.
		liveSet := make(map[string]bool, len(live))
		for _, a := range live {
			liveSet[a] = true
		}
		// Exact match against THIS session's OWN live aliases wins before
		// expandAlias's local-first bias even runs. expandAlias's local-first
		// comment explains why local-first is right everywhere else: a bare
		// name is ambiguous against the WIDER roster, and guessing wrong
		// reaches a stranger's row on another machine — action at a distance
		// the design exists to prevent. That risk needs a candidate this
		// session does not own. Here every candidate comes from
		// becomeLiveAliases, which returns only aliases live on THIS session
		// — there is no stranger's row in the set to protect against, so an
		// exact hit is simply the more precise read of what was typed. This
		// is also what makes a legacy unprefixed row nameable again: a
		// session holding both "dotfiles" and its seeded twin
		// "personal-dotfiles" has expandAlias always preferring the twin, so
		// without this exact-first check "dotfiles" could never be named —
		// the twin's own literal spelling was never in danger, only the
		// legacy row's was. Naming a row exactly now claims exactly that row.
		if !liveSet[fromAlias] {
			fromAlias = expandAlias(fromAlias, func(a string) bool { return liveSet[a] })
		}
	}

	// The CLAIM is seeded; the injected harness name and the confirmation are
	// not. syncAgentName sets the tmux/harness session name, which is a human
	// surface and the identity `proj` reads — it must stay short, or the
	// prefix reappears in exactly the title bar this design clears.
	//
	// Both derive from `claim`, not from the raw `to`: a full alias read off
	// ANOTHER machine and pasted back here is a supported input (it is the
	// round trip Seed's idempotence guard exists for), and it already carries
	// the prefix, so `to` is not reliably short. dispAlias(claim) is "the
	// stored alias as a human sees it", which is exactly what both surfaces
	// want and is identical for a short and a full input.
	claim := seedAlias(to)
	shown := dispAlias(claim)
	raw, err := callData("become", map[string]any{"from": fromAlias, "to": claim})
	if err != nil {
		return err
	}
	var res struct {
		Unread    int  `json:"unread"`
		Reclaimed bool `json:"reclaimed"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	// The claim landed, and the cloned row keeps the seed's pane and model —
	// so the harness session name follows in the SAME gesture (syncAgentName,
	// shared with label): prefix-T delegates its whole rename here, and the
	// injected name is the full claimed alias (e.g. <project>/<work>).
	// --no-inject is for names that already came from the harness side (the
	// statusline promoting a /rename the agent itself typed) — re-injecting
	// would loop the same text back into the pane.
	if !*noInject {
		syncAgentName(out, shown, c.SocketPath, c.SessionID)
	}
	_, err = fmt.Fprintf(out, "you are now '%s' (was '%s') — %d unread thread(s)\n", shown, dispAlias(fromAlias), res.Unread)
	if err != nil {
		return err
	}
	if res.Reclaimed {
		// A departed name may be reclaimed; a live one is refused
		// (ErrBecomeToExists, above). Say so explicitly — otherwise this
		// reads as an ordinary rename, and the operator has no way to tell
		// they just inherited a name (and whatever inbox/history rides along
		// with it) that used to belong to someone else.
		_, err = fmt.Fprintf(out, "note: '%s' reclaimed departed name — its prior history is now yours\n", shown)
	}
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
		// A row this lookup couldn't read is left OUT of the live list: the
		// caller offers these as names the operator may become, and a name
		// nobody could confirm is live is not one to offer.
		if ag, ok, err := hookGetAgent(a); err == nil && ok && !ag.Departed {
			live = append(live, a)
		}
	}
	return live, nil
}
