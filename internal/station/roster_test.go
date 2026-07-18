package station

import (
	"strings"
	"testing"
)

// This file covers the roster/agent-list enrichments carried over from
// iteration-5 into the spec §5-LOCK design: last-active timestamps and
// unread-age suffixes on the agent list, plus the eye-marker removal check.
// (The pre-lock "FOR YOU" pinned section these once lived alongside is gone
// — see mailbox_test.go for its §5-LOCK replacement, the mailbox page.)

// TestLastActiveRendersOnAgentStripAndHeaderBand covers Tier 0a: once
// lastActiveMsg has resolved an alias, its "last active: <relative>"/
// "active <t>" text shows on the agent-list row AND the agent page's own
// header band (spec §5-LOCK screen 4) — fed directly (bypassing the fetch)
// since the model's cache is the only thing rendering depends on.
func TestLastActiveRendersOnAgentStripAndHeaderBand(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{{Alias: "alpha-1", Project: "alpha", Live: true}}})
	m = mustModel(t, next)
	m.pollGen = 5
	next, _ = m.Update(lastActiveMsg{alias: "alpha-1", ts: 1000, gen: 5})
	m = mustModel(t, next)
	if m.lastActive["alpha-1"] != 1000 {
		t.Fatalf("expected lastActive[alpha-1]=1000, got %v", m.lastActive)
	}

	stripLine := m.renderRosterLine("  ", m.agents[0], 80)
	if !strings.Contains(stripLine, "last active:") {
		t.Fatalf("agent list row must show 'last active:', got %q", stripLine)
	}

	// A stale-generation msg must not apply.
	next, _ = m.Update(lastActiveMsg{alias: "alpha-1", ts: 999999, gen: 1})
	m = mustModel(t, next)
	if m.lastActive["alpha-1"] != 1000 {
		t.Fatalf("a stale-gen lastActiveMsg must not overwrite the cache, got %v", m.lastActive["alpha-1"])
	}

	// Agent page (screenAgent) header band shows it too.
	bandView := m.renderAgentHeaderBandBox(80, m.agents[0])
	if !strings.Contains(bandView, "active") {
		t.Fatalf("agent page header band must show last-active, got:\n%s", bandView)
	}
}

// TestUnreadAgeRendersOnRosterAndProjectRows covers Tier 0b: an unread
// count's row gains "(N · age)" — the oldest waiting thread's age — derived
// client-side from already-fetched thread data (never get_inbox).
func TestUnreadAgeRendersOnRosterAndProjectRows(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	// SocketPath/SessionID must be set: computeProjectSummaries only rolls
	// an agent's Unread into its project's total for a resolvable session
	// tuple (see nav.go's computeProjectSummaries).
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "alpha-1", Project: "alpha", Live: true, Unread: 2, SocketPath: "/s", SessionID: "$1"},
	}})
	m = mustModel(t, next)
	next, _ = m.Update(threadsMsg{threads: []listThreadRow{
		{ID: 1, FromAgent: "other", ToKind: "agent", ToTarget: "alpha-1", LastFrom: "other", LastAt: 1},
	}})
	m = mustModel(t, next)

	stripLine := m.renderRosterLine("  ", m.agents[0], 80)
	if !strings.Contains(stripLine, "(2") || !strings.Contains(stripLine, " · ") {
		t.Fatalf("roster row must show the unread age suffix '(2 · age)', got %q", stripLine)
	}

	rows := m.projectRows()
	idx := keyIndex(rows, projKey, "alpha")
	if idx < 0 {
		t.Fatalf("expected an 'alpha' project row, got %+v", rows)
	}
	projLine := m.renderProjectLine("  ", rows[idx], 80)
	if !strings.Contains(projLine, " · ") {
		t.Fatalf("project rollup row must show the unread age suffix, got %q", projLine)
	}
}

// TestEyeMarkerRemovedEverywhere covers: nothing in station's rendering may
// reference internal/tmuxenv.SessionAttached's own 👁 glyph.
func TestEyeMarkerRemovedEverywhere(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{})
	next, _ := m.Update(agentsMsg{rows: []agentEnriched{
		{Alias: "alpha-1", Project: "alpha", Live: true},
		{Alias: "alpha-2", Project: "alpha", Live: true},
	}})
	m = mustModel(t, next)

	idx := keyIndex(m.agents, agentKey, "alpha-1")
	if strings.Contains(m.renderRosterLine("  ", m.agents[idx], 80), "👁") {
		t.Fatalf("roster rows must never show the 👁 marker any more")
	}

	m.screen = screenAgent
	m.agent = "alpha-1"
	if strings.Contains(m.renderBreadcrumb(), "👁") {
		t.Fatalf("the agent page header must never show the 👁 marker any more, got:\n%s", m.renderBreadcrumb())
	}

	help := m
	help.helpOpen = true
	if strings.Contains(help.View(), "👁") {
		t.Fatalf("the help overlay must never mention the 👁 marker any more:\n%s", help.View())
	}
}
