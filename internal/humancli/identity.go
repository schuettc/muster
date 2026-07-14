package humancli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// cmdRegister registers the current tmux session as an agent. Alias precedence:
// explicit positional arg → $MUSTER_ALIAS → tmux session name.
func cmdRegister(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	role := fs.String("role", "", "this agent's role")
	model := fs.String("model", "claude", "model backing this agent: claude or codex")
	// register has no boolean flags: --role and --model both take values, so
	// pass an empty bool-flags set (an implicit default would wrongly reuse
	// send's --role, which IS boolean there).
	flagArgs, rest := splitFlagsAndPositional(args, map[string]bool{})
	if err := fs.Parse(flagArgs); err != nil {
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
		"socket_path": c.SocketPath, "pane_id": c.PaneID,
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
	alias := ""
	if len(args) > 0 {
		alias = args[0]
	} else if os.Getenv("MUSTER_ALIAS") != "" {
		alias = os.Getenv("MUSTER_ALIAS")
	} else {
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

// cmdGC deregisters every agent whose tmux session is no longer alive.
func cmdGC(out io.Writer) error {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return err
	}
	var agents []agentRow
	if err := json.Unmarshal(raw, &agents); err != nil {
		return err
	}
	reaped := 0
	for _, a := range agents {
		if tmuxenv.IsSessionAlive(a.SocketPath, a.SessionID) {
			continue
		}
		if _, err := callData("deregister_agent", map[string]any{"alias": a.Alias}); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "reaped %s (dead session)\n", a.Alias); err != nil {
			return err
		}
		reaped++
	}
	_, err = fmt.Fprintf(out, "gc: reaped %d\n", reaped)
	return err
}
