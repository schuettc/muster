package humancli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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

// ResolveTarget maps a user-supplied target to a unique agent alias, scoped to
// caller's project. Rules (in order): exact alias; qualified proj:label; bare
// addressable label within callerProject. Never silently crosses projects.
func ResolveTarget(agents []enrichedAgent, given, caller string) (string, error) {
	// 1. exact alias (globally unique)
	for _, a := range agents {
		if a.Alias == given {
			return a.Alias, nil
		}
	}
	// 2. qualified proj:label
	if proj, label, ok := strings.Cut(given, ":"); ok {
		var hits []string
		for _, a := range agents {
			if a.Project == proj && a.EffManual && a.EffLabel == label {
				hits = append(hits, a.Alias)
			}
		}
		return uniqueOrErr(hits, given)
	}
	// 3. bare label — restrict to caller's project
	var inProject, elsewhere []string
	for _, a := range agents {
		if a.EffManual && a.EffLabel == given {
			if a.Project == caller {
				inProject = append(inProject, a.Alias)
			} else {
				elsewhere = append(elsewhere, qualify(a.Project, given))
			}
		}
	}
	if len(inProject) > 0 {
		return uniqueOrErr(inProject, given)
	}
	if len(elsewhere) > 0 {
		sort.Strings(elsewhere)
		return "", fmt.Errorf("label %q is not in your project; qualify it: %s", given, strings.Join(elsewhere, ", "))
	}
	return "", fmt.Errorf("unknown agent %q", given)
}

func uniqueOrErr(hits []string, given string) (string, error) {
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("unknown agent %q", given)
	default:
		sort.Strings(hits)
		return "", fmt.Errorf("%q is ambiguous: %s", given, strings.Join(hits, ", "))
	}
}

func qualify(project, label string) string {
	if project == "" {
		return label
	}
	return project + ":" + label
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
