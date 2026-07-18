package station

import (
	"encoding/json"
	"strings"
	"testing"
)

// This file is spec §5-LOCK screen 2's own test suite: the mailbox is its
// own page (pushed by 'm', popped by Esc back to the exact origin) showing
// station's ENTIRE mailbox — read and unread — never just a pinned L0
// section. It also covers item 3's canonical header-badge renderer.

// mailboxTestCaller builds a fakeCaller that counts get_inbox calls and
// answers get_thread with an empty page.
func mailboxTestCaller(getInboxCalls *int) fakeCaller {
	return fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "get_inbox":
			*getInboxCalls++
			return json.RawMessage(`[]`), nil
		case "get_thread":
			b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": []threadEntryRow{}, "total": 0})
			return b, nil
		}
		return json.RawMessage(`{}`), nil
	}}
}

// TestHeaderBadgeOneRendererGrayZeroAmberN covers spec §5-LOCK item 3: the
// SAME headerBadgeText renderer, called from renderBreadcrumb on EVERY
// screen, shows the dim "📬 0" form with no unread mail and the bright
// "📬 N for you" form (or plain "📬 N" off the projects screen) once there
// is some — asserted on a projects screen AND an agent screen, so the two
// can never independently drift.
func TestHeaderBadgeOneRendererGrayZeroAmberN(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "station"}, {Alias: "alpha-1", Project: "alpha"}}})
	m = mustModel(t, next)

	if !strings.Contains(m.renderBreadcrumb(), "📬 0") {
		t.Fatalf("badge must show '📬 0' with no unread mail, got:\n%s", m.renderBreadcrumb())
	}

	next, _ = m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "alpha-1", Subject: "one", LastAt: 100, EntryCount: 1},
		{ID: 2, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "alpha-1", Subject: "two", LastAt: 200, EntryCount: 1},
		{ID: 3, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "alpha-1", Subject: "three", LastAt: 300, EntryCount: 1},
	}})
	m = mustModel(t, next)
	if got := m.operatorInboxCount(); got != 3 {
		t.Fatalf("operatorInboxCount = %d, want 3", got)
	}

	// On screenProjects: amber "N for you".
	if !strings.Contains(m.renderBreadcrumb(), "📬 3 for you") {
		t.Fatalf("projects-screen badge must show '📬 3 for you', got:\n%s", m.renderBreadcrumb())
	}

	// The SAME model, viewed from an agent screen, shows the plain "N" form —
	// still the identical count, from the identical renderer.
	agentView := m
	agentView.screen = screenAgent
	agentView.agent = "alpha-1"
	agentView.stack = append(agentView.stack, navFrame{screen: screenProject, project: "alpha"}, navFrame{screen: screenAgent, project: "alpha", agent: "alpha-1"})
	if !strings.Contains(agentView.renderBreadcrumb(), "📬 3") || strings.Contains(agentView.renderBreadcrumb(), "for you") {
		t.Fatalf("agent-screen badge must show plain '📬 3' (no 'for you'), got:\n%s", agentView.renderBreadcrumb())
	}

	// Back to 0 (every thread acked): the badge returns to the dim "📬 0" form.
	m.ackedThreads = map[int64]bool{1: true, 2: true, 3: true}
	if !strings.Contains(m.renderBreadcrumb(), "📬 0") {
		t.Fatalf("badge must show '📬 0' once every thread is acked, got:\n%s", m.renderBreadcrumb())
	}
}

// TestL0HasNoForYouSection covers spec §5-LOCK item 2: "REMOVE the FOR YOU
// pinned section from L0 entirely" — even with unread mail outstanding, the
// projects screen's own View() must not carry a pinned mail section (only
// the always-present header badge).
func TestL0HasNoForYouSection(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "station"},
		{Alias: "alpha-1", Project: "alpha", Live: true},
	}})
	m = mustModel(t, next)
	next, _ = m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "alpha-1", Subject: "need review", LastAt: 1000, EntryCount: 1},
	}})
	m = mustModel(t, next)
	if m.screen != screenProjects {
		t.Fatalf("setup: expected screenProjects, got %v", m.screen)
	}

	view := m.View()
	if strings.Contains(view, "FOR YOU") {
		t.Fatalf("L0 must carry no FOR YOU section any more:\n%s", view)
	}
	if strings.Contains(view, "need review") {
		t.Fatalf("L0 must not list mail subjects inline — mail is its own page:\n%s", view)
	}
}

// TestMailboxShowsBothReadAndUnreadHistory covers spec §5-LOCK screen 2: 'm'
// pushes the mailbox page showing EVERY thread addressed to station — read
// AND unread — with unread ones marked; reading one clears its unread mark
// but KEEPS the row (re-readable), never removing it from the page.
func TestMailboxShowsBothReadAndUnreadHistory(t *testing.T) {
	var getInboxCalls int
	fake := mailboxTestCaller(&getInboxCalls)

	m := NewModel(fake, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "station"},
		{Alias: "alpha-1", Project: "alpha", Live: true},
	}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 7, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "alpha-1", Subject: "unread one", LastAt: 1000, EntryCount: 1},
		{ID: 8, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "station", Subject: "already read", LastAt: 500, EntryCount: 2},
	}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)

	next, mCmd := m.Update(keyMsg("m"))
	m = mustModel(t, next)
	m = drainCmd(t, m, mCmd)
	if m.screen != screenMailbox {
		t.Fatalf("'m' must push the mailbox page, got screen=%v", m.screen)
	}

	view := m.View()
	if !strings.Contains(view, "unread one") || !strings.Contains(view, "already read") {
		t.Fatalf("mailbox must list BOTH the unread and the already-read thread:\n%s", view)
	}
	if !strings.Contains(view, "•") {
		t.Fatalf("mailbox must mark the unread row with a leading bullet:\n%s", view)
	}
	if getInboxCalls != 0 {
		t.Fatalf("viewing the mailbox must never call get_inbox on its own, got %d calls", getInboxCalls)
	}

	// Enter on the unread row acknowledges it exactly once.
	if m.mailboxSel != 7 {
		t.Fatalf("mailbox selection should default to the newest row (7), got %d", m.mailboxSel)
	}
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if getInboxCalls != 1 {
		t.Fatalf("reading an unread mailbox thread must call get_inbox exactly once, got %d", getInboxCalls)
	}
	if m.screen != screenRead || m.viewThreadID != 7 {
		t.Fatalf("Enter on a mailbox row must read it full-width, got screen=%v viewThreadID=%d", m.screen, m.viewThreadID)
	}

	// Esc back to the mailbox: thread 7 is now dimmed (read) but STILL listed.
	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	if m.screen != screenMailbox {
		t.Fatalf("Esc from reading a mailbox thread must pop back to the mailbox, got %v", m.screen)
	}
	if got := m.operatorInboxCount(); got != 0 {
		t.Fatalf("operatorInboxCount should now be 0 (thread 7 acked, thread 8 was never unread), got %d", got)
	}
	viewAfter := m.View()
	if !strings.Contains(viewAfter, "unread one") {
		t.Fatalf("a read thread must stay listed in the mailbox, not disappear:\n%s", viewAfter)
	}

	// Re-reading the now-read thread 7 must be side-effect-free: no second
	// get_inbox call.
	getInboxCalls = 0
	next, cmd = m.Update(keyMsg("enter"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if getInboxCalls != 0 {
		t.Fatalf("re-reading an already-read mailbox thread must not re-acknowledge, got %d get_inbox calls", getInboxCalls)
	}
}

// TestMailboxPopsBackToExactOrigin covers spec §5-LOCK decision B: 'm'
// pushes an overlay frame that Esc pops back to the EXACT origin — from deep
// inside an agent's own thread list, not just from L0.
func TestMailboxPopsBackToExactOrigin(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "station"},
		{Alias: "alpha-1", Project: "alpha", Live: true},
		{Alias: "beta-1", Project: "beta", Live: true},
	}})
	m = mustModel(t, next)
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{{ID: 1, FromAgent: "alpha-1", EntryCount: 0}}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenProjects {
		t.Fatalf("setup: expected screenProjects (multiple projects), got %v", m.screen)
	}

	m.project = "alpha"
	next, _ = m.Update(keyMsg("enter")) // L0 -> L1
	m = mustModel(t, next)
	next, _ = m.Update(keyMsg("enter")) // L1 -> L2 (alpha-1's own threads)
	m = mustModel(t, next)
	if m.screen != screenAgent || m.agent != "alpha-1" {
		t.Fatalf("setup: expected screenAgent/alpha-1, got screen=%v agent=%q", m.screen, m.agent)
	}

	next, mCmd := m.Update(keyMsg("m"))
	m = mustModel(t, next)
	m = drainCmd(t, m, mCmd)
	if m.screen != screenMailbox {
		t.Fatalf("'m' must push the mailbox, got %v", m.screen)
	}

	next, _ = m.Update(keyMsg("esc"))
	m = mustModel(t, next)
	if m.screen != screenAgent || m.agent != "alpha-1" || m.project != "alpha" {
		t.Fatalf("Esc from the mailbox must pop back to the EXACT origin (screenAgent/alpha/alpha-1), got screen=%v project=%q agent=%q", m.screen, m.project, m.agent)
	}
}

// TestMailJumpTogglesBothDirections covers the 'm' toggle: pressing 'm' opens
// the mailbox page (a push), and pressing 'm' AGAIN while it's already the
// top frame pops it straight back to the exact origin — a second push must
// never stack a duplicate mailbox frame on top of itself.
func TestMailJumpTogglesBothDirections(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "station"},
		{Alias: "alpha-1", Project: "alpha", Live: true},
		{Alias: "beta-1", Project: "beta", Live: true},
	}})
	m = mustModel(t, next)
	if m.screen != screenProjects {
		t.Fatalf("setup: expected screenProjects (multiple projects), got %v", m.screen)
	}
	m.project = "alpha"
	next, _ = m.Update(keyMsg("enter")) // L0 -> L1
	m = mustModel(t, next)
	if m.screen != screenProject {
		t.Fatalf("setup: expected screenProject, got %v", m.screen)
	}
	stackDepthAtOrigin := len(m.stack)

	// First press: pushes the mailbox.
	next, cmd := m.Update(keyMsg("m"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenMailbox {
		t.Fatalf("first 'm' must push the mailbox, got screen=%v", m.screen)
	}
	if len(m.stack) != stackDepthAtOrigin+1 {
		t.Fatalf("first 'm' must push exactly one frame, stack depth=%d, want %d", len(m.stack), stackDepthAtOrigin+1)
	}

	// Second press, while the mailbox is already on top: pops back to the
	// exact origin instead of pushing a second mailbox frame.
	next, cmd = m.Update(keyMsg("m"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenProject {
		t.Fatalf("second 'm' (mailbox already on top) must pop back to the origin screenProject, got screen=%v", m.screen)
	}
	if len(m.stack) != stackDepthAtOrigin {
		t.Fatalf("second 'm' must pop exactly the one mailbox frame it pushed, stack depth=%d, want %d", len(m.stack), stackDepthAtOrigin)
	}

	// A third press re-opens it (fresh push, not left permanently popped).
	next, cmd = m.Update(keyMsg("m"))
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if m.screen != screenMailbox {
		t.Fatalf("third 'm' must re-open the mailbox, got screen=%v", m.screen)
	}
}

// TestMailboxReplyWorksDirectlyFromTheList covers spec §5-LOCK screen 2: 'r'
// replies directly from a selected mailbox row, without first opening it.
func TestMailboxReplyWorksDirectlyFromTheList(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{Alias: "station"})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "station"}, {Alias: "alpha-1", Project: "alpha"}}})
	m = mustModel(t, next)
	next, _ = m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 7, FromAgent: "alpha-1", ToKind: "agent", ToTarget: "station", LastFrom: "alpha-1", Subject: "s", LastAt: 100, EntryCount: 1},
	}})
	m = mustModel(t, next)

	next, mCmd := m.Update(keyMsg("m"))
	m = mustModel(t, next)
	m = drainCmd(t, m, mCmd)
	if m.screen != screenMailbox || m.mailboxSel != 7 {
		t.Fatalf("setup: expected screenMailbox with thread 7 selected, got screen=%v sel=%d", m.screen, m.mailboxSel)
	}

	next, _ = m.Update(keyMsg("r"))
	m = mustModel(t, next)
	if m.composer.phase != composerEditingBody || m.composer.kind != composerKindReply || m.composer.threadID != 7 {
		t.Fatalf("'r' on a mailbox row must open a reply composer targeting it, got %+v", m.composer)
	}
}
