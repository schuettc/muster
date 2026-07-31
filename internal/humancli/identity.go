package humancli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/schuettc/muster/internal/clock"
	"github.com/schuettc/muster/internal/harnessenv"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// newRegisterFlagsWithVals declares register's flags and returns typed
// access to their values — shared by cmdRegister (real parsing) and
// newRegisterFlags (registry help/man rendering).
func newRegisterFlagsWithVals() (fs *flag.FlagSet, role, model, harness *string) {
	fs = flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	role = fs.String("role", "", "this agent's role")
	model = fs.String("model", "claude", "model backing this agent: claude, codex, or cursor")
	harness = fs.String("harness-session", "", "harness session UUID this registration belongs to — the pane-side launch handshake passes the UUID it then hands to `claude --session-id`, so the session's own hooks (which see no tmux) can find this row")
	return fs, role, model, harness
}

// newRegisterFlags builds register's flag.FlagSet for registry-driven
// help/man rendering.
func newRegisterFlags() *flag.FlagSet {
	fs, _, _, _ := newRegisterFlagsWithVals()
	return fs
}

// cmdRegister registers the current session as an agent. Alias precedence:
// explicit positional arg → $MUSTER_ALIAS → tmux session name → the working
// directory's basename (paneless fallback). A session with no tmux pane in
// its process environment (harness daemon-hosted sessions — see harnessenv)
// registers PANELESS: socket_path/pane_id empty, session_id carrying the
// harness session UUID, so hook ownership and sibling grouping still have an
// identity tuple, ("", uuid), to key on.
func cmdRegister(args []string, out io.Writer) error {
	fs, role, model, harness := newRegisterFlagsWithVals()
	// register has no boolean flags: --role and --model both take values, so
	// pass an empty bool-flags set (an implicit default would wrongly reuse
	// send's --role, which IS boolean there).
	flagArgs, rest := splitFlagsAndPositional(args, map[string]bool{})
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpFor("register", out)
		}
		return err
	}
	c := tmuxenv.CaptureEnv()
	h := harnessenv.FromEnv()
	paneless := c.SocketPath == "" || c.PaneID == ""
	alias := ""
	switch {
	case len(rest) > 0:
		alias = rest[0]
	case os.Getenv("MUSTER_ALIAS") != "":
		alias = os.Getenv("MUSTER_ALIAS")
	case c.SessionName != "":
		alias = c.SessionName
	default:
		// Paneless cwd fallback: every session in a directory derives the
		// same base, so the alias must be ALLOCATED unique (base, base-2, …)
		// — never taken over from another live session. An explicit arg,
		// $MUSTER_ALIAS, or a tmux session name (unique by construction)
		// registers exactly, above.
		if h.Alias() == "" && h.SessionID == "" {
			return fmt.Errorf("cannot determine alias: no tmux session and no usable working directory; pass one explicitly or set $MUSTER_ALIAS")
		}
		regFn := func(a string, ifAbsent bool) error {
			args := registerPanelessArgs(a, *role, *model, h, ifAbsent)
			_, err := callData("register_agent", args)
			return err
		}
		if owned := harnessOwnedRows(h.SessionID); len(owned) > 0 {
			// This session already has an identity (a handshake tmux row or a
			// prior paneless one): refresh/revive it with its stored shape
			// intact rather than allocating another.
			alias = owned[0].Alias
			reviveRow(owned[0], *model)
			_, err := fmt.Fprintf(out, "registered %s (existing identity, project %q, model %s)\n", alias, owned[0].Project, *model)
			return err
		}
		var err error
		if alias, err = allocPanelessAlias(h.Alias(), h.SessionID, regFn); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "registered %s (paneless, project %q, model %s)\n", alias, h.Project(), *model)
		return err
	}
	if alias == "" {
		return fmt.Errorf("cannot determine alias: no tmux session and no usable working directory; pass one explicitly or set $MUSTER_ALIAS")
	}
	socketPath, paneID := c.SocketPath, c.PaneID
	sessionID, sessionName, created := c.SessionID, c.SessionName, c.SessionCreated
	project := c.Project
	if paneless {
		// A half-captured tmux identity (socket without a pane — run-shell
		// contexts) must not be stored: a tuple mixing a real socket with a
		// non-tmux session ID would read as a dead session to every liveness
		// check. Store the clean paneless shape instead.
		socketPath, paneID, sessionName, created = "", "", "", 0
		sessionID = h.SessionID
		project = h.Project()
	}
	// The harness link: --harness-session (the pane-side launch handshake,
	// which mints the UUID before the session exists), else the ambient
	// harness UUID when this process runs inside a session.
	harnessID := *harness
	if harnessID == "" {
		harnessID = h.SessionID
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": alias, "role": *role, "model_type": *model,
		"session_name": sessionName, "session_id": sessionID,
		"session_created":    created,
		"harness_session_id": harnessID,
		"socket_path":        socketPath, "pane_id": paneID,
		"project": project, "label": c.Label, "label_manual": c.LabelManual,
	}); err != nil {
		return err
	}
	shape := ""
	if paneless {
		shape = "paneless, "
	}
	_, err := fmt.Fprintf(out, "registered %s (%sproject %q, model %s)\n", alias, shape, project, *model)
	return err
}

// cmdDeregister removes an agent's registration. Alias precedence mirrors
// register: explicit arg → $MUSTER_ALIAS → tmux session name → working
// directory basename (paneless fallback).
func cmdDeregister(args []string, out io.Writer) error {
	if helpRequested(args) {
		return HelpFor("deregister", out)
	}
	alias := ""
	switch {
	case len(args) > 0:
		alias = args[0]
	case os.Getenv("MUSTER_ALIAS") != "":
		alias = os.Getenv("MUSTER_ALIAS")
	default:
		alias = tmuxenv.CaptureEnv().SessionName
		if alias == "" {
			// No tmux: this session's alias may be handshake- or
			// suffix-allocated, so resolve through the roster by harness
			// session UUID first; the raw cwd basename is only a last resort.
			h := harnessenv.FromEnv()
			if owned := harnessOwnedRows(h.SessionID); len(owned) > 0 {
				alias = owned[0].Alias
			} else {
				alias = h.Alias()
			}
		}
	}
	if alias == "" {
		return fmt.Errorf("cannot determine alias to deregister")
	}
	if _, err := callData("deregister_agent", map[string]any{"alias": alias}); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "deregistered %s\n", alias)
	return err
}

// newGCFlagsWithVals declares gc's flags and returns typed access to their
// values — shared by cmdGC (real parsing) and newGCFlags (registry help/man
// rendering).
func newGCFlagsWithVals() (fs *flag.FlagSet, eventsKeep *time.Duration, purgeAgents *bool) {
	fs = flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	eventsKeep = fs.Duration("events-keep", 720*time.Hour, "prune journal events older than this")
	purgeAgents = fs.Bool("purge-agents", false, "hard-delete departed and tmux-dead agent rows instead of tombstoning them (irreversible: identity, project, label, and read-state are all gone; paneless rows — no tmux identity — are only reaped once departed)")
	return fs, eventsKeep, purgeAgents
}

// newGCFlags builds gc's flag.FlagSet for registry-driven help/man
// rendering.
func newGCFlags() *flag.FlagSet {
	fs, _, _ := newGCFlagsWithVals()
	return fs
}

// cmdGC tombstones every agent whose tmux session is no longer alive (spec:
// deregistration is a soft delete now — departed=1, row and read-state kept
// as history), then prunes journal events older than --events-keep (default
// 720h = 30 days). --purge-agents instead hard-deletes every departed OR
// currently-dead agent row (the pre-tombstone behavior, now explicit and
// irreversible). Paneless rows (empty socket_path — harness daemon-hosted
// sessions, see harnessenv) are exempt from BOTH liveness judgments: they
// have no tmux session to be dead in, so gc would otherwise reap every live
// one; their lifecycle belongs to the SessionEnd hook and explicit
// deregister. The agent phase and the event-prune phase are independent: a
// prune error is reported on the same writer but never masks the agent-phase
// summary already printed.
func cmdGC(args []string, out io.Writer) error {
	fs, eventsKeep, purgeAgents := newGCFlagsWithVals()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpFor("gc", out)
		}
		return err
	}
	if *eventsKeep <= 0 {
		return fmt.Errorf("--events-keep must be > 0")
	}

	raw, err := callData("list_agents", nil)
	if err != nil {
		return err
	}
	var agents []agentRow
	if err := json.Unmarshal(raw, &agents); err != nil {
		return err
	}

	if *purgeAgents {
		purged := 0
		for _, a := range agents {
			// A paneless row (no socket) has no tmux session to be dead in, so
			// liveness cannot condemn it — only its own departure (the
			// SessionEnd hook, or an explicit deregister) makes it purgeable.
			if !a.Departed && (a.SocketPath == "" || tmuxenv.IsSessionAlive(a.SocketPath, a.SessionID, a.SessionCreated)) {
				continue // still live (or liveness-unknowable) and never departed: nothing to purge
			}
			if _, err := callData("purge_agent", map[string]any{"alias": a.Alias}); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "purged %s\n", a.Alias); err != nil {
				return err
			}
			purged++
		}
		if _, err := fmt.Fprintf(out, "gc: purged %d agent(s)\n", purged); err != nil {
			return err
		}
	} else {
		tombstoned := 0
		for _, a := range agents {
			if a.SocketPath == "" {
				continue // paneless: no tmux session to judge dead — lifecycle belongs to the SessionEnd hook
			}
			if a.Departed || tmuxenv.IsSessionAlive(a.SocketPath, a.SessionID, a.SessionCreated) {
				continue // already history, or still alive: nothing to do
			}
			if _, err := callData("deregister_agent", map[string]any{"alias": a.Alias}); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "tombstoned %s (dead session)\n", a.Alias); err != nil {
				return err
			}
			tombstoned++
		}
		if _, err := fmt.Fprintf(out, "gc: tombstoned %d agent(s) (history kept; muster gc --purge-agents deletes departed/dead rows)\n", tombstoned); err != nil {
			return err
		}
	}

	cutoff := strconv.FormatInt(clock.NowMillis()-eventsKeep.Milliseconds(), 10)
	pruneRaw, pruneErr := callData("prune_events", map[string]any{"older_than_ms": cutoff})
	if pruneErr != nil {
		_, _ = fmt.Fprintf(out, "gc: prune_events failed: %v\n", pruneErr)
		return fmt.Errorf("prune_events failed: %w", pruneErr)
	}
	var res struct {
		Pruned int64 `json:"pruned"`
	}
	if err := json.Unmarshal(pruneRaw, &res); err != nil {
		_, _ = fmt.Fprintf(out, "gc: prune_events failed: %v\n", err)
		return fmt.Errorf("prune_events failed: %w", err)
	}
	_, err = fmt.Fprintf(out, "pruned %d event(s)\n", res.Pruned)
	return err
}
