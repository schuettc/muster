package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
)

// standing_set creates a listable order; standing_list returns it; a second
// set under the same key replaces it; standing_retract clears it.
func TestStandingToolsLifecycle(t *testing.T) {
	startTestDaemon(t)
	if _, err := callDaemon("register_agent", map[string]any{"alias": "web1", "project": "web", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}

	if _, out, err := standingSetHandler(context.Background(), nil, StandingSetIn{
		From: "web1", Project: "web", Body: "rebase before push",
	}); err != nil || out.ThreadID == 0 {
		t.Fatalf("standing_set: err=%v out=%+v", err, out)
	}

	_, list, err := standingListHandler(context.Background(), nil, StandingListIn{Project: "web"})
	if err != nil {
		t.Fatalf("standing_list: %v", err)
	}
	if len(list.Orders) != 1 || list.Orders[0].Key != "invariants" || list.Orders[0].Body != "rebase before push" {
		t.Fatalf("standing_list = %+v", list.Orders)
	}

	// Replace under the default key.
	if _, _, err := standingSetHandler(context.Background(), nil, StandingSetIn{
		From: "web1", Project: "web", Body: "rebase before push (v2)",
	}); err != nil {
		t.Fatalf("standing_set replace: %v", err)
	}
	_, list, _ = standingListHandler(context.Background(), nil, StandingListIn{Project: "web"})
	if len(list.Orders) != 1 || list.Orders[0].Body != "rebase before push (v2)" {
		t.Fatalf("after replace, list = %+v", list.Orders)
	}

	// Retract.
	_, chg, err := standingRetractHandler(context.Background(), nil, StandingRetractIn{From: "web1", Project: "web"})
	if err != nil || !chg.Changed {
		t.Fatalf("standing_retract: err=%v changed=%v", err, chg.Changed)
	}
	_, list, _ = standingListHandler(context.Background(), nil, StandingListIn{Project: "web"})
	if len(list.Orders) != 0 {
		t.Fatalf("after retract, list = %+v, want empty", list.Orders)
	}
}

func TestStandingSetUnknownProjectRejected(t *testing.T) {
	startTestDaemon(t)
	if _, err := callDaemon("register_agent", map[string]any{"alias": "web1", "project": "web", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := standingSetHandler(context.Background(), nil, StandingSetIn{From: "web1", Project: "wbe", Body: "x"}); err == nil {
		t.Fatal("standing_set to an unknown project must be rejected")
	}
}

// standing_retract_thread retracts an ad-hoc standing broadcast by id — the
// one kind standing_retract cannot name — and a repeat is a no-op.
func TestStandingRetractThreadTool(t *testing.T) {
	startTestDaemon(t)
	if _, err := callDaemon("register_agent", map[string]any{"alias": "web1", "project": "web", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	raw, err := callDaemon("send_message", map[string]any{"from": "web1", "to_kind": "broadcast", "to_target": "web", "body": "ad-hoc", "standing": true, "confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	var sent ThreadIDOut
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatal(err)
	}
	_, chg, err := standingRetractThreadHandler(context.Background(), nil, StandingRetractThreadIn{From: "web1", ThreadID: sent.ThreadID})
	if err != nil || !chg.Changed {
		t.Fatalf("standing_retract_thread: err=%v changed=%v", err, chg.Changed)
	}
	_, chg, err = standingRetractThreadHandler(context.Background(), nil, StandingRetractThreadIn{From: "web1", ThreadID: sent.ThreadID})
	if err != nil || chg.Changed {
		t.Fatalf("repeat must be a no-op: err=%v changed=%v", err, chg.Changed)
	}
}
