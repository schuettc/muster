package humancli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// newWhereamiFlagsWithVals declares whereami's flags and returns typed
// access to their values — shared by cmdWhereami (real parsing) and
// newWhereamiFlags (registry help/man rendering).
func newWhereamiFlagsWithVals() (fs *flag.FlagSet, jsonOut *bool) {
	fs = flag.NewFlagSet("whereami", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut = fs.Bool("json", false, "print a JSON object instead of the one-line tuple")
	return fs, jsonOut
}

// newWhereamiFlags builds whereami's flag.FlagSet for registry-driven
// help/man rendering.
func newWhereamiFlags() *flag.FlagSet {
	fs, _ := newWhereamiFlagsWithVals()
	return fs
}

// whereamiJSON is the --json shape for `muster whereami`.
type whereamiJSON struct {
	SocketPath  string `json:"socket_path"`
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name"`
	PaneID      string `json:"pane_id"`
	Created     int64  `json:"session_created"`
}

// cmdWhereami prints the tmux identity of the pane this process runs under:
// $TMUX/$TMUX_PANE first (tmuxenv.CaptureEnv), falling back to the process-
// ancestry walk (tmuxenv.CaptureFromAncestry) when the environment is
// stripped, as it is inside an agent harness's hooks. Fails with a non-nil
// error and no stdout when neither resolves — never a cwd guess (the same
// fail-safe posture CaptureFromAncestry itself commits to).
func cmdWhereami(args []string, out io.Writer) error {
	fs, jsonOut := newWhereamiFlagsWithVals()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpFor("whereami", out)
		}
		return err
	}
	c := tmuxenv.CaptureEnv()
	if c.SocketPath == "" || c.PaneID == "" {
		c = tmuxenv.CaptureFromAncestry()
	}
	if c.SocketPath == "" || c.PaneID == "" {
		return fmt.Errorf("whereami: no tmux pane resolved (not in tmux, and no ancestor pid matched a pane)")
	}
	if *jsonOut {
		b, err := json.Marshal(whereamiJSON{
			SocketPath:  c.SocketPath,
			SessionID:   c.SessionID,
			SessionName: c.SessionName,
			PaneID:      c.PaneID,
			Created:     c.SessionCreated,
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(b))
		return err
	}
	_, err := fmt.Fprintf(out, "socket=%s session_id=%s session_name=%s pane=%s created=%d\n",
		c.SocketPath, c.SessionID, c.SessionName, c.PaneID, c.SessionCreated)
	return err
}
