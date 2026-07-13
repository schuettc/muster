package mcpserver

import (
	"context"
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
