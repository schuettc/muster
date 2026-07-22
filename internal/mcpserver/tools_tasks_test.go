package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
)

func TestTaskCreateClaimTransition(t *testing.T) {
	startTestDaemon(t)
	if _, err := callDaemon("register_agent", map[string]any{
		"alias": "backend", "role": "producer", "model_type": "claude",
	}); err != nil {
		t.Fatal(err)
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
