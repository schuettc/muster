package humancli

import (
	"encoding/json"
	"fmt"

	"github.com/schuettc/muster/internal/harnessenv"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// panelessAliasCap bounds the suffix probe. Fifty sessions in one directory
// mirrors proj's own session-numbering cap; hitting it means something is
// leaking registrations, and failing loudly beats alias -51.
const panelessAliasCap = 50

// allocPanelessAlias claims a UNIQUE alias for a paneless session: base, then
// base-2, base-3, … (proj's session-numbering vocabulary). Every session in a
// directory derives the same base — the tmux model got uniqueness for free
// from numbered session names, so the paneless model must allocate it.
// Per-candidate rules:
//
//   - my own row (same paneless tuple), departed or not → reuse it (resume)
//   - a tombstone → revive-takeover (a departed row never owns a name)
//   - a tmux row whose pane is provably dead → takeover (the existing
//     cross-session-reclaim philosophy; liveness IS checkable there)
//   - a live tmux owner, or another session's paneless row → next suffix.
//     A paneless row's liveness is unknowable, so it is presumed live and
//     never stolen — that presumption is exactly what keeps two sessions in
//     one directory from silently trading an identity (and its inbox) back
//     and forth. A crashed session's stale row squats its name until
//     `muster deregister <alias>` or `gc --purge-agents`; suffixing past it
//     costs one integer.
//
// Free candidates are claimed with if_absent=true — the daemon's CAS — so two
// sessions launching in one directory at the same instant cannot both win a
// name; the loser just moves to the next suffix. register is the caller's
// actual registration op (hook vs CLI carry different flags); ifAbsent is
// forwarded to it. Returns the claimed alias.
func allocPanelessAlias(base, sessionID string, register func(alias string, ifAbsent bool) error) (string, error) {
	if base == "" {
		return "", fmt.Errorf("no alias base to allocate from")
	}
	for i := 1; i <= panelessAliasCap; i++ {
		cand := base
		if i > 1 {
			cand = fmt.Sprintf("%s-%d", base, i)
		}
		ag, found := hookGetAgent(cand)
		switch {
		case !found:
			if err := register(cand, true); err != nil {
				continue // lost the CAS race (or transient failure): next suffix
			}
			return cand, nil
		case ag.SocketPath == "" && ag.SessionID != "" && ag.SessionID == sessionID:
			// Mine already (resume/refresh) — plain upsert, tuple unchanged.
			if err := register(cand, false); err != nil {
				return "", err
			}
			return cand, nil
		case ag.Departed:
			if err := register(cand, false); err != nil {
				return "", err
			}
			return cand, nil
		case ag.SocketPath != "" && ag.PaneID != "" && !tmuxenv.IsPaneAlive(ag.SocketPath, ag.PaneID):
			// A tmux row whose pane is provably gone: reclaim.
			if err := register(cand, false); err != nil {
				return "", err
			}
			return cand, nil
		default:
			continue // live tmux owner, or another session's paneless row
		}
	}
	return "", fmt.Errorf("no free paneless alias within %s..%s-%d", base, base, panelessAliasCap)
}

// registerPanelessArgs builds the register_agent payload for a paneless
// registration — the one canonical shape: empty socket/pane, the harness
// session UUID as both session_id (the tuple) and harness_session_id (the
// uniform lookup key shared with handshake-launched tmux rows), project from
// the enclosing checkout.
func registerPanelessArgs(alias, role, model string, h harnessenv.Capture, ifAbsent bool) map[string]any {
	return map[string]any{
		"alias": alias, "role": role, "model_type": model,
		"session_name": "", "session_id": h.SessionID, "session_created": 0,
		"harness_session_id": h.SessionID,
		"socket_path":        "", "pane_id": "",
		"project": h.Project(), "label": "", "label_manual": false,
		"if_absent": ifAbsent,
	}
}

// harnessOwnedRows returns every roster row belonging to harness session
// uuid — handshake-registered tmux rows (harness_session_id) and paneless
// rows (tuple ("", uuid)) alike, tombstones included so resume can revive.
// Empty on any daemon failure.
func harnessOwnedRows(uuid string) []agentRow {
	if uuid == "" {
		return nil
	}
	raw, err := callData("list_agents", nil)
	if err != nil {
		return nil
	}
	var rows []agentRow
	if json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	var mine []agentRow
	for _, ag := range rows {
		if ag.HarnessSessionID == uuid || (ag.SocketPath == "" && ag.SessionID == uuid) {
			mine = append(mine, ag)
		}
	}
	return mine
}

// reviveRow re-registers a tombstoned row echoing back its own stored
// identity — tuple, harness link, project, label — so revival never rewrites
// what the row already knows about itself. Returns the daemon's ack (outcome
// + unread) so callers that want to surface it can; callers that don't care
// may call this as a bare statement — Go permits discarding a value-returning
// function's result.
func reviveRow(ag agentRow, model string) registerAck {
	if model == "" {
		model = ag.ModelType
	}
	raw, _ := callData("register_agent", map[string]any{
		"alias": ag.Alias, "role": ag.Role, "model_type": model,
		"session_name": ag.SessionName, "session_id": ag.SessionID,
		"session_created":    ag.SessionCreated,
		"harness_session_id": ag.HarnessSessionID,
		"socket_path":        ag.SocketPath, "pane_id": ag.PaneID,
		"project": ag.Project, "label": ag.Label, "label_manual": ag.LabelManual,
	})
	return decodeRegisterAck(raw)
}
