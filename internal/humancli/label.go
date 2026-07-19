package humancli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

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
		syncLabelToBus(out, "", false)
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
	syncLabelToBus(out, name, true)
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
func syncLabelToBus(out io.Writer, label string, manual bool) {
	socket := tmuxenv.SocketFromEnv()
	sessionID := tmuxenv.CurrentSessionID()
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
