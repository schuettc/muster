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
	"github.com/schuettc/muster/internal/tmuxenv"
)

// newRegisterFlagsWithVals declares register's flags and returns typed
// access to their values — shared by cmdRegister (real parsing) and
// newRegisterFlags (registry help/man rendering).
func newRegisterFlagsWithVals() (fs *flag.FlagSet, role, model *string) {
	fs = flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	role = fs.String("role", "", "this agent's role")
	model = fs.String("model", "claude", "model backing this agent: claude or codex")
	return fs, role, model
}

// newRegisterFlags builds register's flag.FlagSet for registry-driven
// help/man rendering.
func newRegisterFlags() *flag.FlagSet {
	fs, _, _ := newRegisterFlagsWithVals()
	return fs
}

// cmdRegister registers the current tmux session as an agent. Alias precedence:
// explicit positional arg → $MUSTER_ALIAS → tmux session name.
func cmdRegister(args []string, out io.Writer) error {
	fs, role, model := newRegisterFlagsWithVals()
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
	alias := ""
	switch {
	case len(rest) > 0:
		alias = rest[0]
	case os.Getenv("MUSTER_ALIAS") != "":
		alias = os.Getenv("MUSTER_ALIAS")
	default:
		alias = c.SessionName
	}
	if alias == "" {
		return fmt.Errorf("cannot determine alias: not in a named tmux session; pass one explicitly or set $MUSTER_ALIAS")
	}
	if _, err := callData("register_agent", map[string]any{
		"alias": alias, "role": *role, "model_type": *model,
		"session_name": c.SessionName, "session_id": c.SessionID,
		"session_created": c.SessionCreated,
		"socket_path":     c.SocketPath, "pane_id": c.PaneID,
		"project": c.Project, "label": c.Label, "label_manual": c.LabelManual,
	}); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "registered %s (project %q, model %s)\n", alias, c.Project, *model)
	return err
}

// cmdDeregister removes an agent's registration. Alias precedence mirrors
// register: explicit arg → $MUSTER_ALIAS → tmux session name.
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
	purgeAgents = fs.Bool("purge-agents", false, "hard-delete departed and tmux-dead agent rows instead of tombstoning them (irreversible: identity, project, label, and read-state are all gone)")
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
// irreversible). The agent phase and the event-prune phase are independent: a
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
			if !a.Departed && tmuxenv.IsSessionAlive(a.SocketPath, a.SessionID, a.SessionCreated) {
				continue // still live and never departed: nothing to purge
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
