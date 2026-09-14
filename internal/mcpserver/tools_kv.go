package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/schuettc/muster/internal/store"
)

// KVSetIn is the input for the kv_set tool.
type KVSetIn struct {
	Key   string `json:"key" jsonschema:"the fact key, e.g. api.base or schema.version"`
	Value string `json:"value" jsonschema:"the value to store"`
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

// KVListIn optionally filters blackboard keys by a literal prefix.
type KVListIn struct {
	Prefix string `json:"prefix,omitempty" jsonschema:"optional literal key prefix"`
}

// KVListOut contains matching pairs in lexicographic key order.
type KVListOut struct {
	Pairs []store.KVPair `json:"pairs" jsonschema:"matching blackboard pairs"`
}

// KVDeleteIn names the blackboard key to remove.
type KVDeleteIn struct {
	Key string `json:"key" jsonschema:"the fact key to delete"`
}

// KVDeleteOut reports whether the key existed.
type KVDeleteOut struct {
	Key     string `json:"key" jsonschema:"the requested fact key"`
	Deleted bool   `json:"deleted" jsonschema:"whether an existing pair was removed"`
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
	identity, err := resolveCallerIdentity()
	if err != nil {
		return nil, OKOut{}, err
	}
	if !identity.Registered {
		return nil, OKOut{}, fmt.Errorf("kv_set requires a registered caller; call current_agent first")
	}
	if _, err := callDaemon("kv_set", map[string]any{"key": in.Key, "value": in.Value, "by": identity.Agent.Alias}); err != nil {
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

func kvListHandler(_ context.Context, _ *mcp.CallToolRequest, in KVListIn) (*mcp.CallToolResult, KVListOut, error) {
	raw, err := callDaemon("kv_list", map[string]any{"prefix": in.Prefix})
	if err != nil {
		return nil, KVListOut{}, err
	}
	var out KVListOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, KVListOut{}, err
	}
	if out.Pairs == nil {
		out.Pairs = []store.KVPair{}
	}
	return nil, out, nil
}

func kvDeleteHandler(_ context.Context, _ *mcp.CallToolRequest, in KVDeleteIn) (*mcp.CallToolResult, KVDeleteOut, error) {
	identity, err := resolveCallerIdentity()
	if err != nil {
		return nil, KVDeleteOut{}, err
	}
	if !identity.Registered {
		return nil, KVDeleteOut{}, fmt.Errorf("kv_delete requires a registered caller; call current_agent first")
	}
	raw, err := callDaemon("kv_delete", map[string]any{"key": in.Key, "by": identity.Agent.Alias})
	if err != nil {
		return nil, KVDeleteOut{}, err
	}
	var result struct {
		Deleted bool `json:"deleted"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, KVDeleteOut{}, err
	}
	return nil, KVDeleteOut{Key: in.Key, Deleted: result.Deleted}, nil
}

// registerKVTools registers the blackboard tools on srv.
func registerKVTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "kv_set", Description: "Set a shared fact on the bus blackboard. Attribution is derived from the calling session; callers cannot choose updated_by."}, kvSetHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "kv_get", Description: "Read a shared fact from the bus blackboard. Returns found=false if the key is absent."}, kvGetHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "kv_list", Description: "List the complete small blackboard, optionally filtered by a literal key prefix, in lexicographic key order."}, kvListHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "kv_delete", Description: "Delete a shared fact. Attribution is derived from the calling session; deletion is idempotent."}, kvDeleteHandler)
}
