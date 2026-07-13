package wake

import (
	"reflect"
	"testing"
)

func TestTmuxWakerSendsLiteralThenEnter(t *testing.T) {
	var calls [][]string
	w := TmuxWaker{Run: func(args ...string) error {
		calls = append(calls, args)
		return nil
	}}
	if err := w.Wake("/tmp/tmux-501/proj-x", "%6", "hello there"); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	want := [][]string{
		{"-S", "/tmp/tmux-501/proj-x", "send-keys", "-t", "%6", "-l", "hello there"},
		{"-S", "/tmp/tmux-501/proj-x", "send-keys", "-t", "%6", "Enter"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("unexpected tmux calls:\n got %v\nwant %v", calls, want)
	}
}

func TestTmuxWakerSkipsEnterWhenLiteralFails(t *testing.T) {
	var calls int
	boom := func(_ ...string) error {
		calls++
		if calls == 1 {
			return errPaneGone
		}
		return nil
	}
	w := TmuxWaker{Run: boom}
	if err := w.Wake("/s", "%9", "hi"); err == nil {
		t.Fatalf("expected error when literal send fails")
	}
	if calls != 1 {
		t.Fatalf("Enter should not be sent after a failed literal; calls=%d", calls)
	}
}

var errPaneGone = &wakeErr{"pane gone"}

type wakeErr struct{ s string }

func (e *wakeErr) Error() string { return e.s }
