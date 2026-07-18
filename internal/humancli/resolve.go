package humancli

import (
	"encoding/json"

	"github.com/schuettc/muster/internal/resolve"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// enrichedAgent is an agentRow with live tmux-derived state overlaid.
type enrichedAgent struct {
	agentRow
	Live      bool
	EffLabel  string // live label if alive, else stored snapshot
	EffManual bool   // live manual flag if alive, else stored
}

// enrichAgents overlays liveness + live label onto each row. For a live
// session the label is re-read from tmux; for a dead one the stored snapshot
// stands in.
func enrichAgents(rows []agentRow) []enrichedAgent {
	out := make([]enrichedAgent, 0, len(rows))
	for _, a := range rows {
		e := enrichedAgent{agentRow: a, EffLabel: a.Label, EffManual: a.LabelManual}
		e.Live = tmuxenv.IsSessionAlive(a.SocketPath, a.SessionID)
		if e.Live {
			e.EffLabel, e.EffManual = tmuxenv.SessionLabel(a.SocketPath, a.SessionID)
		}
		out = append(out, e)
	}
	return out
}

// callerProject derives the calling shell's project from its own $TMUX socket.
func callerProject() string {
	return tmuxenv.ProjectFromSocket(tmuxenv.SocketFromEnv())
}

// ResolveTarget maps a user-supplied target to a unique agent alias, scoped
// to caller's project. This is a thin CLI-side wrapper over
// internal/resolve.Target — the ONE canonical resolver, shared with the
// daemon's own send_message/task_create validation (see that package's doc
// comment for the full precedence rules and the black-hole incident it
// closes). agents carries live tmux-refreshed labels (enrichAgents); a
// departed row still resolves by exact alias but never by label, exactly
// like the daemon's roster check.
func ResolveTarget(agents []enrichedAgent, given, caller string) (string, error) {
	candidates := make([]resolve.Candidate, len(agents))
	for i, a := range agents {
		candidates[i] = resolve.Candidate{
			Alias: a.Alias, Project: a.Project,
			Label: a.EffLabel, LabelManual: a.EffManual, Departed: a.Departed,
		}
	}
	return resolve.Target(candidates, given, caller)
}

// resolveVia lists agents, enriches them, and resolves given to an alias.
func resolveVia(given string) (string, error) {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return "", err
	}
	var rows []agentRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return "", err
	}
	return ResolveTarget(enrichAgents(rows), given, callerProject())
}
