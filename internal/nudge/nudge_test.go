package nudge

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func recorder() (*[][]string, func(args ...string) error) {
	var calls [][]string
	return &calls, func(args ...string) error { calls = append(calls, args); return nil }
}

// sleepRecorder returns a Sleep func that records the durations it was asked to
// wait, so tests can assert whether (and how long) a nudge paused before Enter.
func sleepRecorder() (*[]time.Duration, func(time.Duration)) {
	var slept []time.Duration
	return &slept, func(d time.Duration) { slept = append(slept, d) }
}

func joinCalls(calls [][]string) string {
	joined := ""
	for _, c := range calls {
		joined += strings.Join(c, " ") + "\n"
	}
	return joined
}

func TestNudgeClaudeTypesAndSubmitsWithoutDelay(t *testing.T) {
	calls, run := recorder()
	slept, sleep := sleepRecorder()
	n := TmuxNudger{Run: run, Sleep: sleep}
	submitted, err := n.Nudge("/s", "%1", "claude", true)
	if err != nil || !submitted {
		t.Fatalf("claude submit: submitted=%v err=%v", submitted, err)
	}
	joined := joinCalls(*calls)
	if !strings.Contains(joined, "send-keys") || !strings.Contains(joined, "-t %1") || !strings.Contains(joined, "Enter") {
		t.Fatalf("expected send-keys + Enter for claude:\n%s", joined)
	}
	if len(*slept) != 0 {
		t.Fatalf("claude must submit with no delay, slept=%v", *slept)
	}
}

func TestNudgeCodexTypesAndSubmitsAfterDelay(t *testing.T) {
	calls, run := recorder()
	slept, sleep := sleepRecorder()
	n := TmuxNudger{Run: run, Sleep: sleep}
	submitted, err := n.Nudge("/s", "%2", "codex", true)
	if err != nil {
		t.Fatalf("codex: %v", err)
	}
	if !submitted {
		t.Fatalf("codex must now report submitted=true (delayed standalone Enter submits its TUI)")
	}
	// Codex must pause once before pressing Enter.
	if len(*slept) != 1 || (*slept)[0] <= 0 {
		t.Fatalf("codex must sleep exactly once for a positive delay before Enter, slept=%v", *slept)
	}
	// The Enter must be a separate send-keys call issued after the text, not bundled.
	c := *calls
	if len(c) != 2 {
		t.Fatalf("expected 2 tmux calls (text, then Enter), got %d: %v", len(c), c)
	}
	if strings.Contains(strings.Join(c[0], " "), "Enter") {
		t.Fatalf("first call (text) must not contain Enter: %v", c[0])
	}
	if !strings.Contains(strings.Join(c[1], " "), "Enter") {
		t.Fatalf("second call must be the Enter: %v", c[1])
	}
}

func TestNudgeCodexNoSubmitTypesOnly(t *testing.T) {
	calls, run := recorder()
	slept, sleep := sleepRecorder()
	n := TmuxNudger{Run: run, Sleep: sleep}
	submitted, err := n.Nudge("/s", "%2", "codex", false)
	if err != nil {
		t.Fatalf("codex no-submit: %v", err)
	}
	if submitted {
		t.Fatalf("no-submit must report submitted=false")
	}
	if strings.Contains(joinCalls(*calls), "Enter") {
		t.Fatalf("no-submit must not send Enter:\n%s", joinCalls(*calls))
	}
	if len(*slept) != 0 {
		t.Fatalf("no-submit must not delay, slept=%v", *slept)
	}
}

// TestNudgeMessageCarriesDrainAndActInstruction: the typed line must be the
// full drain-and-act instruction (spec §3b), not the old bare "check your
// inbox" — a nudged agent that only lists its inbox and idles must instead be
// told to read each thread, handle it, and reply, autonomously.
func TestNudgeMessageCarriesDrainAndActInstruction(t *testing.T) {
	calls, run := recorder()
	n := TmuxNudger{Run: run}
	if _, err := n.Nudge("/s", "%1", "claude", false); err != nil {
		t.Fatalf("nudge: %v", err)
	}
	joined := joinCalls(*calls)
	for _, want := range []string{"get_inbox", "get_thread", "handle the request", "reply on the thread", "autonomously"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("typed nudge text missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "check your muster inbox (call get_inbox)") {
		t.Fatalf("old bare check-your-inbox wording must be gone:\n%s", joined)
	}
}

func TestNudgeUnknownModelTypedOnly(t *testing.T) {
	calls, run := recorder()
	n := TmuxNudger{Run: run}
	submitted, err := n.Nudge("/s", "%3", "gemini", true)
	if err != nil {
		t.Fatalf("unknown model: %v", err)
	}
	if submitted {
		t.Fatalf("unknown model type must be typed-only (submitted=false)")
	}
	if strings.Contains(joinCalls(*calls), "Enter") {
		t.Fatalf("unknown model must not send Enter (submit behavior unverified):\n%s", joinCalls(*calls))
	}
}

func TestTypeLineTypesCallerTextAndSubmitsForClaude(t *testing.T) {
	var calls [][]string
	n := TmuxNudger{Run: func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}}
	submitted, err := n.TypeLine("/tmp/sock", "%5", "claude", "/rename standard 2000", true)
	if err != nil || !submitted {
		t.Fatalf("TypeLine: submitted=%v err=%v", submitted, err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected type + Enter, got %v", calls)
	}
	want := []string{"-S", "/tmp/sock", "send-keys", "-t", "%5", "-l", "/rename standard 2000"}
	if !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("type call = %v, want %v", calls[0], want)
	}
	if calls[1][len(calls[1])-1] != "Enter" {
		t.Fatalf("expected trailing Enter submit, got %v", calls[1])
	}
}

func TestNudgeStillTypesTheCanonicalMessage(t *testing.T) {
	var calls [][]string
	n := TmuxNudger{Run: func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}}
	if _, err := n.Nudge("/tmp/sock", "%5", "claude", false); err != nil {
		t.Fatal(err)
	}
	if calls[0][len(calls[0])-1] != message {
		t.Fatalf("Nudge must type the canonical message, got %v", calls[0])
	}
}
