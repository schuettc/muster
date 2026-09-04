package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

func TestSendMessageAndInbox(t *testing.T) {
	startTestDaemon(t)
	// Register both sender and recipient.
	if _, err := callDaemon("register_agent", map[string]any{
		"alias": "backend", "role": "producer", "model_type": "claude",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callDaemon("register_agent", map[string]any{
		"alias": "consumer", "role": "consumer", "model_type": "codex",
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

// TestSendStandingBroadcastSurfacesStandingOnRead: a standing broadcast is
// accepted via the MCP tool, and get_thread reports Standing=true so an agent
// can tell a standing order from a transient one.
func TestSendStandingBroadcastSurfacesStandingOnRead(t *testing.T) {
	startTestDaemon(t)
	if _, err := callDaemon("register_agent", map[string]any{
		"alias": "backend", "model_type": "claude",
	}); err != nil {
		t.Fatal(err)
	}
	_, sendOut, err := sendMessageHandler(context.Background(), nil, SendMessageIn{
		From: "backend", ToKind: "broadcast", Standing: true,
		Subject: "order", Body: "read CONTRACT.md before editing", Confirm: true,
	})
	if err != nil || sendOut.ThreadID == 0 {
		t.Fatalf("standing broadcast: err=%v out=%+v", err, sendOut)
	}
	_, thr, err := getThreadHandler(context.Background(), nil, GetThreadIn(sendOut))
	if err != nil {
		t.Fatalf("get_thread: %v", err)
	}
	if !thr.Thread.Standing {
		t.Fatalf("ThreadView should surface Standing=true, got %+v", thr.Thread)
	}
}

// TestSendStandingRejectedOnDirectedMessage: standing is broadcast-only, and
// the MCP tool surfaces the daemon's rejection.
func TestSendStandingRejectedOnDirectedMessage(t *testing.T) {
	startTestDaemon(t)
	if _, err := callDaemon("register_agent", map[string]any{"alias": "backend", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callDaemon("register_agent", map[string]any{"alias": "consumer", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := sendMessageHandler(context.Background(), nil, SendMessageIn{
		From: "backend", ToKind: "agent", ToTarget: "consumer", Standing: true, Body: "x",
	})
	if err == nil {
		t.Fatal("standing on a directed message must be rejected")
	}
}

// TestGetInboxSendsCallerProof covers the regression this task closes: an
// MCP get_inbox must send the caller's tmux/harness proof so the daemon's
// callerOwns check (spec 2026-08-21 §3.2, daemon commit 7f17f35) can move the
// alias's read watermark for a caller who actually owns it — without proof,
// every MCP read is a harmless peek that never clears an agent's own badge.
// There is deliberately no caller_pane_id: ownership is session-granular
// (daemon commit 5a79d0e), not pane-granular.
func TestGetInboxSendsCallerProof(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%6")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "harness-uuid-1")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if args[len(args)-1] == "#{session_id}" {
			return "$1", nil
		}
		if args[len(args)-1] == "#{session_created}" {
			return "500", nil
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var got map[string]any
	prevDaemon := callDaemon
	callDaemon = func(_ string, args map[string]any) (json.RawMessage, error) {
		got = args
		return json.RawMessage(`{"threads":[],"marked_read":false}`), nil
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, out, err := getInboxHandler(context.Background(), nil, GetInboxIn{Alias: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	if got["caller_socket_path"] != "/tmp/sock" || got["caller_session_id"] != "$1" ||
		got["caller_session_created"] != int64(500) || got["caller_harness_session_id"] != "harness-uuid-1" {
		t.Fatalf("caller proof missing/wrong in args: %+v", got)
	}
	if _, present := got["caller_pane_id"]; present {
		t.Fatalf("caller_pane_id must never be sent — ownership is session-granular, got %+v", got)
	}
	if out.Detail != "peek only — 'backend' is not this session's; its unread state is unchanged" {
		t.Fatalf("expected the peek notice, got %q", out.Detail)
	}
}

// TestGetInboxOwnedNoPeekNotice covers the other half: when the daemon
// reports marked_read=true (the caller proved ownership), no peek notice
// should be appended.
func TestGetInboxOwnedNoPeekNotice(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%6")
	prev := tmuxenv.Run
	tmuxenv.Run = func(_ ...string) (string, error) { return "$1", nil }
	t.Cleanup(func() { tmuxenv.Run = prev })

	prevDaemon := callDaemon
	callDaemon = func(_ string, _ map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"threads":[],"marked_read":true}`), nil
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, out, err := getInboxHandler(context.Background(), nil, GetInboxIn{Alias: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Detail != "" {
		t.Fatalf("expected no peek notice on an owned read, got %q", out.Detail)
	}
}

// TestSendMessageIntentPassesThrough proves send_message's optional Intent
// field reaches the daemon and lands on the thread (visible via
// list_threads, the same op the CLI/station read).
func TestSendMessageIntentPassesThrough(t *testing.T) {
	startTestDaemon(t)
	if _, err := callDaemon("register_agent", map[string]any{
		"alias": "backend", "role": "producer", "model_type": "claude",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callDaemon("register_agent", map[string]any{
		"alias": "consumer", "role": "consumer", "model_type": "codex",
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

// TestGetInboxCarriesLastFromAndUnread proves ThreadView's new wire fields
// (last_from, last_at, entry_count, unread) actually reach the MCP tool
// output — the fix for the production defect where get_inbox exposed only
// thread metadata and an agent couldn't distinguish a peer's reply from its
// own last send without a get_thread round trip.
func TestGetInboxCarriesLastFromAndUnread(t *testing.T) {
	startTestDaemon(t)
	if _, err := callDaemon("register_agent", map[string]any{
		"alias": "web", "role": "producer", "model_type": "claude",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := callDaemon("register_agent", map[string]any{
		"alias": "api", "role": "consumer", "model_type": "claude",
	}); err != nil {
		t.Fatal(err)
	}

	_, sendOut, err := sendMessageHandler(context.Background(), nil, SendMessageIn{
		From: "web", ToKind: "agent", ToTarget: "api", Subject: "status?", Body: "how's it going",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := replyHandler(context.Background(), nil, ReplyIn{
		ThreadID: sendOut.ThreadID, From: "api", Body: "all good",
	}); err != nil {
		t.Fatal(err)
	}

	_, inbox, err := getInboxHandler(context.Background(), nil, GetInboxIn{Alias: "web"})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	var got ThreadView
	found := false
	for _, th := range inbox.Threads {
		if th.ID == sendOut.ThreadID {
			got = th
			found = true
		}
	}
	if !found {
		t.Fatalf("thread %d not found in inbox: %+v", sendOut.ThreadID, inbox.Threads)
	}
	if got.LastFrom != "api" {
		t.Fatalf("last_from = %q, want %q", got.LastFrom, "api")
	}
	if got.EntryCount != 2 {
		t.Fatalf("entry_count = %d, want 2", got.EntryCount)
	}
	if got.Unread != 1 {
		t.Fatalf("unread = %d, want 1 (peer reply on a thread web originated must be visible)", got.Unread)
	}
	if got.LastAt == 0 {
		t.Fatalf("last_at = 0, want nonzero")
	}
}
