package station

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// This file covers spec §5-LOCK's L1/L2 content deltas: the AGENTS list's
// live/quit split with a plain divider bar (decision A: no "departed" label
// word anywhere), label-collision alias disambiguation (item 7), the L2
// preview clearing when its thread list goes empty (screen 4), and the
// agent-page header band's marked-but-empty 0.6.1 vitals slot (decision C).

// TestAgentsListHasDividerBarAndQuitMarkerNoDepartedWord covers spec §5-LOCK
// screen 3: live agents render first (● green), then — once there's at
// least one quit agent — a plain divider bar with NO label text, then quit
// agents by their real alias, dimmed, marked ✗. The word "departed" must
// never appear anywhere in the rendered view.
func TestAgentsListHasDividerBarAndQuitMarkerNoDepartedWord(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "live-one", Project: "p", Live: true},
		{Alias: "gone-agent", Project: "p", Live: false},
	}})
	m = mustModel(t, next)
	if m.screen != screenProject {
		t.Fatalf("setup: expected screenProject, got %v", m.screen)
	}

	view := m.View()
	if strings.Contains(strings.ToLower(view), "departed") {
		t.Fatalf("view must never contain the word \"departed\":\n%s", view)
	}
	if !strings.Contains(view, "●") {
		t.Fatalf("expected the live dot on the live agent:\n%s", view)
	}
	if !strings.Contains(view, "✗") {
		t.Fatalf("expected the ✗ marker on the quit agent:\n%s", view)
	}
	if !strings.Contains(view, "gone-agent") {
		t.Fatalf("expected the quit agent's REAL alias listed:\n%s", view)
	}

	box := m.renderAgentsBox(m.layout().leftW, m.layout().convListH)
	lines := strings.Split(box, "\n")
	liveLine, dividerLine, quitLine := -1, -1, -1
	for i, l := range lines {
		switch {
		case strings.Contains(l, "live-one") && liveLine < 0:
			liveLine = i
		case strings.Contains(l, "gone-agent") && quitLine < 0:
			quitLine = i
		case i > 0 && i < len(lines)-1 && dividerLine < 0 && strings.Contains(strings.TrimSpace(l), "──────"):
			dividerLine = i
		}
	}
	if dividerLine < 0 {
		t.Fatalf("expected a plain divider rule (an interior line of dashes) between live and quit agents:\n%s", box)
	}

	// The live agent's row must precede the divider; the quit agent's row
	// must follow it — live on top, quit below (spec §5-LOCK screen 3).
	if liveLine < 0 || dividerLine <= liveLine || quitLine <= dividerLine {
		t.Fatalf("expected order live-one -> divider -> gone-agent, got liveLine=%d dividerLine=%d quitLine=%d\n%s", liveLine, dividerLine, quitLine, box)
	}
}

// TestLabelCollisionAppendsAlias covers spec §5-LOCK item 7: when an
// agent's display label collides with ANOTHER agent's label, dispLabel
// appends its alias so WHO can never read like nonsense — checked both via
// the shared helper directly and in a rendered thread row.
func TestLabelCollisionAppendsAlias(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "reviewer-1", Project: "p", Label: "reviewer"},
		{Alias: "reviewer-2", Project: "p", Label: "reviewer"}, // SAME label: collision
		{Alias: "unique-1", Project: "p", Label: "unique"},     // no collision
	}})
	m = mustModel(t, next)

	if got := m.dispLabel("reviewer-1"); got != "reviewer (reviewer-1)" {
		t.Fatalf("dispLabel(reviewer-1) = %q, want the alias appended for a colliding label", got)
	}
	if got := m.dispLabel("reviewer-2"); got != "reviewer (reviewer-2)" {
		t.Fatalf("dispLabel(reviewer-2) = %q, want the alias appended for a colliding label", got)
	}
	if got := m.dispLabel("unique-1"); got != "unique" {
		t.Fatalf("dispLabel(unique-1) = %q, want the plain label (no collision)", got)
	}

	// A thread row's WHO column carries the disambiguated form too.
	row := conversationRow{listThreadRow: listThreadRow{ID: 1, FromAgent: "reviewer-1", ToKind: "agent", ToTarget: "unique-1"}}
	line := m.renderConversationLine(row, 200, m.threadWhoContentWidth([]conversationRow{row}))
	if !strings.Contains(line, "reviewer (reviewer-1)") {
		t.Fatalf("WHO column must disambiguate a colliding label with its alias, got %q", line)
	}
}

// TestLabelEqualsAnotherAliasAlsoCollides covers item 7's second collision
// rule: a label that happens to equal a DIFFERENT agent's real alias is
// just as ambiguous as two identical labels, so it also gets disambiguated.
func TestLabelEqualsAnotherAliasAlsoCollides(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "nfl-research-agent", Project: "p"},                    // no label: displays as its own alias
		{Alias: "muster-2", Project: "p", Label: "nfl-research-agent"}, // label EQUALS the other agent's alias
	}})
	m = mustModel(t, next)

	if got := m.dispLabel("muster-2"); got != "nfl-research-agent (muster-2)" {
		t.Fatalf("dispLabel(muster-2) = %q, want its alias appended (label collides with another agent's alias)", got)
	}
}

// TestSelfSendRendersToSelfNotArrow covers the self-send rendering fix: when
// a thread's from and to resolve to the SAME agent alias, WHO must render
// "<name> · to self" rather than a literal "x→x", which reads like a
// duplicate-send bug rather than an agent messaging itself. Checked in both
// the columnized THREADS table (renderConversationLine) and the plain
// filter/preview text (renderThreadRow) — the two call sites renderWho
// shares. A label collision between two DIFFERENT agents must never be
// mistaken for a self-send (identity, not display text, decides).
func TestSelfSendRendersToSelfNotArrow(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "solo-1", Project: "p", Label: "solo"},
		{Alias: "twin-a", Project: "p", Label: "shared"},
		{Alias: "twin-b", Project: "p", Label: "shared"},
	}})
	m = mustModel(t, next)

	selfRow := listThreadRow{ID: 1, FromAgent: "solo-1", ToKind: "agent", ToTarget: "solo-1"}
	if !threadIsSelfSend(selfRow) {
		t.Fatalf("threadIsSelfSend must report true for from==to, got false")
	}
	if got := m.renderWho(selfRow, "→"); got != "solo · to self" {
		t.Fatalf("renderWho(self-send) = %q, want %q", got, "solo · to self")
	}

	// Columnized THREADS table.
	selfConvRow := conversationRow{listThreadRow: selfRow}
	convLine := m.renderConversationLine(selfConvRow, 200, m.threadWhoContentWidth([]conversationRow{selfConvRow}))
	if !strings.Contains(convLine, "solo · to self") {
		t.Fatalf("THREADS table WHO must render 'solo · to self', got %q", convLine)
	}
	if strings.Contains(convLine, "solo→solo") {
		t.Fatalf("THREADS table WHO must never render a literal 'x→x' self-arrow, got %q", convLine)
	}

	// Plain filter/preview text.
	plainLine := m.renderThreadRow(selfRow)
	if !strings.Contains(plainLine, "solo · to self") {
		t.Fatalf("plain thread row must render 'solo · to self', got %q", plainLine)
	}

	// Two DIFFERENT agents sharing a colliding LABEL must never be treated as
	// a self-send — identity (alias), not display text, decides.
	collideRow := listThreadRow{ID: 2, FromAgent: "twin-a", ToKind: "agent", ToTarget: "twin-b"}
	if threadIsSelfSend(collideRow) {
		t.Fatalf("two different aliases sharing a colliding label must NOT be treated as a self-send")
	}
	if got := m.renderWho(collideRow, "→"); !strings.Contains(got, "→") {
		t.Fatalf("a genuine two-agent thread must still render an arrow, got %q", got)
	}
}

// TestMailboxSelfSendRendersToSelf covers the mailbox-page half of the same
// fix (spec: "applies in mailbox rows and thread tables"): a thread addressed
// to station and ALSO sent BY station renders "<name> · to self" in the
// mailbox row instead of a bare name that would otherwise read as an ordinary
// message from someone else.
func TestMailboxSelfSendRendersToSelf(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "station"}}})
	m = mustModel(t, next)
	selfRow := listThreadRow{ID: 1, FromAgent: "station", ToKind: "agent", ToTarget: "station", Subject: "note to self", LastAt: 100}
	line := m.renderMailboxLine("  ", selfRow, 80)
	if !strings.Contains(line, "station · to self") {
		t.Fatalf("mailbox row for a self-sent thread must render 'station · to self', got %q", line)
	}
}

// TestEmptyThreadListClearsPreviewNoStaleContent covers spec §5-LOCK screen
// 4: "when the list is empty the preview is EMPTY (clear any stale
// content)" — an agent page that had a loaded preview must blank it out the
// moment its thread list goes empty on a later poll.
func TestEmptyThreadListClearsPreviewNoStaleContent(t *testing.T) {
	fake := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op == "get_thread" {
			b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": []threadEntryRow{{ID: 1, FromAgent: "a", Body: "stale body content"}}, "total": 1})
			return b, nil
		}
		return json.RawMessage(`{}`), nil
	}}
	m := focusConversationList(t, NewModel(fake, Options{}), "a")
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{{ID: 1, FromAgent: "a", EntryCount: 1}}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.viewThreadID != 1 || len(m.viewEntries) == 0 {
		t.Fatalf("setup: expected the preview loaded for thread 1, got viewThreadID=%d entries=%d", m.viewThreadID, len(m.viewEntries))
	}
	preview := m.renderConversationBox(m.layout().rightW, m.layout().rightColumnHeight(), false)
	if !strings.Contains(preview, "stale body content") {
		t.Fatalf("setup: expected the preview to show the loaded body, got:\n%s", preview)
	}

	// The thread list goes empty on the next poll (thread 1 no longer
	// exists for this agent).
	next, _ = m.Update(threadsMsg{threads: nil})
	m = mustModel(t, next)
	if m.conversation != 0 || m.viewThreadID != 0 || len(m.viewEntries) != 0 {
		t.Fatalf("an emptied thread list must clear the selection AND the preview, got conversation=%d viewThreadID=%d entries=%d", m.conversation, m.viewThreadID, len(m.viewEntries))
	}
	previewAfter := m.renderConversationBox(m.layout().rightW, m.layout().rightColumnHeight(), false)
	if strings.Contains(previewAfter, "stale body content") {
		t.Fatalf("preview must not show stale content once the list is empty:\n%s", previewAfter)
	}
}

// TestVitalsSlotRendersNothingWhenDisabledButCodePathExists covers spec
// §5-LOCK decision C: the agent-page header band's vitals slot is a marked
// container — built now, wired end to end — that renders NOTHING while
// hasVitals is false (the package const for all of 0.6.0), while
// renderVitalsLines' own code path, exercised directly with on=true, proves
// the rendering logic is real and correct, ready for 0.6.1 to just flip the
// switch.
func TestVitalsSlotRendersNothingWhenDisabledButCodePathExists(t *testing.T) {
	now := time.Now()
	v := agentVitals{WorkingOn: "v2 broker convergence", CtxUsedK: 120, CtxWindowK: 200, CtxPercent: 60, OutTokensK: 4, LastTurnEndedAt: now.Add(-5 * time.Minute).UnixMilli()}

	if lines := renderVitalsLines(v, false, now); lines != nil {
		t.Fatalf("renderVitalsLines with on=false must render nothing, got %+v", lines)
	}

	lines := renderVitalsLines(v, true, now)
	if len(lines) != 2 {
		t.Fatalf("renderVitalsLines with on=true must render its 2 lines, got %d: %+v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "working on") || !strings.Contains(lines[0], "v2 broker convergence") {
		t.Fatalf("vitals line 0 must show the working-on status, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "ctx ~120k / 200k") || !strings.Contains(lines[1], "60%") || !strings.Contains(lines[1], "out 4k") {
		t.Fatalf("vitals line 1 must show ctx/window/percent/out-tokens, got %q", lines[1])
	}

	// The package const gating production rendering is false for all of
	// 0.6.0 — the real agent-page header band must carry none of the
	// vitals text today.
	if hasVitals {
		t.Fatalf("hasVitals must stay false for the whole 0.6.0 branch — 0.6.1 flips this once real data lands")
	}
	m := NewModel(fakeCaller{}, Options{})
	band := m.renderAgentHeaderBandBox(80, agentEnriched{Alias: "a", Live: true, ModelType: "codex", Role: "reviewer"})
	if strings.Contains(band, "working on") || strings.Contains(band, "ctx ~") {
		t.Fatalf("the production header band must render no vitals text while hasVitals is false:\n%s", band)
	}
}
