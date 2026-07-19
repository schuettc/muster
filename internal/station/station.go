package station

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
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

// FlagSet declares station's flags, exported so muster's CLI command
// registry (internal/humancli) can render `muster help station` /
// `muster station -h` and detect flag.ErrHelp before ever spinning up the
// full-screen TUI, without keeping a second, driftable copy of the flag
// list. Run below builds its OWN identical FlagSet rather than calling this
// one directly, because it needs typed *string/*bool/*int pointers back —
// keep the two declarations in sync if you touch either.
func FlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("station", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Duration("interval", time.Second, "poll interval")
	fs.Bool("aliases", false, "show raw aliases instead of current labels")
	fs.Int("width", 0, "line budget in columns (default $COLUMNS or 120)")
	fs.String("alias", "station", "this station's registration alias (so two stations on one machine don't collide)")
	return fs
}

// Run parses station's flags, registers its tmux identity on the bus
// (collision-safe fail-over per spec §5), and runs the Bubble Tea program.
//
// Exit does NOT deregister (spec iteration-8: "operator read-state survives
// restarts"). A prior version deregistered — conditionally but
// unconditionally-on-every-exit-path — which DELETED the agent row,
// including last_read_entry_id: the operator's own read watermark. That
// meant every station restart resurrected already-acknowledged mail as
// unread (an operator-diagnosed regression: "📬 2 came back" after a plain
// quit/relaunch). Leaving the row in place instead means: tmux-liveness
// (get_agent/list_agents' own liveness check, which shells to real tmux
// rather than trusting this row) correctly shows the row DEAD between runs —
// nothing here claims station is still live while it isn't — and the NEXT
// `muster station` launch's registerStation call re-registers the exact same
// alias, whose upsert (store.Store.RegisterAgent) already preserves
// last_read_entry_id verbatim (it simply isn't one of the columns the ON
// CONFLICT clause overwrites — see agents.go), so the operator's read
// watermark survives the restart intact.
//
// The `muster gc` caveat noted in earlier revisions of this comment is
// resolved: gc's default reap now tombstones (DepartAgent, departed=1)
// instead of hard-deleting, so a dead station row — including its read
// watermark — survives gc exactly like a plain quit/relaunch. Only the
// explicit, opt-in `muster gc --purge-agents` still hard-deletes (the old
// behavior), which is an operator's deliberate choice, not gc's default.
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

	if _, runErr := p.Run(); runErr != nil {
		return fmt.Errorf("station: %w", runErr)
	}
	return nil
}

// maxStationSuffix bounds the alias-2, alias-3, … fail-over search, so a
// persistently misbehaving daemon (every candidate reports found+live) fails
// loudly instead of looping forever.
const maxStationSuffix = 50

// agentTuple is the (socket_path, session_id, session_created) slice of
// get_agent's response station needs for collision/ownership comparisons —
// session_created feeds the liveness check's recycled-session-ID
// discrimination (see tmuxenv.IsSessionAlive).
type agentTuple struct {
	Found bool `json:"found"`
	Agent struct {
		SocketPath     string `json:"socket_path"`
		SessionID      string `json:"session_id"`
		SessionCreated int64  `json:"session_created"`
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
		if deadTakeover && tmuxenv.IsSessionAlive(res.Agent.SocketPath, res.Agent.SessionID, res.Agent.SessionCreated) {
			continue // LIVE collision with a different tuple: try the next suffix
		}
		regArgs := map[string]any{
			"alias": candidate, "role": "operator", "model_type": "station",
			"session_name": c.SessionName, "session_id": c.SessionID,
			"session_created": c.SessionCreated,
			"socket_path":     c.SocketPath, "pane_id": c.PaneID,
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
