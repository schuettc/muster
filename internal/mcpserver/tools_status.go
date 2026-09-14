package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/schuettc/muster/internal/store"
)

// GetStatusIn optionally selects one exact alias.
type GetStatusIn struct {
	Alias string `json:"alias,omitempty" jsonschema:"optional exact alias to filter; labels and prefixes do not match"`
}

// GetStatusOut contains side-effect-free inbox counts.
type GetStatusOut struct {
	Agents []store.AliasStatus `json:"agents" jsonschema:"side-effect-free inbox counts for matching aliases"`
}

func getStatusHandler(_ context.Context, _ *mcp.CallToolRequest, in GetStatusIn) (*mcp.CallToolResult, GetStatusOut, error) {
	raw, err := callDaemon("status", nil)
	if err != nil {
		return nil, GetStatusOut{}, err
	}
	var response GetStatusOut
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, GetStatusOut{}, err
	}
	if in.Alias == "" {
		if response.Agents == nil {
			response.Agents = []store.AliasStatus{}
		}
		return nil, response, nil
	}
	filtered := make([]store.AliasStatus, 0, 1)
	for _, row := range response.Agents {
		if row.Alias == in.Alias {
			filtered = append(filtered, row)
		}
	}
	return nil, GetStatusOut{Agents: filtered}, nil
}

func registerStatusTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_status",
		Description: "Read side-effect-free unread and action-required counts for every alias, or for one exact alias. Unknown aliases return an empty list. This does not mark inboxes read or journal a peek.",
	}, getStatusHandler)
}
