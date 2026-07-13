package mcpserver

import (
	"context"
	"testing"
)

func TestTaskCreateClaimTransition(t *testing.T) {
	startTestDaemon(t)

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

func TestTaskTransitionRejectsInvalidStatus(t *testing.T) {
	startTestDaemon(t)
	_, created, _ := taskCreateHandler(context.Background(), nil, TaskCreateIn{
		From: "backend", ToKind: "role", ToTarget: "reviewer", Subject: "x", Body: "y",
	})
	if _, _, err := taskTransitionHandler(context.Background(), nil, TaskTransitionIn{
		ThreadID: created.ThreadID, By: "rev1", Status: "bogus",
	}); err == nil {
		t.Fatalf("expected error for invalid status")
	}
}
