package humancli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/schuettc/muster/internal/nudge"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// newLabelFlagsWithVals declares label's flags and returns typed access to
// their values — shared by cmdLabel (real parsing) and newLabelFlags
// (registry help/man rendering).
func newLabelFlagsWithVals() (fs *flag.FlagSet, clearFlag *bool) {
	fs = flag.NewFlagSet("label", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	clearFlag = fs.Bool("clear", false, "clear this session's label")
	return fs, clearFlag
}

// newLabelFlags builds label's flag.FlagSet for registry-driven help/man
// rendering.
func newLabelFlags() *flag.FlagSet {
	fs, _ := newLabelFlagsWithVals()
	return fs
}

// cmdLabel implements "muster label <name>" / "muster label --clear": naming
// (or clearing) the current tmux session's label in one command, in place of
// the two tmux set-option incantations an operator would otherwise type by
// hand. It requires $TMUX (there is no "current session" outside tmux).
//
// The tmux option is only half the write: the daemon's resolver reads the
// STORED label (it is tmux-agnostic by rule), so cmdLabel also pushes the
// change to the bus via the set_label op (see syncLabelToBus). Without that
// push, a CLI sender resolving against live tmux and an MCP sender resolving
// against the store would disagree until the session's next re-register.
func cmdLabel(args []string, out io.Writer) error {
	fs, clearFlag := newLabelFlagsWithVals()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpFor("label", out)
		}
		return err
	}
	rest := fs.Args()
	name := ""
	if len(rest) > 0 {
		name = rest[0]
	}
	if os.Getenv("TMUX") == "" {
		return fmt.Errorf("muster label requires a tmux session ($TMUX is unset)")
	}
	opt := tmuxenv.LabelOption()
	manualOpt := opt + "_manual"
	if *clearFlag || name == "" {
		if err := tmuxenv.UnsetSessionOption(opt); err != nil {
			return err
		}
		if err := tmuxenv.UnsetSessionOption(manualOpt); err != nil {
			return err
		}
		_ = tmuxenv.RefreshClient() // best-effort: repaint title bars
		socket := tmuxenv.SocketFromEnv()
		sessionID := tmuxenv.CurrentSessionID()
		syncLabelToBus(out, "", false, socket, sessionID)
		_, err := fmt.Fprintln(out, "label cleared")
		return err
	}
	if err := tmuxenv.SetSessionOption(opt, name); err != nil {
		return err
	}
	if err := tmuxenv.SetSessionOption(manualOpt, "1"); err != nil {
		return err
	}
	_ = tmuxenv.RefreshClient() // best-effort: repaint title bars
	socket := tmuxenv.SocketFromEnv()
	sessionID := tmuxenv.CurrentSessionID()
	syncLabelToBus(out, name, true, socket, sessionID)
	if socket != "" && sessionID != "" {
		syncClaudeName(out, name, socket, sessionID)
	}
	_, err := fmt.Fprintf(out, "labeled this session %q (%s)\n", name, opt)
	return err
}

// syncLabelToBus lands the just-written tmux label in the store via the
// set_label op, for every alias registered to the ambient session, so the
// daemon's tmux-agnostic resolver agrees with live tmux immediately. The
// tmux option is already set when this runs, and that option is the source
// of truth the store mirrors — so a failed push degrades to the OLD
// behavior (stored copy refreshes at the next register_agent), never a
// wrong label. It therefore warns rather than fails, and stays silent for
// a session with no registered agents (updated=0): labeling before
// registering is routine, and register captures the option anyway.
func syncLabelToBus(out io.Writer, label string, manual bool, socket, sessionID string) {
	if socket == "" || sessionID == "" {
		return
	}
	if _, err := callData("set_label", map[string]any{
		"socket_path": socket, "session_id": sessionID,
		"label": label, "label_manual": manual,
	}); err != nil {
		_, _ = fmt.Fprintf(out, "warning: bus label sync failed (%v); the stored label refreshes on this session's next register\n", err)
	}
}

// syncClaudeName types "/rename <name>" into this session's registered live
// Claude pane so the Claude Code session name follows the label — making
// prefix T (which shells out to `muster label`) the ONE naming gesture for
// tmux, the bus, and Claude. Strictly gated on the roster: a non-departed
// claude-model row on this exact session tuple whose pane is still alive. A
// session with no live Claude (plain shell, codex, dead pane) gets no
// injection — the roster is the definition of "Claude Code runs here", not
// pane_current_command sniffing. Best-effort like syncLabelToBus: a skipped
// or failed injection never fails the label write. Clearing never injects
// (there is no "/rename to nothing" gesture worth typing at a session).
func syncClaudeName(out io.Writer, name, socket, sessionID string) {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return // no daemon → no roster to gate on; the tmux label already landed
	}
	var rows []agentRow
	if json.Unmarshal(raw, &rows) != nil {
		return
	}
	// Route typing through the tmuxenv.Run seam (NOT a zero-value TmuxNudger,
	// whose nil Run spawns real tmux): humancli's tests stub tmuxenv.Run, and
	// one process-spawning seam per package keeps them able to observe this.
	typer := nudge.TmuxNudger{Run: func(args ...string) error {
		_, err := tmuxenv.Run(args...)
		return err
	}}
	for _, ag := range rows {
		if ag.Departed || ag.ModelType != "claude" || ag.SocketPath != socket ||
			ag.SessionID != sessionID || ag.PaneID == "" {
			continue
		}
		if !tmuxenv.IsPaneAlive(socket, ag.PaneID) {
			continue
		}
		if _, err := typer.TypeLine(socket, ag.PaneID, "claude", "/rename "+name, true); err != nil {
			_, _ = fmt.Fprintf(out, "warning: claude session rename failed (%v); run /rename %s in claude yourself\n", err, name)
			return
		}
		_, _ = fmt.Fprintf(out, "renamed claude session to match (pane %s)\n", ag.PaneID)
		return // one live claude per session; first match wins
	}
}
