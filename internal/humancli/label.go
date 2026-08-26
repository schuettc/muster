package humancli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/schuettc/muster/internal/nudge"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// newLabelFlagsWithVals declares label's flags and returns typed access to
// their values — shared by cmdLabel (real parsing) and newLabelFlags
// (registry help/man rendering).
func newLabelFlagsWithVals() (fs *flag.FlagSet, clearFlag, noInject *bool) {
	fs = flag.NewFlagSet("label", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	clearFlag = fs.Bool("clear", false, "clear this session's label")
	noInject = fs.Bool("no-inject", false, "skip typing /rename into the live agent pane (for callers whose name ALREADY came from the harness side, e.g. the statusline promoting a /rename)")
	return fs, clearFlag, noInject
}

// newLabelFlags builds label's flag.FlagSet for registry-driven help/man
// rendering.
func newLabelFlags() *flag.FlagSet {
	fs, _, _ := newLabelFlagsWithVals()
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
//
// --no-inject skips the syncAgentName /rename typing below — everything else
// is identical. It exists for callers whose name ALREADY came from the
// harness side (the statusline promoting a name that originated from a
// /rename the agent itself typed): re-injecting it would loop the same text
// back into a live pane.
func cmdLabel(args []string, out io.Writer) error {
	fs, clearFlag, noInject := newLabelFlagsWithVals()
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
	// sessionCreated is the ambient incarnation proof (spec §5.1): the same
	// capture hookCapture uses ambient-side (tmuxenv.CaptureEnv), so the
	// set_label push can only ever land on THIS session's rows, never a
	// recycled-ID ghost. socket/sessionID keep their existing ambient reads
	// (SocketFromEnv/CurrentSessionID) — only SessionCreated is new here.
	sessionCreated := tmuxenv.CaptureEnv().SessionCreated
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
		syncLabelToBus(out, "", false, socket, sessionID, sessionCreated)
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
	syncLabelToBus(out, name, true, socket, sessionID, sessionCreated)
	if !*noInject && socket != "" && sessionID != "" {
		syncAgentName(out, name, socket, sessionID)
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
//
// sessionCreated forwards the caller's proof of incarnation (spec §5.1) so
// the store-side write (SetSessionLabel) can never land on a recycled-ID
// ghost; a caller with no proof (0) still pushes, but the store then updates
// nothing, degrading to the same "refreshes on next register" story.
func syncLabelToBus(out io.Writer, label string, manual bool, socket, sessionID string, sessionCreated int64) {
	if socket == "" || sessionID == "" {
		return
	}
	if _, err := callData("set_label", map[string]any{
		"socket_path": socket, "session_id": sessionID, "session_created": sessionCreated,
		"label": label, "label_manual": manual,
	}); err != nil {
		_, _ = fmt.Fprintf(out, "warning: bus label sync failed (%v); the stored label refreshes on this session's next register\n", err)
	}
}

// syncAgentName types "/rename <name>" into this session's registered live
// Claude Code or Cursor pane so its session name follows the label — making
// prefix T (which shells out to `muster label`) the ONE naming gesture for
// tmux, the bus, and the agent harness. Codex has no /rename, so it gets no
// injection. Strictly gated on the roster: a non-departed claude- or
// cursor-model row on this exact session tuple whose pane is still alive. A
// session with no supported live agent (plain shell, Codex, dead pane) gets no
// injection — the roster is the definition of a supported harness here, not
// pane_current_command sniffing. Also gated on session incarnation
// (tmuxenv.IsSessionAlive): tmux recycles session IDs across server
// restarts, so a stale un-reaped row can match this exact tuple yet name a
// pane that now belongs to a completely different, fresh session — typing
// "/rename" into that pane would be renaming a stranger. Best-effort like
// syncLabelToBus: a skipped or failed injection never fails the label
// write. Clearing never injects (there is no "/rename to nothing" gesture
// worth typing at a session).
func syncAgentName(out io.Writer, name, socket, sessionID string) {
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
		if ag.Departed || renameCommand(ag.ModelType) == "" || ag.SocketPath != socket ||
			ag.SessionID != sessionID || ag.PaneID == "" {
			continue
		}
		if !tmuxenv.IsPaneAlive(socket, ag.PaneID) {
			continue
		}
		if !tmuxenv.IsSessionAlive(socket, ag.SessionID, ag.SessionCreated) {
			continue
		}
		if _, err := typer.TypeLine(socket, ag.PaneID, ag.ModelType, renameCommand(ag.ModelType)+name, true); err != nil {
			_, _ = fmt.Fprintf(out, "warning: %s session rename failed (%v); run %s %s in %s yourself\n", ag.ModelType, err, strings.TrimSpace(renameCommand(ag.ModelType)), name, ag.ModelType)
			return
		}
		_, _ = fmt.Fprintf(out, "renamed %s session to match (pane %s)\n", ag.ModelType, ag.PaneID)
		return // one live supported harness per session; first match wins
	}
}

// renameCommand is the slash command that sets a harness's own session
// title, with its trailing space, or "" for a harness that has none (codex)
// or that muster does not know — those panes are never typed into. pi gets
// its NATIVE `/name`. pi's harness extension has no rename command of its
// own — it reacts to pi's `session_info_changed` event and calls
// `become --no-inject`, so typing `/name` here propagates without looping.
func renameCommand(modelType string) string {
	switch modelType {
	case "claude", "cursor":
		return "/rename "
	case "pi":
		return "/name "
	}
	return ""
}
