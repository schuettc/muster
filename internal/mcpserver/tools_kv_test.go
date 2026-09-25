package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
	"github.com/schuettc/tools-common/harness"
)

func stubKVCaller(t *testing.T, aliasesJSON, agentsJSON string, handle func(string, map[string]any) (json.RawMessage, error)) {
	t.Helper()
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	stubCallerCaptures(t, tmuxenv.Capture{}, harness.Capture{SessionID: "hs-1"})
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		switch op {
		case "session_aliases":
			return json.RawMessage(aliasesJSON), nil
		case "list_agents":
			return json.RawMessage(agentsJSON), nil
		default:
			return handle(op, args)
		}
	}
}

func TestKVSetUsesProvenCallerAlias(t *testing.T) {
	stubKVCaller(t, `{"aliases":["actor"]}`, `[{"alias":"actor"}]`, func(op string, args map[string]any) (json.RawMessage, error) {
		if op != "kv_set" || args["key"] != "api.base" || args["value"] != "url" || args["by"] != "actor" {
			t.Fatalf("kv_set call: op=%q args=%+v", op, args)
		}
		return json.RawMessage(`null`), nil
	})

	_, out, err := kvSetHandler(context.Background(), nil, KVSetIn{Key: "api.base", Value: "url"})
	if err != nil || !out.OK {
		t.Fatalf("set: out=%+v err=%v", out, err)
	}
}

func TestKVDeleteUsesProvenCallerAlias(t *testing.T) {
	stubKVCaller(t, `{"aliases":["actor"]}`, `[{"alias":"actor"}]`, func(op string, args map[string]any) (json.RawMessage, error) {
		if op != "kv_delete" || args["key"] != "api.base" || args["by"] != "actor" {
			t.Fatalf("kv_delete call: op=%q args=%+v", op, args)
		}
		return json.RawMessage(`{"deleted":true}`), nil
	})

	_, out, err := kvDeleteHandler(context.Background(), nil, KVDeleteIn{Key: "api.base"})
	if err != nil || !out.Deleted || out.Key != "api.base" {
		t.Fatalf("delete: out=%+v err=%v", out, err)
	}
}

func TestKVMutationsRejectUnregisteredCaller(t *testing.T) {
	stubKVCaller(t, `{"aliases":[]}`, `[]`, func(op string, _ map[string]any) (json.RawMessage, error) {
		t.Fatalf("unregistered caller reached %q", op)
		return nil, nil
	})
	if _, _, err := kvSetHandler(context.Background(), nil, KVSetIn{Key: "k", Value: "v"}); err == nil {
		t.Fatal("kv_set must reject an unregistered caller")
	}
	if _, _, err := kvDeleteHandler(context.Background(), nil, KVDeleteIn{Key: "k"}); err == nil {
		t.Fatal("kv_delete must reject an unregistered caller")
	}
}

func TestKVListForwardsLiteralPrefix(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		if op != "kv_list" || args["prefix"] != "api.%" {
			t.Fatalf("kv_list call: op=%q args=%+v", op, args)
		}
		return json.RawMessage(`{"pairs":[{"key":"api.%","value":"v","updated_by":"actor","updated_at":10}]}`), nil
	}

	_, out, err := kvListHandler(context.Background(), nil, KVListIn{Prefix: "api.%"})
	if err != nil || len(out.Pairs) != 1 || out.Pairs[0].Key != "api.%" {
		t.Fatalf("list: out=%+v err=%v", out, err)
	}
}

func TestKVGetMissingAndFound(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	responses := []json.RawMessage{
		json.RawMessage(`{"found":true,"pair":{"key":"api.base","value":"url","updated_by":"actor","updated_at":10}}`),
		json.RawMessage(`{"found":false}`),
	}
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		if op != "kv_get" {
			t.Fatalf("unexpected op %q", op)
		}
		out := responses[0]
		responses = responses[1:]
		return out, nil
	}

	_, got, err := kvGetHandler(context.Background(), nil, KVGetIn{Key: "api.base"})
	if err != nil || !got.Found || got.Value != "url" || got.UpdatedBy != "actor" {
		t.Fatalf("found: out=%+v err=%v", got, err)
	}
	_, missing, err := kvGetHandler(context.Background(), nil, KVGetIn{Key: "nope"})
	if err != nil || missing.Found {
		t.Fatalf("missing: out=%+v err=%v", missing, err)
	}
}
