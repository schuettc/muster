package humancli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/schuettc/muster/internal/device"
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
		// expandAlias always prefers the seeded form when it exists, with no
		// regard for whether the literal --from ALSO names a live alias on
		// its own — which is exactly true of a legacy unprefixed row sitting
		// beside its seeded twin. Left alone, expandAlias would silently
		// retire the twin while the confirmation (stripped for display)
		// prints the legacy name, reading as though that row were the one
		// retired. Refuse rather than guess which of the two was meant;
		// dispAlias is not used here because the whole point is telling two
		// rows apart that dispAlias would strip to the same string.
		if seeded := device.Seed(device.Name(), fromAlias); seeded != fromAlias && liveSet[fromAlias] && liveSet[seeded] {
			return fmt.Errorf("--from %q is ambiguous: this session has both %q and %q live; pass --from with the exact alias you mean", fromAlias, fromAlias, seeded)
		}
		fromAlias = expandAlias(fromAlias, func(a string) bool { return liveSet[a] })
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
		Unread int `json:"unread"`
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
