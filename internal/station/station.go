package station

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
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
// deregisters on exit. Deregistration is a single deferred, CONDITIONAL
// operation (deregisterOnce — re-fetches the record and deletes only if the
// session tuple still matches ours) that fires on every exit path: a clean
// `q`, SIGINT/SIGTERM (translated into the same p.Quit() teardown `q` uses,
// so the terminal is restored the same way), and a panic — bubbletea itself
// catches a panic inside Update/View, restores the terminal, and returns it
// as Run's error, but the recover here also covers anything escaping that
// guarded region. Init/run failure rolls the registration back too: since
// deregisterIfStillOurs is conditional on the tuple still matching ours, and
// nothing else could plausibly have raced onto a brand-new alias in the
// (sub-second) span between registerStation returning and p.Run() failing,
// the conditional check is a no-op rollback in practice — one code path
// covers both "ran fine then quit" and "never really got going."
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
	dereg := deregisterOnce(caller, registeredAlias, c)

	m := NewModel(caller, Options{Interval: *interval, Aliases: *aliases, Width: *width, Alias: registeredAlias})
	p := tea.NewProgram(m, tea.WithAltScreen())

	// SIGINT/SIGTERM quit the program through the SAME path as pressing `q`
	// (p.Quit() feeds a quit message through the normal Update loop), so the
	// terminal is restored by bubbletea's own teardown either way — there is
	// no separate "signal path" terminal-restore to get wrong.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	stopSignalWatch := make(chan struct{})
	defer close(stopSignalWatch)
	go func() {
		select {
		case <-sigCh:
			p.Quit()
		case <-stopSignalWatch:
		}
	}()

	// Covers a panic escaping p.Run() itself (bubbletea's default panic
	// catching already restores the terminal and converts an Update/View
	// panic into runErr below — this is a backstop, not the primary path).
	defer func() {
		if r := recover(); r != nil {
			dereg()
			panic(r)
		}
	}()

	if _, runErr := p.Run(); runErr != nil {
		dereg()
		return fmt.Errorf("station: %w", runErr)
	}
	dereg()
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
//
// Every candidate that isn't a known dead-tuple takeover is registered with
// if_absent=true (spec §5, deferred from T7): this closes the read-then-write
// gap between this function's own get_agent check above and its
// register_agent call below — if another process races onto the SAME
// candidate in that gap, the daemon's alias-locked CAS (see
// handleRegisterAgent) fails this call instead of one of the two silently
// clobbering the other, and the loop just fails over to the next suffix
// exactly as it would for a live collision caught by the check itself. A
// dead-tuple takeover deliberately omits if_absent — the daemon has no tmux
// liveness of its own, so if_absent's CAS can't distinguish "steal this
// known-dead tuple" from "a live collision raced in"; that call still goes
// through as a plain upsert, same as before this change.
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
		deadTakeover := res.Found && !sameTuple
		if deadTakeover && tmuxenv.IsSessionAlive(res.Agent.SocketPath, res.Agent.SessionID) {
			continue // LIVE collision with a different tuple: try the next suffix
		}
		regArgs := map[string]any{
			"alias": candidate, "role": "operator", "model_type": "station",
			"session_name": c.SessionName, "session_id": c.SessionID,
			"socket_path": c.SocketPath, "pane_id": c.PaneID,
			"project": c.Project, "label": c.Label, "label_manual": c.LabelManual,
		}
		if !deadTakeover {
			regArgs["if_absent"] = true
		}
		if _, err := caller.Call("register_agent", regArgs); err != nil {
			if !deadTakeover && isIfAbsentConflict(err) {
				continue // lost the race for this candidate: try the next suffix
			}
			return "", err
		}
		return candidate, nil
	}
	return "", fmt.Errorf("no free station alias after %s..%s-%d (every candidate is a live collision)", alias, alias, maxStationSuffix)
}

// isIfAbsentConflict reports whether err is register_agent's if_absent CAS
// failure (see handleRegisterAgent) — the one register_agent error
// registerStation treats as "try the next suffix" rather than fatal.
func isIfAbsentConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "if_absent conflict")
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

// deregisterOnce wraps deregisterIfStillOurs in a sync.Once: Run's several
// exit paths (a clean quit, a signal, and a recovered panic) can each try to
// fire the SAME deregistration, and this collapses them to exactly one
// attempt — harmless even without the Once (deregisterIfStillOurs already
// no-ops once the record is gone), but avoids a redundant daemon round trip
// when two paths race to be first.
func deregisterOnce(caller render.Caller, alias string, c tmuxenv.Capture) func() {
	var once sync.Once
	return func() {
		once.Do(func() { deregisterIfStillOurs(caller, alias, c) })
	}
}
