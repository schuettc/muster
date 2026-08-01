package humancli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/schuettc/muster/internal/harnessenv"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// cmdHook implements "muster hook <SessionStart|SessionEnd|Stop> [model]" —
// the single entry point an agent harness's hook config points at directly
// (in place of a copied contrib/muster-session-hook.sh). model defaults to
// "claude" when omitted.
//
// The stdin payload is read once here and handed to every branch: harnesses
// that host sessions outside tmux (daemon-hosted sessions — see harnessenv)
// leave tmuxenv.CaptureEnv empty, and the payload's session_id/cwd are then
// the ONLY identity a hook has, for SessionStart's register just as much as
// for Stop's inbox check.
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
	payload, _ := io.ReadAll(io.LimitReader(stdin, 1<<20)) // a hook payload is small; the cap only guards a pathological writer
	switch args[0] {
	case "SessionStart":
		c := hookCapture()
		h := harnessenv.FromHookPayload(payload)
		if c.SocketPath != "" && c.PaneID != "" {
			if hookMayClaimIdentity(c) {
				hookRegisterPane(c, h, model)
			}
		} else {
			hookSessionStartPaneless(h, model)
		}
	case "SessionEnd":
		hookSessionEnd(tmuxenv.CaptureEnv(), harnessenv.FromHookPayload(payload))
	case "Stop":
		hookStop(payload, out)
	}
	return nil
}

// hookCapture resolves the tmux identity a hook acts on: the environment
// when the harness passed it through, else the process-ancestry walk —
// hooks are spawned env-stripped, but a synchronous hook is still a
// descendant of its pane's shell (see tmuxenv.CaptureFromAncestry). The
// zero Capture (both paths empty) means genuinely paneless.
func hookCapture() tmuxenv.Capture {
	if c := tmuxenv.CaptureEnv(); c.SocketPath != "" && c.PaneID != "" {
		return c
	}
	return tmuxenv.CaptureFromAncestry()
}

// hookRegisterPane registers the session-name alias for a pane-anchored
// SessionStart. It cannot delegate to cmdRegister: that reads the tmux
// identity from the ENVIRONMENT, which a stripped hook doesn't have — the
// capture c (env or ancestry walk) is the truth here.
func hookRegisterPane(c tmuxenv.Capture, h harnessenv.Capture, model string) {
	alias := hookAlias(c)
	if alias == "" {
		return
	}
	_, _ = callData("register_agent", map[string]any{
		"alias": alias, "role": "", "model_type": model,
		"session_name": c.SessionName, "session_id": c.SessionID,
		"session_created":    c.SessionCreated,
		"harness_session_id": h.SessionID,
		"socket_path":        c.SocketPath, "pane_id": c.PaneID,
		"project": c.Project, "label": c.Label, "label_manual": c.LabelManual,
	})
}

// hookSessionStartPaneless auto-registers a session that has no tmux pane in
// its environment (harness daemon-hosted sessions) on the paneless tuple
// ("", harness session UUID). $MUSTER_ALIAS is explicit operator intent and
// registers exactly (plain upsert). Otherwise the alias is ALLOCATED from
// the payload cwd's basename via allocPanelessAlias — every session in a
// directory derives the same base, so uniqueness (dotfiles, dotfiles-2, …)
// must be allocated, never taken over: the takeover this replaces let a
// second session in the same directory silently steal the first one's
// identity and inbox. A session that already owns aliases on its tuple
// (resume) refreshes the first instead of allocating a new one.
func hookSessionStartPaneless(h harnessenv.Capture, model string) {
	regFn := func(alias string, ifAbsent bool) error {
		_, err := callData("register_agent", registerPanelessArgs(alias, "", model, h, ifAbsent))
		return err
	}
	if alias := os.Getenv("MUSTER_ALIAS"); alias != "" {
		_ = regFn(alias, false)
		return
	}
	if h.SessionID == "" && h.Alias() == "" {
		return // no resolvable identity: never dial the daemon from an identity-less hook
	}
	if owned := harnessOwnedRows(h.SessionID); len(owned) > 0 {
		// This session already has an identity — the pane-side launch
		// handshake pre-registered a tmux-anchored row, or a prior life of
		// this session (resume) left one. A live row needs nothing from
		// this hook; if every owned row is a tombstone, revive the first
		// with its stored identity intact.
		for _, ag := range owned {
			if !ag.Departed {
				return
			}
		}
		reviveRow(owned[0], model)
		return
	}
	_, _ = allocPanelessAlias(h.Alias(), h.SessionID, regFn)
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
	if ag.Departed {
		return true // a tombstone never owns the identity
	}
	if ag.SocketPath != c.SocketPath || ag.SessionID != c.SessionID {
		return true // cross-session takeover: a renamed/recreated session reclaims its name
	}
	if ag.PaneID == "" || ag.PaneID == c.PaneID {
		return true
	}
	return !tmuxenv.IsPaneAlive(c.SocketPath, ag.PaneID)
}

// hookSessionEnd deregisters EVERY alias the dying pane owns, not just the
// one hookAlias resolves. A session that registered a custom alias (via the
// register_agent tool or `muster register <alias>`) on top of its
// session-name alias used to leave the custom one behind: @muster_agent
// lives on the tmux session, which outlives the Claude session, so the next
// agent in that pane silently inherited the leftover identity AND its inbox
// — the Stop hook then instructed it to act on another agent's mail (the
// lake-broker incident, thread 85). Ownership is judged per alias with the
// same predicate hookOwnsIdentity applies to one: same (socket_path,
// session_id) tuple, not departed, and the row's pane is unset or this pane
// — so a dying sibling (subagent) pane still cannot tombstone the primary's
// registrations. Outside tmux the paneless tuple ("", harness session UUID)
// is enumerated the same way — every alias this harness session registered is
// tombstoned; with no harness identity either, the single-identity gate is
// all there is, exactly as before.
func hookSessionEnd(c tmuxenv.Capture, h harnessenv.Capture) {
	if c.SocketPath == "" || c.SessionID == "" {
		if h.SessionID != "" {
			hookSessionEndPaneless(h)
			return
		}
		if hookOwnsIdentity(c) {
			_ = cmdDeregister(nil, io.Discard)
		}
		return
	}
	raw, err := callData("list_agents", nil)
	if err != nil {
		return // hooks never block a session on a dead daemon
	}
	var rows []agentRow
	if json.Unmarshal(raw, &rows) != nil {
		return
	}
	for _, ag := range rows {
		if ag.Departed || ag.SocketPath != c.SocketPath || ag.SessionID != c.SessionID {
			continue
		}
		if ag.PaneID != "" && ag.PaneID != c.PaneID {
			continue // a sibling pane's identity: not ours to tombstone
		}
		_ = cmdDeregister([]string{ag.Alias}, io.Discard)
	}
}

// hookSessionEndPaneless tombstones every row the dying harness session owns
// — handshake-registered tmux rows and paneless rows alike (harnessOwnedRows'
// two match shapes). Rows of other sessions, or with no harness link at all
// (pre-handshake registrations), are never this session's to tombstone.
func hookSessionEndPaneless(h harnessenv.Capture) {
	for _, ag := range harnessOwnedRows(h.SessionID) {
		if ag.Departed {
			continue
		}
		_ = cmdDeregister([]string{ag.Alias}, io.Discard)
	}
}

// hookOwnsIdentity is the SessionEnd gate for the no-tmux fallback (and the
// single-identity ownership predicate hookSessionEnd generalizes): deregister
// only what this pane owns — a dying sibling (subagent) must not tombstone
// the primary.
func hookOwnsIdentity(c tmuxenv.Capture) bool {
	if hookAlias(c) == "" {
		// No resolvable identity (global hooks, non-tmux sessions): nothing to
		// deregister, and never dial the daemon from an identity-less hook.
		return false
	}
	ag, found := hookGetAgent(hookAlias(c))
	if !found {
		return false
	}
	if ag.Departed {
		return false // a tombstone is already gone: nothing live to deregister
	}
	if ag.SocketPath != c.SocketPath || ag.SessionID != c.SessionID {
		return false
	}
	return ag.PaneID == "" || ag.PaneID == c.PaneID
}

// stopInput decodes the Stop-hook stdin payload. Invalid or empty JSON leaves
// it at its zero value, matching the contrib script's tolerant
// `jq -r '.stop_hook_active // false'`. Cursor uses loop_count and status for
// the same loop guard.
type stopInput struct {
	StopHookActive bool   `json:"stop_hook_active"`
	LoopCount      int    `json:"loop_count"`
	Status         string `json:"status"`
}

// stopReason is the JSON payload muster prints on stdout for a Stop hook
// finding unread mail: {"decision":"block","reason":"..."}. Claude Code and
// Codex both treat decision:block as "run reason as the next prompt"; Cursor
// accepts the same shape (with stopInput's loop guard preventing a loop).
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
func hookStop(payload []byte, out io.Writer) {
	var in stopInput
	_ = json.Unmarshal(payload, &in) // invalid/empty JSON -> zero value (false)
	if in.StopHookActive || in.LoopCount > 0 {
		return // loop guard: we already triggered a continuation this cycle
	}
	if in.Status == "aborted" || in.Status == "error" {
		return
	}
	if os.Getenv("TMUX") == "" {
		hookStopPaneless(harnessenv.FromHookPayload(payload), out)
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

	// Repair missing harness links (durable-alias spec: the stamp's Stop
	// half). An alias registered via the MCP tool in an env carrying no
	// harness UUID has no link for a future resume to find; every Stop
	// payload carries the UUID, so stamp it here. This runs only when the
	// mail gate above already opened — the cheap @muster_inbox check stays
	// the sole decider of whether Stop dials the daemon at all, so a
	// mail-less session costs nothing (documented residual: a link-less
	// alias that never receives mail is never auto-stamped and rides the
	// re-register-by-transcript contract instead).
	stampHarnessLinks(aliases, harnessenv.FromHookPayload(payload), socketPath, sessionID)

	if !hookStopOwnsAnyAlias(aliases) {
		return // roster names a live owner and it isn't me: don't drain a sibling's mail
	}

	label := tmuxenv.CurrentSessionOption(tmuxenv.LabelOption())
	reason := hookReason(total, action, aliases, label)
	b, err := json.Marshal(stopReason{Decision: "block", Reason: reason})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(out, string(b)) // best-effort: a hook's stdout write failing has nowhere to report to
}

// hookStopPaneless is the Stop-hook inbox check for sessions with no tmux in
// their environment (harness daemon-hosted sessions). Such a session has no
// @muster_inbox tmux option to serve as the cheap firing gate, so this dials
// the daemon directly on every Stop — two ops over a local unix socket, the
// price of daemon-hosted mail. Identity resolves by harness session UUID
// (harnessOwnedRows): handshake-registered tmux rows and paneless rows
// alike. An unregistered session prints nothing, and unlike the tmux path
// there is no session-name fallback to address — a harness identity either
// resolved from the roster or doesn't exist.
func hookStopPaneless(h harnessenv.Capture, out io.Writer) {
	if h.SessionID == "" {
		return
	}
	var aliases []string
	var label string
	var tup *agentRow
	owned := harnessOwnedRows(h.SessionID)
	for i, ag := range owned {
		if ag.Departed {
			continue
		}
		aliases = append(aliases, ag.Alias)
		if label == "" && ag.LabelManual {
			label = ag.Label
		}
		if tup == nil && ag.SocketPath != "" && ag.SessionID != "" {
			tup = &owned[i]
		}
	}
	if len(aliases) == 0 {
		return
	}
	// Unread comes from ONE tuple: the handshake row's tmux tuple when the
	// session has one (its mail lives there), else the paneless tuple. A
	// split identity spanning both would need two queries whose overlapping
	// threads can't be deduplicated client-side — the aliases list still
	// names every identity, so nothing goes undrained.
	sock, sid := "", h.SessionID
	if tup != nil {
		sock, sid = tup.SocketPath, tup.SessionID
	}
	total, action, ok := sessionUnreadForHook(sock, sid)
	if !ok || total <= 0 {
		return
	}
	b, err := json.Marshal(stopReason{Decision: "block", Reason: hookReason(total, action, aliases, label)})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(out, string(b)) // best-effort: a hook's stdout write failing has nowhere to report to
}

// stampHarnessLinks attaches h.SessionID to every alias of MY tuple that
// lacks a harness link. Best-effort per alias: a failed get or stamp is
// skipped — hooks never block a session.
func stampHarnessLinks(aliases []string, h harnessenv.Capture, socketPath, sessionID string) {
	if h.SessionID == "" {
		return
	}
	for _, alias := range aliases {
		ag, ok := hookGetAgent(alias)
		if !ok || ag.Departed || ag.HarnessSessionID != "" ||
			ag.SocketPath != socketPath || ag.SessionID != sessionID {
			continue
		}
		_, _ = callData("stamp_harness_session", map[string]any{
			"alias": alias, "harness_session_id": h.SessionID,
		})
	}
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
		if !found || ag.Departed || ag.PaneID == "" {
			continue // a tombstoned row neither grants nor denies ownership
		}
		if ag.PaneID == myPane {
			return true
		}
		sawNamedOwner = true
	}
	return !sawNamedOwner
}

// hookReason builds the Stop hook's decision:block reason. When the session
// carries a task label (the operator's chosen name — prefix T / `muster
// label`), the reason leads with it ("You are 'standard 2000'") so the agent
// learns the label vocabulary the operator thinks in; the alias stays in the
// tool instructions because aliases are what the tools accept. An empty
// label renders the pre-label wording unchanged. The instruction line is
// singular for exactly one alias and a for-each across all of them when the
// session has more than one — a split-identity session must drain every
// alias. Both variants end with the CLI fallback (cliFallback).
func hookReason(total, action int, aliases []string, label string) string {
	countLine := fmt.Sprintf("You have %d unread muster thread(s)", total)
	if action > 0 {
		countLine += fmt.Sprintf(", %d needing action", action)
	}

	if len(aliases) <= 1 {
		alias := ""
		if len(aliases) == 1 {
			alias = aliases[0]
		}
		identity := fmt.Sprintf("Your muster alias is '%s' (this tmux session).", alias)
		if label != "" {
			identity = fmt.Sprintf("You are '%s' — muster alias '%s' (this tmux session).", label, alias)
		}
		return fmt.Sprintf(
			"%s. %s "+
				"Call your muster get_inbox tool now with alias '%s', read each new thread with get_thread, "+
				"handle the request, and reply with the muster reply tool only if the sender needs something from you "+
				"(never reply just to acknowledge an ack or closure; close out with fyi=true so nobody is woken). "+
				"Act autonomously — do not ask the user. "+
				cliFallback,
			countLine, identity, alias, alias, alias,
		)
	}

	quoted := make([]string, len(aliases))
	for i, a := range aliases {
		quoted[i] = "'" + a + "'"
	}
	identity := fmt.Sprintf("Your muster aliases are %s (this tmux session).", strings.Join(quoted, ", "))
	if label != "" {
		identity = fmt.Sprintf("You are '%s' — muster aliases %s (this tmux session).", label, strings.Join(quoted, ", "))
	}
	return fmt.Sprintf(
		"%s. %s "+
			"For EACH alias call get_inbox, read each new thread with get_thread, handle the request, "+
			"and reply with the muster reply tool only if the sender needs something from you "+
			"(never reply just to acknowledge an ack or closure; close out with fyi=true so nobody is woken). "+
			"Act autonomously — do not ask the user. "+
			cliFallback,
		countLine, identity, "<alias>", "<alias>",
	)
}

// cliFallback is the shell-equivalent loop appended to every hook reason.
// %s is the alias to drain (the literal placeholder "<alias>" in the
// multi-alias variant, where the agent substitutes each of its own).
const cliFallback = "(If the muster MCP tools are unavailable, the muster CLI is equivalent: " +
	"`muster inbox '%s'`, `muster thread <id>`, `muster reply <id> \"...\" --from '%s' [--fyi]`.)"
