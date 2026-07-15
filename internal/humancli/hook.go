package humancli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/schuettc/muster/internal/tmuxenv"
)

// cmdHook implements "muster hook <SessionStart|SessionEnd|Stop> [model]" —
// the single entry point an agent harness's hook config points at directly
// (in place of a copied contrib/muster-session-hook.sh). model defaults to
// "claude" when omitted.
//
// A hook must never block a session, so cmdHook always returns nil: every
// internal error is swallowed, and on any input other than a recognized
// event it is simply a no-op.
func cmdHook(args []string, stdin io.Reader, out io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: muster hook <SessionStart|SessionEnd|Stop> [model]")
	}
	model := "claude"
	if len(args) > 1 && args[1] != "" {
		model = args[1]
	}
	switch args[0] {
	case "SessionStart":
		_ = cmdRegister([]string{"--model", model}, io.Discard)
	case "SessionEnd":
		_ = cmdDeregister(nil, io.Discard)
	case "Stop":
		hookStop(stdin, out)
	}
	return nil
}

// stopInput decodes the Stop-hook stdin payload. Invalid or empty JSON leaves
// it at its zero value (StopHookActive=false), matching the contrib script's
// tolerant `jq -r '.stop_hook_active // false'`.
type stopInput struct {
	StopHookActive bool `json:"stop_hook_active"`
}

// stopReason is the JSON payload muster prints on stdout for a Stop hook
// finding unread mail: {"decision":"block","reason":"..."}. Claude Code and
// Codex both treat decision:block as "run reason as the next prompt".
type stopReason struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// hookStop ports contrib/muster-session-hook.sh's Stop branch to Go, with
// identical semantics: a self-resolving inbox check. If this tmux session has
// unread muster mail (the @muster_inbox option the daemon sets), it prints one
// line of decision:block JSON telling the agent to drain its inbox
// autonomously; otherwise it prints nothing. Best-effort throughout — stdin
// read/decode failures, a missing tmux, or a missing/non-numeric/zero count
// all fall through to "print nothing".
func hookStop(stdin io.Reader, out io.Writer) {
	var in stopInput
	if b, err := io.ReadAll(stdin); err == nil {
		_ = json.Unmarshal(b, &in) // invalid/empty JSON -> zero value (false)
	}
	if in.StopHookActive {
		return // loop guard: we already triggered a continuation this cycle
	}
	if os.Getenv("TMUX") == "" {
		return
	}
	count, err := strconv.Atoi(tmuxenv.CurrentSessionOption("@muster_inbox"))
	if err != nil || count <= 0 {
		return
	}
	alias := tmuxenv.CurrentSessionName()
	reason := fmt.Sprintf(
		"You have %d unread muster message(s). Your muster alias is '%s' (this tmux session). "+
			"Call your muster get_inbox tool now with alias '%s', read each new thread with get_thread, "+
			"handle the request, and reply with the muster reply tool. Act autonomously — do not ask the user.",
		count, alias, alias,
	)
	b, err := json.Marshal(stopReason{Decision: "block", Reason: reason})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(out, string(b)) // best-effort: a hook's stdout write failing has nowhere to report to
}
