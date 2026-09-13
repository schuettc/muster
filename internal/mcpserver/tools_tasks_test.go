package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
)

func TestListTasksForwardsFiltersAndTruncation(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		if op != "list_tasks" {
			t.Fatalf("unexpected op %q", op)
		}
		if args["project"] != "muster" || args["from"] != "author" || args["to_kind"] != "role" ||
			args["to_target"] != "reviewer" || args["limit"] != 25 {
			t.Fatalf("args = %+v", args)
		}
		statuses, ok := args["statuses"].([]string)
		if !ok || len(statuses) != 2 || statuses[0] != "open" || statuses[1] != "blocked" {
			t.Fatalf("statuses = %#v", args["statuses"])
		}
		return json.RawMessage(`{"tasks":[{"id":9,"kind":"task","from_agent":"author","to_kind":"role","to_target":"reviewer","subject":"Review","ref":"repo=x","status":"blocked","created_at":10,"updated_at":20,"last_from":"reviewer","entry_count":3}],"truncated":true}`), nil
	}

	_, got, err := listTasksHandler(context.Background(), nil, ListTasksIn{
		Project: "muster", Statuses: []string{"open", "blocked"}, From: "author",
		ToKind: "role", ToTarget: "reviewer", Limit: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(got.Tasks) != 1 || got.Tasks[0].ID != 9 || got.Tasks[0].LastFrom != "reviewer" || got.Tasks[0].EntryCount != 3 {
		t.Fatalf("list_tasks = %+v", got)
	}
}

func TestListTasksDefaultsOmitOptionalArgumentsAndReturnEmptyList(t *testing.T) {
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		if op != "list_tasks" || len(args) != 0 {
			t.Fatalf("call = %q %+v", op, args)
		}
		return json.RawMessage(`{"tasks":[],"truncated":false}`), nil
	}

	_, got, err := listTasksHandler(context.Background(), nil, ListTasksIn{})
	if err != nil || got.Tasks == nil || len(got.Tasks) != 0 || got.Truncated {
		t.Fatalf("list_tasks = %+v err=%v", got, err)
	}
}

func TestTaskCreateClaimTransition(t *testing.T) {
	startTestDaemon(t)
	// rev2 exists only so the second claim below fails for the reason this
	// test is about — the store's atomic claim — rather than bouncing off
	// task_claim's actor-existence check and pinning nothing.
	for _, alias := range []string{"backend", "rev1", "rev2"} {
		if _, err := callDaemon("register_agent", map[string]any{
			"alias": alias, "role": "producer", "model_type": "claude",
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, created, err := taskCreateHandler(context.Background(), nil, TaskCreateIn{
		From: "backend", ToKind: "role", ToTarget: "reviewer",
		Subject: "Review feat/wagers", Ref: "repo=bhw branch=feat/wagers", Body: "please review",
	})
	if err != nil || created.ThreadID == 0 {
		t.Fatalf("create: err=%v out=%+v", err, created)
	}

	if _, out, err := taskClaimHandler(context.Background(), nil, TaskClaimIn{ThreadID: created.ThreadID, By: "rev1"}); err != nil || !out.OK {
		t.Fatalf("claim: err=%v out=%+v", err, out)
	}
	// A second claim must fail (atomic claim in the store).
	if _, _, err := taskClaimHandler(context.Background(), nil, TaskClaimIn{ThreadID: created.ThreadID, By: "rev2"}); err == nil {
		t.Fatalf("second claim should error")
	}

	if _, out, err := taskTransitionHandler(context.Background(), nil, TaskTransitionIn{
		ThreadID: created.ThreadID, By: "rev1", Status: "completed", Note: "LGTM",
	}); err != nil || !out.OK {
		t.Fatalf("transition: err=%v out=%+v", err, out)
	}

	_, thr, err := getThreadHandler(context.Background(), nil, GetThreadIn(created))
	if err != nil {
		t.Fatalf("get_thread: %v", err)
	}
	if thr.Thread.Status != "completed" {
		t.Fatalf("status should be completed, got %q", thr.Thread.Status)
	}
}

// TestTaskCreateIntentPassesThrough proves task_create's optional Intent
// field reaches the daemon and lands on the thread.
func TestTaskCreateIntentPassesThrough(t *testing.T) {
	startTestDaemon(t)
	if _, err := callDaemon("register_agent", map[string]any{
		"alias": "backend", "role": "producer", "model_type": "claude",
	}); err != nil {
		t.Fatal(err)
	}
	_, created, err := taskCreateHandler(context.Background(), nil, TaskCreateIn{
		From: "backend", ToKind: "role", ToTarget: "reviewer",
		Subject: "urgent fix needed", Body: "please act now", Intent: "action-requested",
	})
	if err != nil || created.ThreadID == 0 {
		t.Fatalf("create: err=%v out=%+v", err, created)
	}

	raw, err := callDaemon("list_threads", map[string]any{"limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Threads []struct {
			ID     int64  `json:"id"`
			Intent string `json:"intent"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, th := range res.Threads {
		if th.ID == created.ThreadID {
			found = true
			if th.Intent != "action-requested" {
				t.Fatalf("expected intent action-requested, got %q", th.Intent)
			}
		}
	}
	if !found {
		t.Fatalf("thread %d not found in list_threads: %+v", created.ThreadID, res.Threads)
	}
}

func TestTaskTransitionRejectsInvalidStatus(t *testing.T) {
	startTestDaemon(t)
	if _, err := callDaemon("register_agent", map[string]any{
		"alias": "backend", "role": "producer", "model_type": "claude",
	}); err != nil {
		t.Fatal(err)
	}
	_, created, _ := taskCreateHandler(context.Background(), nil, TaskCreateIn{
		From: "backend", ToKind: "role", ToTarget: "reviewer", Subject: "x", Body: "y",
	})
	if _, _, err := taskTransitionHandler(context.Background(), nil, TaskTransitionIn{
		ThreadID: created.ThreadID, By: "rev1", Status: "bogus",
	}); err == nil {
		t.Fatalf("expected error for invalid status")
	}
}
