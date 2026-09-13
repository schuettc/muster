package mcpserver

import (
	"encoding/json"
	"sort"

	"github.com/schuettc/muster/internal/harnessenv"
	"github.com/schuettc/muster/internal/tmuxenv"
)

var captureCallerTmux = tmuxenv.CaptureEnv
var captureCallerHarness = harnessenv.FromEnv

type callerIdentity struct {
	Agent       rosterRow
	Registered  bool
	LiveAliases []string
}

func resolveCallerIdentity() (callerIdentity, error) {
	c := captureCallerTmux()
	h := captureCallerHarness()
	socketPath, sessionID, sessionCreated := c.SocketPath, c.SessionID, c.SessionCreated
	if socketPath == "" || c.PaneID == "" {
		socketPath, sessionID, sessionCreated = "", h.SessionID, 0
	}
	if sessionID == "" {
		return callerIdentity{}, nil
	}

	raw, err := callDaemon("session_aliases", map[string]any{
		"socket_path": socketPath, "session_id": sessionID, "session_created": sessionCreated,
	})
	if err != nil {
		return callerIdentity{}, err
	}
	var lineage struct {
		Aliases []string `json:"aliases"`
	}
	if err := json.Unmarshal(raw, &lineage); err != nil {
		return callerIdentity{}, err
	}
	if len(lineage.Aliases) == 0 {
		return callerIdentity{}, nil
	}

	raw, err = callDaemon("list_agents", nil)
	if err != nil {
		return callerIdentity{}, err
	}
	var rows []rosterRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return callerIdentity{}, err
	}
	owned := make(map[string]bool, len(lineage.Aliases))
	for _, alias := range lineage.Aliases {
		owned[alias] = true
	}
	var live []rosterRow
	for _, row := range rows {
		if owned[row.Alias] && !row.Departed {
			live = append(live, row)
		}
	}
	if len(live) == 0 {
		return callerIdentity{}, nil
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Alias < live[j].Alias })
	primary := live[0]
	if socketPath != "" {
		for _, row := range live {
			if row.SocketPath == socketPath && row.SessionID == sessionID && row.PaneID == c.PaneID && row.SessionCreated == sessionCreated {
				primary = row
				break
			}
		}
	}
	aliases := make([]string, len(live))
	for i, row := range live {
		aliases[i] = row.Alias
	}
	return callerIdentity{Agent: primary, Registered: true, LiveAliases: aliases}, nil
}

func agentViewOf(row rosterRow) AgentView {
	return AgentView{
		Alias: row.Alias, Role: row.Role, ModelType: row.ModelType,
		SessionName: row.SessionName, DeviceName: row.DeviceName,
		Project: row.Project, Label: row.Label, LabelManual: row.LabelManual,
		Departed: row.Departed, RegisteredAt: row.RegisteredAt, LastSeen: row.LastSeen,
	}
}
