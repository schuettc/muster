package station

import (
	"encoding/json"
	"strings"
	"testing"
)

// nudgeCall records one fakeNudger.Nudge invocation.
type nudgeCall struct {
	socketPath, paneID, modelType string
	submit                        bool
}

// fakeNudger is the model-level test double for the nudger seam
// (Options.Nudger) — never shells out to real tmux.
type fakeNudger struct {
	calls     []nudgeCall
	submitted bool
	err       error
}

func (f *fakeNudger) Nudge(socketPath, paneID, modelType string, submit bool) (bool, error) {
	f.calls = append(f.calls, nudgeCall{socketPath, paneID, modelType, submit})
	return f.submitted, f.err
}

// typeString drives m through a sequence of single-rune keyMsgs, as if the
// operator typed s one keystroke at a time.
func typeString(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, ch := range s {
		next, _ := m.Update(keyMsg(string(ch)))
		m = mustModel(t, next)
	}
	return m
}

// TestComposerSendInvokesSendMessageWithIntentAndTarget is the composer's
// core send path (spec §5, brief item 1): 's' opens the roster-filtered
// target picker, typing narrows candidates to a label/alias substring match,
// Enter advances to the body, CycleIntent (tab) advances the F/R/A
// indicator, and the final Enter sends via send_message with the resolved
// target and intent.
func TestComposerSendInvokesSendMessageWithIntentAndTarget(t *testing.T) {
	var calls []map[string]any
	fake := fakeCaller{fn: func(op string, args map[string]any) (json.RawMessage, error) {
		if op == "send_message" {
			calls = append(calls, args)
		}
		return json.RawMessage(`{}`), nil
	}}
	m := NewModel(fake, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "backend-1", Label: "backend"},
		{Alias: "reviewer-1", Label: "review"},
	}})
	m = mustModel(t, next)

	next, _ = m.Update(keyMsg("s"))
	m = mustModel(t, next)
	if m.composer.phase != composerPickingTarget {
		t.Fatalf("phase after 's' = %v, want composerPickingTarget", m.composer.phase)
	}

	m = typeString(t, m, "back")
	if cands := m.composerCandidates(); len(cands) != 1 || cands[0].Alias != "backend-1" {
		t.Fatalf("candidates after filtering 'back' = %+v, want just backend-1", cands)
	}

	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.composer.phase != composerEditingBody || m.composer.target != "backend-1" {
		t.Fatalf("after picking a target: phase=%v target=%q, want composerEditingBody/backend-1", m.composer.phase, m.composer.target)
	}

	m = typeString(t, m, "hello")
	next, _ = m.Update(keyMsg("tab")) // F -> R
	m = mustModel(t, next)
	next, _ = m.Update(keyMsg("tab")) // R -> A
	m = mustModel(t, next)
	if m.composer.intent != intentActionRequested {
		t.Fatalf("intent after two cycles = %v, want action-requested", m.composer.intent)
	}

	next, cmd := m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.composer.phase != composerClosed {
		t.Fatalf("submitting must close the composer immediately")
	}
	if cmd == nil {
		t.Fatalf("submitting must issue the send_message Cmd")
	}
	for _, msg := range flattenCmds(cmd) {
		next, _ = m.Update(msg)
		m = mustModel(t, next)
	}
	if len(calls) != 1 {
		t.Fatalf("send_message calls = %d, want 1: %+v", len(calls), calls)
	}
	got := calls[0]
	if got["from"] != "station" || got["to_target"] != "backend-1" || got["to_kind"] != "agent" ||
		got["body"] != "hello" || got["intent"] != "action-requested" {
		t.Fatalf("send_message args = %+v, want from=station to_target=backend-1 to_kind=agent body=hello intent=action-requested", got)
	}
	if !strings.Contains(m.status, "sent") {
		t.Fatalf("status after a successful send = %q, want it to mention sent", m.status)
	}
}

// TestComposerEscCancelsWithoutSending covers spec §5's "Esc cancels": at
// any composer phase, Esc closes it without invoking any op.
func TestComposerEscCancelsWithoutSending(t *testing.T) {
	var sendCalls int
	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "send_message" {
			sendCalls++
		}
		return json.RawMessage(`{}`), nil
	}}
	m := NewModel(fake, Options{Alias: "station"})
	next, _ := m.Update(keyMsg("s"))
	m = mustModel(t, next)

	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	if m.composer.phase != composerClosed {
		t.Fatalf("Esc from the target picker must close the composer")
	}

	// Esc from the body-editing phase too.
	next, _ = m.Update(keyMsg("s"))
	m = mustModel(t, next)
	next, _ = m.Update(keyMsg("enter")) // no candidates yet -> no-op, stays in picker
	m = mustModel(t, next)
	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	if m.composer.phase != composerClosed {
		t.Fatalf("Esc must close the composer regardless of phase")
	}
	if sendCalls != 0 {
		t.Fatalf("Esc must never invoke send_message, got %d calls", sendCalls)
	}
}

// TestComposerReplyFromThreadView covers spec §5: 'r' in the thread view
// opens the composer as a reply to that thread (no target picker — the
// target is the thread already open), and Enter sends via the reply op.
func TestComposerReplyFromThreadView(t *testing.T) {
	var calls []map[string]any
	fake := fakeCaller{fn: func(op string, args map[string]any) (json.RawMessage, error) {
		switch op {
		case "reply":
			calls = append(calls, args)
		case "get_thread":
			return json.RawMessage(`{"thread":{},"entries":[],"total":0}`), nil
		}
		return json.RawMessage(`{}`), nil
	}}
	m := NewModel(fake, Options{Alias: "station"})
	next, _ := m.Update(threadsMsg{threads: []listThreadRow{{ID: 7, EntryCount: 0}}})
	m = mustModel(t, next)
	m = focusThreads(t, m)

	next, cmd := m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	for _, msg := range flattenCmds(cmd) {
		next, _ = m.Update(msg)
		m = mustModel(t, next)
	}
	if !m.viewOpen {
		t.Fatalf("expected the thread view open before replying")
	}

	next, _ = m.Update(keyMsg("r"))
	m = mustModel(t, next)
	if m.composer.phase != composerEditingBody || m.composer.kind != composerKindReply || m.composer.threadID != 7 {
		t.Fatalf("'r' must open a reply composer targeting thread 7, got %+v", m.composer)
	}

	m = typeString(t, m, "onit")
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	for _, msg := range flattenCmds(cmd) {
		next, _ = m.Update(msg)
		m = mustModel(t, next)
	}
	if len(calls) != 1 {
		t.Fatalf("reply calls = %d, want 1: %+v", len(calls), calls)
	}
	if calls[0]["thread_id"] != int64(7) || calls[0]["from"] != "station" || calls[0]["body"] != "onit" {
		t.Fatalf("reply args = %+v, want thread_id=7 from=station body=onit", calls[0])
	}
	if _, hasIntent := calls[0]["intent"]; hasIntent {
		t.Fatalf("reply must not send an intent arg (the op has none), got %+v", calls[0])
	}
}

// TestNudgeConfirmGateYesInvokesNudgeWithSelfReport covers spec §5's nudge
// confirm gate: 'n' on the roster's selected agent shows "nudge <label>?
// y/n"; confirming with 'y' invokes the SAME sequence cmdNudge does
// (get_agent, TmuxNudger.Nudge, then a best-effort log_event self-report).
func TestNudgeConfirmGateYesInvokesNudgeWithSelfReport(t *testing.T) {
	var logCalls []map[string]any
	fn := &fakeNudger{submitted: true}
	fake := fakeCaller{fn: func(op string, args map[string]any) (json.RawMessage, error) {
		switch op {
		case "get_agent":
			return json.RawMessage(`{"found":true,"agent":{"socket_path":"/s","pane_id":"%1","model_type":"claude"}}`), nil
		case "log_event":
			logCalls = append(logCalls, args)
		}
		return json.RawMessage(`{}`), nil
	}}
	m := NewModel(fake, Options{Nudger: fn})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "backend-1", Label: "backend"}}})
	m = mustModel(t, next)

	next, _ = m.Update(keyMsg("n"))
	m = mustModel(t, next)
	if m.nudgeConfirmAlias != "backend-1" {
		t.Fatalf("nudgeConfirmAlias = %q, want backend-1", m.nudgeConfirmAlias)
	}
	if got := m.renderBottomLine(); !strings.Contains(got, "nudge backend? y/n") {
		t.Fatalf("bottom line = %q, want the y/n confirmation naming the label", got)
	}

	next, cmd := m.Update(keyMsg("y"))
	m = mustModel(t, next)
	if m.nudgeConfirmAlias != "" {
		t.Fatalf("confirming must clear the pending confirmation")
	}
	if cmd == nil {
		t.Fatalf("confirming must issue the nudge Cmd")
	}
	for _, msg := range flattenCmds(cmd) {
		next, _ = m.Update(msg)
		m = mustModel(t, next)
	}
	if len(fn.calls) != 1 {
		t.Fatalf("Nudge calls = %d, want 1: %+v", len(fn.calls), fn.calls)
	}
	if got := fn.calls[0]; got.socketPath != "/s" || got.paneID != "%1" || got.modelType != "claude" || !got.submit {
		t.Fatalf("Nudge call = %+v, want socket=/s pane=%%1 model=claude submit=true", got)
	}
	if len(logCalls) != 1 || logCalls[0]["target"] != "backend-1" || logCalls[0]["detail"] != "submitted" {
		t.Fatalf("log_event self-report = %+v, want target=backend-1 detail=submitted", logCalls)
	}
	if !strings.Contains(m.status, "backend") {
		t.Fatalf("status after nudging = %q, want it to mention the label", m.status)
	}
}

// TestNudgeConfirmGateOtherKeyCancelsWithoutNudging covers the decline path:
// any key other than 'y' (Esc included) cancels without ever calling
// get_agent or Nudge.
func TestNudgeConfirmGateOtherKeyCancelsWithoutNudging(t *testing.T) {
	fn := &fakeNudger{}
	var getAgentCalls int
	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "get_agent" {
			getAgentCalls++
		}
		return json.RawMessage(`{}`), nil
	}}
	m := NewModel(fake, Options{Nudger: fn})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "backend-1", Label: "backend"}}})
	m = mustModel(t, next)

	next, _ = m.Update(keyMsg("n"))
	m = mustModel(t, next)

	next, cmd := m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	if m.nudgeConfirmAlias != "" {
		t.Fatalf("esc must clear the pending confirmation")
	}
	if cmd != nil {
		t.Fatalf("declining must not issue a Cmd")
	}
	if getAgentCalls != 0 || len(fn.calls) != 0 {
		t.Fatalf("declining must never call get_agent or Nudge, got %d get_agent calls and %d Nudge calls", getAgentCalls, len(fn.calls))
	}
}

// TestFilterHidesNonMatchingRosterRows covers spec §5's '/': a substring
// filter over the focused pane's rendered row text, live as the operator
// types, applied until Esc clears it (re-opening the SAME pane's filter for
// editing preserves the existing query).
func TestFilterHidesNonMatchingRosterRows(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "backend-1", Project: "muster", Label: "backend"},
		{Alias: "reviewer-1", Project: "muster", Label: "review"},
	}})
	m = mustModel(t, next)

	next, _ = m.Update(keyMsg("/"))
	m = mustModel(t, next)
	if !m.filter.editing || m.filter.pane != paneRoster {
		t.Fatalf("expected filter editing on paneRoster, got %+v", m.filter)
	}
	m = typeString(t, m, "back")

	view := m.View()
	if !strings.Contains(view, "backend") {
		t.Fatalf("filtered view missing the matching row:\n%s", view)
	}
	if strings.Contains(view, "review") {
		t.Fatalf("filtered view still shows the non-matching row:\n%s", view)
	}

	next, _ = m.Update(keyMsg("enter")) // stop editing, filter stays applied
	m = mustModel(t, next)
	if m.filter.editing {
		t.Fatalf("enter must stop editing")
	}
	if m.filter.query != "back" {
		t.Fatalf("query = %q, want it to stay applied after enter", m.filter.query)
	}

	next, _ = m.Update(keyMsg("/")) // re-open editing on the SAME pane
	m = mustModel(t, next)
	if m.filter.input.Value() != "back" {
		t.Fatalf("re-opening the same pane's filter must preserve the existing query, got %q", m.filter.input.Value())
	}
	next, _ = m.Update(keyMsg("esc")) // clear entirely
	m = mustModel(t, next)
	if m.filter.query != "" || m.filter.editing {
		t.Fatalf("esc while editing must clear the filter entirely, got %+v", m.filter)
	}
	view = m.View()
	if !strings.Contains(view, "review") {
		t.Fatalf("clearing the filter must show every row again:\n%s", view)
	}
}

// TestAliasesToggleSwitchesDisplay covers spec §5's 'a': toggling flips
// Options.Aliases and re-renders both the roster (via dispLabel) and the
// feed (via the shared render.Renderer).
func TestAliasesToggleSwitchesDisplay(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "backend-1", Project: "p", Label: "backend"}}})
	m = mustModel(t, next)
	if strings.Contains(m.View(), "backend-1") {
		t.Fatalf("default view should show the label, not the raw alias:\n%s", m.View())
	}

	next, _ = m.Update(keyMsg("a"))
	m = mustModel(t, next)
	if !m.opts.Aliases {
		t.Fatalf("'a' must toggle Aliases on")
	}
	if view := m.View(); !strings.Contains(view, "backend-1") {
		t.Fatalf("after toggling aliases on, the raw alias must show:\n%s", view)
	}
}
