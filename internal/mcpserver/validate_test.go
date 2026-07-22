package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/tmuxenv"
)

func stubRoster(t *testing.T, rosterJSON string) *[]string {
	t.Helper()
	prev := callDaemon
	t.Cleanup(func() { callDaemon = prev })
	ops := &[]string{}
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		*ops = append(*ops, op)
		if op == "list_agents" {
			return json.RawMessage(rosterJSON), nil
		}
		return json.RawMessage(`{"thread_id":1,"entry_id":1}`), nil
	}
	return ops
}

func TestSendMessageRejectsUnregisteredFrom(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(...string) (string, error) { return "$1", nil }
	t.Cleanup(func() { tmuxenv.Run = prevRun })
	ops := stubRoster(t, `[{"alias":"timewalk-2","socket_path":"/tmp/sock","pane_id":"%14","session_id":"$1","label":"standard 2000","departed":false}]`)

	_, _, err := sendMessageHandler(context.Background(), nil, SendMessageIn{
		From: "timewalk-1998", ToKind: "agent", ToTarget: "timewalk-2", Body: "hi",
	})
	if err == nil {
		t.Fatal("unregistered from must be rejected")
	}
	if !strings.Contains(err.Error(), "timewalk-1998") || !strings.Contains(err.Error(), "'timewalk-2'") {
		t.Fatalf("error must name the bad alias AND the real identity, got %v", err)
	}
	for _, op := range *ops {
		if op == "send_message" {
			t.Fatal("send_message must not reach the daemon for an unregistered from")
		}
	}
}

// TestSendMessageRejectsUnregisteredFromGhostRowDegradesToGenericError covers
// requireRegisteredFrom's identity-resolution fallback under the same
// ghost-guard: a roster row matching the caller's tuple but recorded under a
// DIFFERENT session_created (a leftover from a recycled tmux session ID) must
// not be reported to the model as ITS real identity — that would tell a
// fresh agent "you are already registered as '<ghost>'" for a pane it never
// owned. The rejection must fall back to the generic list_agents-pointing
// error instead.
func TestSendMessageRejectsUnregisteredFromGhostRowDegradesToGenericError(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if args[len(args)-1] == "#{session_created}" {
			return "222", nil // caller's LIVE session incarnation
		}
		return "$1", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prevRun })
	ops := stubRoster(t, `[{"alias":"timewalk-2","socket_path":"/tmp/sock","pane_id":"%14","session_id":"$1","session_created":111,"label":"standard 2000","departed":false}]`)

	_, _, err := sendMessageHandler(context.Background(), nil, SendMessageIn{
		From: "timewalk-1998", ToKind: "agent", ToTarget: "timewalk-2", Body: "hi",
	})
	if err == nil {
		t.Fatal("unregistered from must be rejected")
	}
	if strings.Contains(err.Error(), "this session is registered as") {
		t.Fatalf("a ghost row (stale session_created) must not be reported as this session's real identity, got %v", err)
	}
	if !strings.Contains(err.Error(), "call list_agents") {
		t.Fatalf("expected the generic fallback error, got %v", err)
	}
	for _, op := range *ops {
		if op == "send_message" {
			t.Fatal("send_message must not reach the daemon for an unregistered from")
		}
	}
}

func TestSendMessageAllowsRegisteredAndDepartedFrom(t *testing.T) {
	roster := `[{"alias":"timewalk-2","departed":false},{"alias":"lake-broker","departed":true}]`
	for _, from := range []string{"timewalk-2", "lake-broker"} {
		stubRoster(t, roster)
		_, _, err := sendMessageHandler(context.Background(), nil, SendMessageIn{
			From: from, ToKind: "agent", ToTarget: "timewalk-2", Body: "hi",
		})
		if err != nil {
			t.Fatalf("from=%q must be allowed (departed rows drain mail): %v", from, err)
		}
	}
}

func TestReplyAndTaskCreateRejectUnregisteredFrom(t *testing.T) {
	stubRoster(t, `[]`)
	if _, _, err := replyHandler(context.Background(), nil, ReplyIn{ThreadID: 1, From: "ghost", Body: "x"}); err == nil {
		t.Fatal("reply must reject unregistered from")
	}
	stubRoster(t, `[]`)
	if _, _, err := taskCreateHandler(context.Background(), nil, TaskCreateIn{From: "ghost", ToKind: "agent", ToTarget: "x", Body: "x"}); err == nil {
		t.Fatal("task_create must reject unregistered from")
	}
}
