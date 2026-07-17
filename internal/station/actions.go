package station

import (
	"encoding/json"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/schuettc/muster/internal/render"
)

// This file holds the operator-action Cmds (spec §5): the composer's
// send/reply submit and the roster's confirm-then-nudge — the daemon calls
// that only fire on an explicit operator keystroke, as opposed to poll.go's
// unconditional per-tick fetches.

// nudger is the seam over internal/nudge.TmuxNudger (station is an operator
// surface, so it reuses the SAME send-keys path cmdNudge does rather than a
// parallel implementation — see nudgeCmd) — tests inject a fake so a model
// test never shells out to real tmux.
type nudger interface {
	Nudge(socketPath, paneID, modelType string, submit bool) (bool, error)
}

// nudgeResultMsg carries the outcome of one nudgeCmd.
type nudgeResultMsg struct {
	alias     string
	submitted bool
	err       error
}

// nudgeCmd resolves alias to its registered tmux pane and nudges it —
// exactly cmdNudge's sequence (internal/humancli.cmdNudge): get_agent, then
// TmuxNudger.Nudge, then a best-effort log_event self-report so the journal
// carries the same "nudge" event a CLI-driven nudge would. Errors at any
// step surface on nudgeResultMsg.err; the self-report is deliberately
// best-effort (its own failure doesn't fail the nudge — matching cmdNudge).
func nudgeCmd(caller render.Caller, n nudger, alias string) tea.Cmd {
	return func() tea.Msg {
		raw, err := caller.Call("get_agent", map[string]any{"alias": alias})
		if err != nil {
			return nudgeResultMsg{alias: alias, err: err}
		}
		var res struct {
			Found bool `json:"found"`
			Agent struct {
				SocketPath string `json:"socket_path"`
				PaneID     string `json:"pane_id"`
				ModelType  string `json:"model_type"`
			} `json:"agent"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nudgeResultMsg{alias: alias, err: err}
		}
		if !res.Found {
			return nudgeResultMsg{alias: alias, err: fmt.Errorf("no agent registered as %q", alias)}
		}
		submitted, err := n.Nudge(res.Agent.SocketPath, res.Agent.PaneID, res.Agent.ModelType, true)
		if err != nil {
			return nudgeResultMsg{alias: alias, err: err}
		}
		detail := "typed"
		if submitted {
			detail = "submitted"
		}
		_, _ = caller.Call("log_event", map[string]any{"target": alias, "detail": detail}) // best-effort journal, mirrors cmdNudge
		return nudgeResultMsg{alias: alias, submitted: submitted}
	}
}

// composerSentMsg carries the outcome of one composer submit (send_message
// or reply — see sendMessageCmd/replyCmd).
type composerSentMsg struct {
	kind composerKind
	err  error
}

// sendMessageCmd issues the composer's 's' path via the same send_message op
// the CLI's `muster send` uses.
func sendMessageCmd(caller render.Caller, from, target, body, intent string) tea.Cmd {
	return func() tea.Msg {
		_, err := caller.Call("send_message", map[string]any{
			"from": from, "to_kind": "agent", "to_target": target,
			"body": body, "intent": intent,
		})
		return composerSentMsg{kind: composerKindSend, err: err}
	}
}

// replyCmd issues the composer's 'r' path via the same reply op the daemon
// exposes to every peer client. Reply carries no intent — the op itself has
// no such arg (a reply doesn't change its thread's intent).
func replyCmd(caller render.Caller, from string, threadID int64, body string) tea.Cmd {
	return func() tea.Msg {
		_, err := caller.Call("reply", map[string]any{
			"thread_id": threadID, "from": from, "body": body,
		})
		return composerSentMsg{kind: composerKindReply, err: err}
	}
}
