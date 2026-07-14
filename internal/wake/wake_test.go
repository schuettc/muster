package wake

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTmuxNotifierNotifySetsOptionAndRefreshes(t *testing.T) {
	var calls [][]string
	n := TmuxNotifier{Option: "@claude_attn", Timeout: time.Second, Run: func(_ context.Context, args ...string) error {
		calls = append(calls, args)
		return nil
	}}
	if err := n.Notify("/sock", "$3", 1); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	// First call must set the option on the session, socket-aware. No send-keys anywhere.
	if len(calls) == 0 {
		t.Fatal("no tmux calls")
	}
	set := strings.Join(calls[0], " ")
	if !strings.Contains(set, "-S /sock") || !strings.Contains(set, "set-option") || !strings.Contains(set, "-t $3") || !strings.Contains(set, "@claude_attn 1") {
		t.Fatalf("first call not a socket-aware set-option: %v", calls[0])
	}
	for _, c := range calls {
		if strings.Contains(strings.Join(c, " "), "send-keys") {
			t.Fatalf("Notify must NEVER send-keys, got: %v", c)
		}
	}
}

func TestNotifySetsCountAndUnsetsOnZero(t *testing.T) {
	var cmds [][]string
	n := TmuxNotifier{Option: "@muster_inbox", Run: func(_ context.Context, args ...string) error {
		cmds = append(cmds, args)
		return nil
	}}
	_ = n.Notify("/s", "$1", 3)
	if cmds[0][5] != "@muster_inbox" || cmds[0][6] != "3" {
		t.Fatalf("set cmd = %v", cmds[0])
	}
	cmds = nil
	_ = n.Notify("/s", "$1", 0)
	if joined := strings.Join(cmds[0], " "); !strings.Contains(joined, "-u") || !strings.Contains(joined, "@muster_inbox") {
		t.Fatalf("zero should unset, got %v", cmds[0])
	}
}

func TestTmuxNotifierClearUnsetsOption(t *testing.T) {
	var calls [][]string
	n := TmuxNotifier{Option: "@claude_attn", Timeout: time.Second, Run: func(_ context.Context, args ...string) error {
		calls = append(calls, args)
		return nil
	}}
	if err := n.Clear("/sock", "$3"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	got := strings.Join(calls[0], " ")
	if !strings.Contains(got, "-S /sock") || !strings.Contains(got, "set-option") || !strings.Contains(got, "-u") || !strings.Contains(got, "@claude_attn") || !strings.Contains(got, "-t $3") {
		t.Fatalf("Clear not a socket-aware unset: %v", calls[0])
	}
}
