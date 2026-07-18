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

// tabToAgentStrip asserts m is already parked on screenProject's L1 agents
// list — the default landing spot on every L0→L1 drill / single-project
// auto-skip. Kept under its old name so every call site below is unchanged.
func tabToAgentStrip(t *testing.T, m Model) Model {
	t.Helper()
	if m.screen != screenProject || m.l1IsOrphaned() {
		t.Fatalf("expected screenProject on the agents list (the default landing spot), got screen=%v project=%q", m.screen, m.project)
	}
	return m
}

// TestComposerSendInvokesSendMessageWithIntentAndTarget is the composer's
// core send path (spec §5-REVISED: "s send global from anywhere with
// picker"): 's' opens the roster-filtered target picker, typing narrows
// candidates to a label/alias substring match, Enter advances to the body,
// CycleIntent (tab) advances the F/R/A indicator, and the final Enter sends
// via send_message with the resolved target and intent.
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

// TestComposerReplyFromFocusedConversation covers spec §5-REVISED: 'r' is
// valid only while a conversation is FOCUSED (focusConvRight, L2) — no
// target picker, the target is the thread that's currently selected/open —
// spec §5-LOCK: 'r' works directly from a thread-LIST row (the agent page's
// own threads table), not only once actually reading one — and Enter sends
// via the reply op.
func TestComposerReplyFromFocusedConversation(t *testing.T) {
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
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "agent-1"}}}) // one default-project agent: auto-skips to screenProject
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{{ID: 7, FromAgent: "agent-1", EntryCount: 0}}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd) // the auto-selected conversation's preview fetch
	if m.screen != screenProject || m.agent != "agent-1" {
		t.Fatalf("setup: expected screenProject/agent-1, got screen=%v agent=%q", m.screen, m.agent)
	}

	// 'r' at screenProject (no thread selection meaningful here) is a no-op.
	next, _ = m.Update(keyMsg("r"))
	m = mustModel(t, next)
	if m.composer.phase != composerClosed {
		t.Fatalf("'r' at screenProject must be a no-op, got composer phase %v", m.composer.phase)
	}

	next, _ = m.Update(keyMsg("enter")) // descend into agent-1's own thread list
	m = mustModel(t, next)
	if m.screen != screenAgent || m.conversation != 7 {
		t.Fatalf("setup: expected screenAgent/conversation=7, got screen=%v conv=%d", m.screen, m.conversation)
	}

	// 'r' directly from the threads-table row (not yet reading it) must open
	// a reply composer targeting the selected thread.
	next, _ = m.Update(keyMsg("r"))
	m = mustModel(t, next)
	if m.composer.phase != composerEditingBody || m.composer.kind != composerKindReply || m.composer.threadID != 7 {
		t.Fatalf("'r' from the threads list must open a reply composer targeting thread 7, got %+v", m.composer)
	}
	next, _ = m.Update(keyMsg("esc")) // cancel; re-open reply after actually reading it below
	m = mustModel(t, next)

	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	for _, msg := range flattenCmds(cmd) {
		next, _ = m.Update(msg)
		m = mustModel(t, next)
	}
	if m.screen != screenRead {
		t.Fatalf("expected the thread focused for reading before replying")
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

// TestNudgeConfirmGateYesInvokesNudgeWithSelfReport covers spec §5-REVISED's
// nudge confirm gate: 'n' on the agent strip's selected agent shows "nudge
// <label>? y/n"; confirming with 'y' invokes the SAME sequence cmdNudge does
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
	// Single default-project bus auto-skips to screenProject/focusProjectItems
	// on the AGENTS section (spec iteration-4/6: "agents first" / "n nudge on
	// AGENTS section/page").
	m = tabToAgentStrip(t, m)

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

// TestNudgeOnAgentPageNudgesItsOwnAgent covers spec §5-REVISED's other nudge
// entry point: on screenAgent (L1.5), 'n' nudges that page's own agent
// regardless of its sub-focus.
func TestNudgeOnAgentPageNudgesItsOwnAgent(t *testing.T) {
	fn := &fakeNudger{submitted: true}
	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "get_agent" {
			return json.RawMessage(`{"found":true,"agent":{"socket_path":"/s","pane_id":"%1","model_type":"claude"}}`), nil
		}
		return json.RawMessage(`{}`), nil
	}}
	m := NewModel(fake, Options{Nudger: fn})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "backend-1", Label: "backend"}}})
	m = mustModel(t, next)
	m = tabToAgentStrip(t, m)
	next, _ = m.Update(keyMsg("enter")) // drill into backend-1's agent page
	m = mustModel(t, next)
	if m.screen != screenAgent || m.agent != "backend-1" {
		t.Fatalf("setup: expected screenAgent for backend-1, got screen=%v agent=%q", m.screen, m.agent)
	}

	next, _ = m.Update(keyMsg("n"))
	m = mustModel(t, next)
	if m.nudgeConfirmAlias != "backend-1" {
		t.Fatalf("nudging from the agent page must target its own agent, got %q", m.nudgeConfirmAlias)
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
	m = tabToAgentStrip(t, m)

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

// TestFilterHidesNonMatchingAgentStripRows covers spec §5-REVISED's '/': a
// substring filter over the current left list's rendered row text, live as
// the operator types, applied until Esc clears it.
func TestFilterHidesNonMatchingAgentStripRows(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "backend-1", Label: "backend"},
		{Alias: "reviewer-1", Label: "review"},
	}})
	m = mustModel(t, next)
	m = tabToAgentStrip(t, m)

	next, _ = m.Update(keyMsg("/"))
	m = mustModel(t, next)
	if !m.filter.editing || m.filter.list != llProjectItems {
		t.Fatalf("expected filter editing on llProjectItems, got %+v", m.filter)
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

	next, _ = m.Update(keyMsg("/")) // re-open editing on the SAME list
	m = mustModel(t, next)
	if m.filter.input.Value() != "back" {
		t.Fatalf("re-opening the same list's filter must preserve the existing query, got %q", m.filter.input.Value())
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
// Options.Aliases and re-renders both the agent strip (via dispLabel) and
// the activity feed (via the shared render.Renderer).
func TestAliasesToggleSwitchesDisplay(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	// Live: true — a departed agent's row always shows its real alias
	// regardless of the aliases toggle (spec §5-LOCK decision A), so this
	// needs a LIVE row to exercise dispLabel's own toggle behavior.
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "backend-1", Label: "backend", Live: true}}})
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

// TestSelfNudgeGuardShowsStatusNoteAndNeverConfirms covers a carried-over
// fix: station's own row appears in its own project's agent strip (it
// registers itself like any other agent), and nudging it would tmux
// send-keys INTO station's own pane — whose quit key is literally 'q',
// which the nudge text contains. Pressing 'n' with that row selected must
// never enter the confirm state, and must explain why on the status line.
func TestSelfNudgeGuardShowsStatusNoteAndNeverConfirms(t *testing.T) {
	var getAgentCalls int
	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "get_agent" {
			getAgentCalls++
		}
		return json.RawMessage(`{}`), nil
	}}
	fn := &fakeNudger{}
	m := NewModel(fake, Options{Alias: "station", Nudger: fn})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "backend-1", Label: "backend"},
		{Alias: "station", Label: "me"},
	}})
	m = mustModel(t, next)
	m = tabToAgentStrip(t, m)

	// agentStripRows sorts by alias: "backend-1" < "station", so index 0 is
	// backend-1 and one 'j' reaches station's own row.
	next, _ = m.Update(keyMsg("j"))
	m = mustModel(t, next)
	if m.agent != "station" {
		t.Fatalf("expected the cursor on station's own row, got %q", m.agent)
	}

	next, _ = m.Update(keyMsg("n"))
	m = mustModel(t, next)
	if m.nudgeConfirmAlias != "" {
		t.Fatalf("self-nudge must never enter the confirm state, got %q", m.nudgeConfirmAlias)
	}
	if !strings.Contains(m.status, "that's you") {
		t.Fatalf("status = %q, want it to explain the self-nudge guard", m.status)
	}

	next, cmd := m.Update(keyMsg("y"))
	m = mustModel(t, next)
	if cmd != nil {
		t.Fatalf("'y' after a self-nudge guard must not issue a Cmd")
	}
	if getAgentCalls != 0 || len(fn.calls) != 0 {
		t.Fatalf("self-nudge guard must never call get_agent or Nudge, got %d/%d calls", getAgentCalls, len(fn.calls))
	}
}

// TestFilterHidesSelectedAgentNudgeIsNoOp covers the filter/selection desync
// carried-over fix on the agent strip: when the '/' filter hides the row
// m.agent currently points at, 'n' must not confirm a nudge against an
// invisible row — it only corrects the selection so a follow-up 'n' acts on
// what the operator can actually see.
func TestFilterHidesSelectedAgentNudgeIsNoOp(t *testing.T) {
	var getAgentCalls int
	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "get_agent" {
			getAgentCalls++
		}
		return json.RawMessage(`{}`), nil
	}}
	m := NewModel(fake, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "backend-1", Label: "backend"},
		{Alias: "reviewer-1", Label: "review"},
	}})
	m = mustModel(t, next)
	m = tabToAgentStrip(t, m)
	if m.agent != "backend-1" {
		t.Fatalf("expected initial selection on backend-1, got %q", m.agent)
	}

	next, _ = m.Update(keyMsg("/"))
	m = mustModel(t, next)
	m = typeString(t, m, "review")
	next, _ = m.Update(keyMsg("enter")) // stop editing; filter (hides backend-1) stays applied
	m = mustModel(t, next)

	next, _ = m.Update(keyMsg("n"))
	m = mustModel(t, next)
	if m.nudgeConfirmAlias != "" {
		t.Fatalf("n on a filtered-out selection must not enter the confirm state, got %q", m.nudgeConfirmAlias)
	}
	if m.agent != "reviewer-1" {
		t.Fatalf("selection must snap to the first visible agent (reviewer-1), got %q", m.agent)
	}
	if getAgentCalls != 0 {
		t.Fatalf("n must never call get_agent when it only corrects a filtered-out selection, got %d calls", getAgentCalls)
	}
}

// TestFilterAgentStripJKSkipsHiddenRows covers the filter/selection desync
// carried-over fix: j/k must walk only rows visible under the agent strip's
// active '/' filter, never landing the cursor on a hidden one.
func TestFilterAgentStripJKSkipsHiddenRows(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "alpha-odd"}, {Alias: "beta-even"}, {Alias: "gamma-odd"},
	}})
	m = mustModel(t, next)
	m = tabToAgentStrip(t, m)

	next, _ = m.Update(keyMsg("/"))
	m = mustModel(t, next)
	m = typeString(t, m, "odd")
	next, _ = m.Update(keyMsg("enter")) // stop editing; filter (hides beta-even) stays applied
	m = mustModel(t, next)

	next, _ = m.Update(keyMsg("j"))
	m = mustModel(t, next)
	if m.agent != "gamma-odd" {
		t.Fatalf("j must skip hidden beta-even and land on gamma-odd, got %q", m.agent)
	}
	next, _ = m.Update(keyMsg("j")) // already at the last visible row: must clamp, not wrap
	m = mustModel(t, next)
	if m.agent != "gamma-odd" {
		t.Fatalf("j at the last visible row must clamp, got %q", m.agent)
	}
	next, _ = m.Update(keyMsg("k"))
	m = mustModel(t, next)
	if m.agent != "alpha-odd" {
		t.Fatalf("k must skip back over hidden beta-even to alpha-odd, got %q", m.agent)
	}
}

// TestClearingFilterRestoresNavigationWithSelectionIntact covers the
// filter/selection desync carried-over fix: clearing the '/' filter must
// restore normal j/k navigation over every row again, and must not disturb
// a selection that's still present.
func TestClearingFilterRestoresNavigationWithSelectionIntact(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "alpha-odd"}, {Alias: "beta-even"}, {Alias: "gamma-odd"},
	}})
	m = mustModel(t, next)
	m = tabToAgentStrip(t, m)

	next, _ = m.Update(keyMsg("/"))
	m = mustModel(t, next)
	m = typeString(t, m, "odd")
	next, _ = m.Update(keyMsg("enter")) // stop editing; filter (hides beta-even) stays applied
	m = mustModel(t, next)

	next, _ = m.Update(keyMsg("j")) // alpha-odd -> gamma-odd, skipping hidden beta-even
	m = mustModel(t, next)
	if m.agent != "gamma-odd" {
		t.Fatalf("setup: expected gamma-odd selected, got %q", m.agent)
	}

	next, _ = m.Update(keyMsg("/"))
	m = mustModel(t, next)
	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	if m.filter.query != "" || m.filter.editing {
		t.Fatalf("esc must clear the filter entirely, got %+v", m.filter)
	}
	if m.agent != "gamma-odd" {
		t.Fatalf("clearing the filter must keep the current selection intact, got %q", m.agent)
	}

	next, _ = m.Update(keyMsg("k"))
	m = mustModel(t, next)
	if m.agent != "beta-even" {
		t.Fatalf("k after clearing the filter must reach beta-even, got %q", m.agent)
	}
}

// TestFilterHidesSelectedConversationEnterIsNoOp covers the filter/selection
// desync carried-over fix on the conversation list: when the '/' filter
// hides the currently selected conversation, Enter must not focus it (and
// must not fire the open-to-acknowledge get_inbox side effect on a
// conversation the operator never saw) — it only snaps the selection to the
// first visible one instead.
func TestFilterHidesSelectedConversationEnterIsNoOp(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "agent-1"}}}) // auto-skip to screenProject
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "agent-1", Subject: "keep-hidden"},
		{ID: 2, FromAgent: "agent-1", Subject: "keep-visible"},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.conversation != 1 {
		t.Fatalf("setup: expected conversation 1 selected, got %d", m.conversation)
	}
	next, _ = m.Update(keyMsg("enter")) // descend into agent-1's own thread list, so '/' below filters conversations
	m = mustModel(t, next)
	if m.screen != screenAgent {
		t.Fatalf("setup: expected screenAgent, got %v", m.screen)
	}

	next, _ = m.Update(keyMsg("/"))
	m = mustModel(t, next)
	m = typeString(t, m, "visible")
	next, _ = m.Update(keyMsg("enter")) // stop editing; filter (hides conversation 1) stays applied
	m = mustModel(t, next)
	if m.filter.editing {
		t.Fatalf("setup: expected filter editing stopped")
	}

	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if cmd != nil {
		t.Fatalf("Enter on a filtered-out selection must be a no-op (no ack), got a Cmd")
	}
	if m.screen == screenRead {
		t.Fatalf("Enter on a filtered-out selection must not focus the conversation")
	}
	if m.conversation != 2 {
		t.Fatalf("selection must snap to the first visible conversation (2), got %d", m.conversation)
	}

	// A follow-up Enter now lands on a visible, snapped selection and
	// focuses normally.
	next, _ = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	if m.screen != screenRead {
		t.Fatalf("Enter on the now-visible snapped selection must focus the conversation")
	}
}

// TestFilterConversationsJKSkipsHiddenRows covers the filter/selection
// desync carried-over fix on the conversation list: j/k must walk only rows
// visible under the active '/' filter.
func TestFilterConversationsJKSkipsHiddenRows(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "agent-1"}}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "agent-1", Subject: "uno-zed"},
		{ID: 2, FromAgent: "agent-1", Subject: "dos-why"},
		{ID: 3, FromAgent: "agent-1", Subject: "tres-zed"},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.conversation != 1 {
		t.Fatalf("setup: expected conversation 1 selected, got %d", m.conversation)
	}
	next, _ = m.Update(keyMsg("enter")) // descend into agent-1's own thread list, so j/k below move the conversation selection
	m = mustModel(t, next)
	if m.screen != screenAgent {
		t.Fatalf("setup: expected screenAgent, got %v", m.screen)
	}

	next, _ = m.Update(keyMsg("/"))
	m = mustModel(t, next)
	m = typeString(t, m, "zed")
	next, _ = m.Update(keyMsg("enter")) // stop editing; filter (hides conversation 2) stays applied
	m = mustModel(t, next)

	next, cmd = m.Update(keyMsg("j"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.conversation != 3 {
		t.Fatalf("j must skip hidden conversation 2 and land on 3, got %d", m.conversation)
	}
	next, cmd = m.Update(keyMsg("j")) // already at the last visible row: must clamp, not wrap
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.conversation != 3 {
		t.Fatalf("j at the last visible row must clamp, got %d", m.conversation)
	}
	next, cmd = m.Update(keyMsg("k"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.conversation != 1 {
		t.Fatalf("k must skip back over hidden conversation 2 to 1, got %d", m.conversation)
	}
}

// TestComposerPickerDisambiguatesSameLabelByProject covers spec §5-LOCK item
// 7: two candidates sharing the same display label are ambiguous in the
// picker's plain label list, so dispLabel's OWN collision handling — the ONE
// shared helper used wherever labels render — appends each one's alias.
func TestComposerPickerDisambiguatesSameLabelByProject(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "backend-1", Project: "muster", Label: "backend"},
		{Alias: "backend-2", Project: "otherproj", Label: "backend"},
	}})
	m = mustModel(t, next)

	next, _ = m.Update(keyMsg("s"))
	m = mustModel(t, next)

	view := m.renderComposerPicker()
	want := "[>backend (backend-1) backend (backend-2)]"
	if !strings.Contains(view, want) {
		t.Fatalf("picker = %q, want it to contain %q (alias-disambiguated labels)", view, want)
	}
}
