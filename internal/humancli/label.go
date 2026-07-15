package humancli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// cmdLabel implements "muster label <name>" / "muster label --clear": naming
// (or clearing) the current tmux session's label in one command, in place of
// the two tmux set-option incantations an operator would otherwise type by
// hand. It requires $TMUX (there is no "current session" outside tmux).
func cmdLabel(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("label", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	clearFlag := fs.Bool("clear", false, "clear this session's label")
	if err := fs.Parse(args); err != nil {
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
	_, err := fmt.Fprintf(out, "labeled this session %q (%s)\n", name, opt)
	return err
}
