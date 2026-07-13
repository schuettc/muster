package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// KVSetIn is the input for the kv_set tool.
type KVSetIn struct {
	Key   string `json:"key" jsonschema:"the fact key, e.g. api.base or schema.version"`
	Value string `json:"value" jsonschema:"the value to store"`
	By    string `json:"by" jsonschema:"the alias setting the value"`
}

// KVGetIn is the input for the kv_get tool.
type KVGetIn struct {
	Key string `json:"key" jsonschema:"the fact key to read"`
}

// KVGetOut is the output of the kv_get tool.
type KVGetOut struct {
	Found     bool   `json:"found" jsonschema:"whether the key exists"`
	Value     string `json:"value" jsonschema:"the stored value (empty if not found)"`
	UpdatedBy string `json:"updated_by" jsonschema:"who last set the value"`
	UpdatedAt int64  `json:"updated_at" jsonschema:"when it was last set (unix ms)"`
}

// kvGetRaw mirrors the daemon's kv_get response: {"found": bool, "pair": {...}}.
// store.KVPair carries snake_case json tags, so Pair's fields below mirror that.
type kvGetRaw struct {
	Found bool `json:"found"`
	Pair  struct {
		Key       string `json:"key"`
		Value     string `json:"value"`
		UpdatedBy string `json:"updated_by"`
		UpdatedAt int64  `json:"updated_at"`
	} `json:"pair"`
}

func kvSetHandler(_ context.Context, _ *mcp.CallToolRequest, in KVSetIn) (*mcp.CallToolResult, OKOut, error) {
	if _, err := callDaemon("kv_set", map[string]any{"key": in.Key, "value": in.Value, "by": in.By}); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true, Detail: "set " + in.Key}, nil
}

func kvGetHandler(_ context.Context, _ *mcp.CallToolRequest, in KVGetIn) (*mcp.CallToolResult, KVGetOut, error) {
	raw, err := callDaemon("kv_get", map[string]any{"key": in.Key})
	if err != nil {
		return nil, KVGetOut{}, err
	}
	var r kvGetRaw
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, KVGetOut{}, err
	}
	return nil, KVGetOut{Found: r.Found, Value: r.Pair.Value, UpdatedBy: r.Pair.UpdatedBy, UpdatedAt: r.Pair.UpdatedAt}, nil
}

// registerKVTools registers the kv_set and kv_get tools on srv.
func registerKVTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "kv_set", Description: "Set a shared fact on the bus blackboard (e.g. api.base, schema.version)."}, kvSetHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "kv_get", Description: "Read a shared fact from the bus blackboard. Returns found=false if the key is absent."}, kvGetHandler)
}
