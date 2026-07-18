package humancli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

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
	if helpRequested(args) {
		return HelpFor("hook", out)
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: muster hook <SessionStart|SessionEnd|Stop> [model]")
	}
	model := "claude"
	if len(args) > 1 && args[1] != "" {
		model = args[1]
	}
	switch args[0] {
	case "SessionStart":
		if hookMayClaimIdentity(tmuxenv.CaptureEnv()) {
			_ = cmdRegister([]string{"--model", model}, io.Discard)
		}
	case "SessionEnd":
		if hookOwnsIdentity(tmuxenv.CaptureEnv()) {
			_ = cmdDeregister(nil, io.Discard)
		}
	case "Stop":
		hookStop(stdin, out)
	}
	return nil
}

// hookAlias resolves the identity a hook event acts on, mirroring
// cmdRegister/cmdDeregister's no-arg precedence: $MUSTER_ALIAS, else the
// captured tmux session name.
func hookAlias(c tmuxenv.Capture) string {
	if v := os.Getenv("MUSTER_ALIAS"); v != "" {
		return v
	}
	return c.SessionName
}

// hookGetAgent fetches an alias's full roster row via the daemon's get_agent
// op, decoded exactly like cmdNudge's pane resolution. false on any
// transport/daemon failure or a not-found alias — callers degrade to
// today's behavior in that case, never block on it.
func hookGetAgent(alias string) (agentFull, bool) {
	raw, err := callData("get_agent", map[string]any{"alias": alias})
	if err != nil {
		return agentFull{}, false
	}
	var res struct {
		Found bool      `json:"found"`
		Agent agentFull `json:"agent"`
	}
	if json.Unmarshal(raw, &res) != nil || !res.Found {
		return agentFull{}, false
	}
	return res.Agent, true
}

// hookMayClaimIdentity is the SessionStart gate (spec: first live claimant
// wins the session's primary-agent pane). Degrades to true — today's
// register — whenever tmux identity or the roster can't answer.
func hookMayClaimIdentity(c tmuxenv.Capture) bool {
	if c.SocketPath == "" || c.PaneID == "" {
		return true
	}
	ag, found := hookGetAgent(hookAlias(c))
	if !found {
		return true
	}
	if ag.SocketPath != c.SocketPath || ag.SessionID != c.SessionID {
		return true // cross-session takeover: a renamed/recreated session reclaims its name
	}
	if ag.PaneID == "" || ag.PaneID == c.PaneID {
		return true
	}
	return !tmuxenv.IsPaneAlive(c.SocketPath, ag.PaneID)
}

// hookOwnsIdentity is the SessionEnd gate: deregister only what this pane
// owns — a dying sibling (subagent) must not tombstone the primary.
func hookOwnsIdentity(c tmuxenv.Capture) bool {
	ag, found := hookGetAgent(hookAlias(c))
	if !found {
		return false
	}
	if ag.SocketPath != c.SocketPath || ag.SessionID != c.SessionID {
		return false
	}
	return ag.PaneID == "" || ag.PaneID == c.PaneID
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
//
// The @muster_inbox option remains the cheap FIRING gate (spec §3): only when
// it reads > 0 does the hook go on to call the daemon at all. From there it
// captures the (socket_path, session_id) tuple and asks the daemon for the
// session's full alias list (session_aliases) and its true unread/action
// counts (session_unread) — a session with a split identity (a tmux-name
// alias plus a chosen one) must drain both, not just the alias the hook
// happened to read the option from. Either call's failure (or an empty
// alias list) falls back to today's single session-name behavior so the
// hook never goes silent because of a daemon hiccup.
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
	optCount, err := strconv.Atoi(tmuxenv.CurrentSessionOption("@muster_inbox"))
	if err != nil || optCount <= 0 {
		return // cheap gate: no daemon calls unless the tmux option says there's mail
	}

	socketPath := tmuxenv.SocketFromEnv()
	sessionID := tmuxenv.CurrentSessionID()

	total, action, ok := sessionUnreadForHook(socketPath, sessionID)
	if !ok {
		total, action = optCount, 0 // fall back to the tmux option value on op failure
	}
	if total <= 0 {
		return
	}
	aliases := sessionAliasesForHook(socketPath, sessionID)

	if !hookStopOwnsAnyAlias(aliases) {
		return // roster names a live owner and it isn't me: don't drain a sibling's mail
	}

	reason := hookReason(total, action, aliases)
	b, err := json.Marshal(stopReason{Decision: "block", Reason: reason})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(out, string(b)) // best-effort: a hook's stdout write failing has nowhere to report to
}

// sessionAliasesForHook calls the session_aliases op and returns the sorted,
// deduplicated alias list for the (socket_path, session_id) tuple. Any
// transport/daemon failure or an empty result falls back to a single-element
// list holding today's session-name wording (spec §3) — the hook always has
// something to address.
func sessionAliasesForHook(socketPath, sessionID string) []string {
	raw, err := callData("session_aliases", map[string]any{"socket_path": socketPath, "session_id": sessionID})
	if err == nil {
		var res struct {
			Aliases []string `json:"aliases"`
		}
		if json.Unmarshal(raw, &res) == nil && len(res.Aliases) > 0 {
			return res.Aliases
		}
	}
	return []string{tmuxenv.CurrentSessionName()}
}

// sessionUnreadForHook calls the session_unread op. ok is false on any
// transport/daemon failure, signaling the caller to fall back to the
// @muster_inbox option's count (with no action-count breakdown available).
func sessionUnreadForHook(socketPath, sessionID string) (total, action int, ok bool) {
	raw, err := callData("session_unread", map[string]any{"socket_path": socketPath, "session_id": sessionID})
	if err != nil {
		return 0, 0, false
	}
	var res struct {
		Total  int `json:"total"`
		Action int `json:"action"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return 0, 0, false
	}
	return res.Total, res.Action, true
}

// hookStopOwnsAnyAlias is the Stop gate (spec §2): drain only when this pane
// is a named owner. It resolves each alias's stored pane_id via get_agent —
// cheap, local, at most a couple of daemon round trips — and engages ONLY
// when the roster actually names a live pane_id for at least one alias and
// none of them is mine. An empty $TMUX_PANE, an unresolvable roster, or every
// alias having no stored pane_id all mean the roster can't name an owner, so
// this falls through to true — today's unconditional drain.
func hookStopOwnsAnyAlias(aliases []string) bool {
	myPane := os.Getenv("TMUX_PANE")
	if myPane == "" {
		return true
	}
	sawNamedOwner := false
	for _, alias := range aliases {
		ag, found := hookGetAgent(alias)
		if !found || ag.PaneID == "" {
			continue
		}
		if ag.PaneID == myPane {
			return true
		}
		sawNamedOwner = true
	}
	return !sawNamedOwner
}

// hookReason builds the Stop hook's decision:block reason (spec §2 drain
// wording, §3 multi-alias drain). The count line states unread threads and
// appends an action-needed count only when > 0. The instruction line is
// singular (today's wording, unchanged) for exactly one alias, and a
// for-each instruction across all of them when the session has more than
// one — a split-identity session must drain every alias, not just the one
// the hook happened to observe.
func hookReason(total, action int, aliases []string) string {
	countLine := fmt.Sprintf("You have %d unread muster thread(s)", total)
	if action > 0 {
		countLine += fmt.Sprintf(", %d needing action", action)
	}

	if len(aliases) <= 1 {
		alias := ""
		if len(aliases) == 1 {
			alias = aliases[0]
		}
		return fmt.Sprintf(
			"%s. Your muster alias is '%s' (this tmux session). "+
				"Call your muster get_inbox tool now with alias '%s', read each new thread with get_thread, "+
				"handle the request, and reply with the muster reply tool. Act autonomously — do not ask the user.",
			countLine, alias, alias,
		)
	}

	quoted := make([]string, len(aliases))
	for i, a := range aliases {
		quoted[i] = "'" + a + "'"
	}
	return fmt.Sprintf(
		"%s. Your muster aliases are %s (this tmux session). "+
			"For EACH alias call get_inbox, read each new thread with get_thread, handle the request, "+
			"and reply with the muster reply tool. Act autonomously — do not ask the user.",
		countLine, strings.Join(quoted, ", "),
	)
}
