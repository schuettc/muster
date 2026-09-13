package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
)

func TestGetStatusReturnsAllRowsWithOnePureRead(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	var calls []string
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		calls = append(calls, op)
		return json.RawMessage(`{"agents":[
			{"alias":"live","unread":3,"action_required":1},
			{"alias":"departed","unread":2,"action_required":0}
		]}`), nil
	}

	_, got, err := getStatusHandler(context.Background(), nil, GetStatusIn{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Agents) != 2 || got.Agents[0].Alias != "live" || got.Agents[1].Alias != "departed" {
		t.Fatalf("agents = %+v", got.Agents)
	}
	if len(calls) != 1 || calls[0] != "status" {
		t.Fatalf("daemon calls = %v, want [status]", calls)
	}
}

func TestGetStatusExactAliasFilter(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"agents":[{"alias":"api","unread":3},{"alias":"api-2","unread":4}]}`), nil
	}

	_, got, err := getStatusHandler(context.Background(), nil, GetStatusIn{Alias: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Agents) != 1 || got.Agents[0].Alias != "api" {
		t.Fatalf("agents = %+v", got.Agents)
	}
}

func TestGetStatusUnknownAliasReturnsEmpty(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"agents":[{"alias":"api"}]}`), nil
	}

	_, got, err := getStatusHandler(context.Background(), nil, GetStatusIn{Alias: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Agents == nil || len(got.Agents) != 0 {
		t.Fatalf("agents = %#v, want non-nil empty slice", got.Agents)
	}
}
