package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSendMessageAndInbox(t *testing.T) {
	startTestDaemon(t)
	// Register the recipient so role/inbox routing has an agent.
	if _, _, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{
		Alias: "consumer", Role: "consumer", ModelType: "codex",
	}); err != nil {
		t.Fatal(err)
	}

	_, sendOut, err := sendMessageHandler(context.Background(), nil, SendMessageIn{
		From: "backend", ToKind: "agent", ToTarget: "consumer",
		Subject: "heads up", Ref: "repo=bhw", Body: "renamed /bets to /wagers",
	})
	if err != nil || sendOut.ThreadID == 0 {
		t.Fatalf("send: err=%v out=%+v", err, sendOut)
	}

	_, inbox, err := getInboxHandler(context.Background(), nil, GetInboxIn{Alias: "consumer"})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(inbox.Threads) != 1 || inbox.Threads[0].Subject != "heads up" {
		t.Fatalf("unexpected inbox: %+v", inbox.Threads)
	}

	// reply appends an entry; get_thread shows both.
	if _, _, err := replyHandler(context.Background(), nil, ReplyIn{
		ThreadID: sendOut.ThreadID, From: "consumer", Body: "got it",
	}); err != nil {
		t.Fatalf("reply: %v", err)
	}
	_, thr, err := getThreadHandler(context.Background(), nil, GetThreadIn(sendOut))
	if err != nil {
		t.Fatalf("get_thread: %v", err)
	}
	if thr.Thread.ID != sendOut.ThreadID || len(thr.Entries) != 2 {
		t.Fatalf("unexpected thread: %+v entries=%d", thr.Thread, len(thr.Entries))
	}
}

// TestSendMessageIntentPassesThrough proves send_message's optional Intent
// field reaches the daemon and lands on the thread (visible via
// list_threads, the same op the CLI/station read).
func TestSendMessageIntentPassesThrough(t *testing.T) {
	startTestDaemon(t)
	if _, _, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{
		Alias: "consumer", Role: "consumer", ModelType: "codex",
	}); err != nil {
		t.Fatal(err)
	}

	_, sendOut, err := sendMessageHandler(context.Background(), nil, SendMessageIn{
		From: "backend", ToKind: "agent", ToTarget: "consumer",
		Subject: "1.2.2 shipped", Body: "for your info", Intent: "fyi",
	})
	if err != nil || sendOut.ThreadID == 0 {
		t.Fatalf("send: err=%v out=%+v", err, sendOut)
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
		if th.ID == sendOut.ThreadID {
			found = true
			if th.Intent != "fyi" {
				t.Fatalf("expected intent fyi, got %q", th.Intent)
			}
		}
	}
	if !found {
		t.Fatalf("thread %d not found in list_threads: %+v", sendOut.ThreadID, res.Threads)
	}
}
