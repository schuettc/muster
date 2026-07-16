package station

import (
	"encoding/json"
	"flag"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/render"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// daemonCaller is station's own daemon-transport wrapper (a render.Caller
// implementation) — the same shape as humancli's callData, deliberately not
// shared: each peer client of the daemon owns its own transport plumbing, both
// ultimately through internal/client.Call. "Keep transport in one place"
// means one place per consumer, not a module shared between humancli and
// station.
type daemonCaller struct{}

func (daemonCaller) Call(op string, args map[string]any) (json.RawMessage, error) {
	resp, err := client.Call(paths.SocketPath(), proto.Request{Op: op, Args: args})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s: %s", op, resp.Error)
	}
	b, err := json.Marshal(resp.Data)
	if err != nil {
		return nil, fmt.Errorf("marshal %s result: %w", op, err)
	}
	return b, nil
}

// Run parses station's flags, registers its tmux identity on the bus
// (collision-safe fail-over per spec §5), runs the Bubble Tea program, and
// deregisters on a clean quit. TUI init/run failure rolls the registration
// back rather than leaving a phantom agent behind.
func Run(args []string) error {
	fs := flag.NewFlagSet("station", flag.ContinueOnError)
	interval := fs.Duration("interval", time.Second, "poll interval")
	aliases := fs.Bool("aliases", false, "show raw aliases instead of current labels")
	width := fs.Int("width", 0, "line budget in columns (default $COLUMNS or 120)")
	alias := fs.String("alias", "station", "this station's registration alias (so two stations on one machine don't collide)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: muster station [--interval <dur>] [--aliases] [--width <cols>] [--alias <name>]")
	}

	caller := daemonCaller{}
	c := tmuxenv.CaptureEnv()
	registeredAlias, err := registerStation(caller, *alias, c)
	if err != nil {
		return fmt.Errorf("station: register: %w", err)
	}

	m := NewModel(caller, Options{Interval: *interval, Aliases: *aliases, Width: *width, Alias: registeredAlias})
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, runErr := p.Run(); runErr != nil {
		// Init/run failure: roll back registration unconditionally — no
		// other station could have raced onto this brand-new alias yet.
		_, _ = caller.Call("deregister_agent", map[string]any{"alias": registeredAlias})
		return fmt.Errorf("station: %w", runErr)
	}
	deregisterIfStillOurs(caller, registeredAlias, c)
	return nil
}

// maxStationSuffix bounds the alias-2, alias-3, … fail-over search, so a
// persistently misbehaving daemon (every candidate reports found+live) fails
// loudly instead of looping forever.
const maxStationSuffix = 50

// agentTuple is the (socket_path, session_id) half of get_agent's response
// station needs for collision/ownership comparisons.
type agentTuple struct {
	Found bool `json:"found"`
	Agent struct {
		SocketPath string `json:"socket_path"`
		SessionID  string `json:"session_id"`
	} `json:"agent"`
}

func getAgentTuple(caller render.Caller, alias string) (agentTuple, error) {
	raw, err := caller.Call("get_agent", map[string]any{"alias": alias})
	if err != nil {
		return agentTuple{}, err
	}
	var res agentTuple
	if err := json.Unmarshal(raw, &res); err != nil {
		return agentTuple{}, err
	}
	return res, nil
}

// registerStation registers alias (or alias-2, alias-3, … on a LIVE
// same-alias collision with a different session tuple — spec §5 identity,
// "a dead record is taken over") and returns the alias actually registered.
func registerStation(caller render.Caller, alias string, c tmuxenv.Capture) (string, error) {
	for n := 1; n <= maxStationSuffix; n++ {
		candidate := alias
		if n > 1 {
			candidate = fmt.Sprintf("%s-%d", alias, n)
		}
		res, err := getAgentTuple(caller, candidate)
		if err != nil {
			return "", err
		}
		sameTuple := res.Agent.SocketPath == c.SocketPath && res.Agent.SessionID == c.SessionID
		if res.Found && !sameTuple && tmuxenv.IsSessionAlive(res.Agent.SocketPath, res.Agent.SessionID) {
			continue // LIVE collision with a different tuple: try the next suffix
		}
		if _, err := caller.Call("register_agent", map[string]any{
			"alias": candidate, "role": "operator", "model_type": "station",
			"session_name": c.SessionName, "session_id": c.SessionID,
			"socket_path": c.SocketPath, "pane_id": c.PaneID,
			"project": c.Project, "label": c.Label, "label_manual": c.LabelManual,
		}); err != nil {
			return "", err
		}
		return candidate, nil
	}
	return "", fmt.Errorf("no free station alias after %s..%s-%d (every candidate is a live collision)", alias, alias, maxStationSuffix)
}

// deregisterIfStillOurs re-fetches alias and deregisters only if its session
// tuple still matches c — a second station (or a re-registered agent) that
// took the alias over is never deleted by this one's exit (spec §5:
// deregistration is CONDITIONAL). Best-effort: errors are swallowed, same as
// a clean quit that can no longer usefully report anything.
func deregisterIfStillOurs(caller render.Caller, alias string, c tmuxenv.Capture) {
	res, err := getAgentTuple(caller, alias)
	if err != nil || !res.Found {
		return
	}
	if res.Agent.SocketPath == c.SocketPath && res.Agent.SessionID == c.SessionID {
		_, _ = caller.Call("deregister_agent", map[string]any{"alias": alias})
	}
}
