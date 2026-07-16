package store

import (
	"reflect"
	"testing"

	"github.com/schuettc/muster/internal/clock"
)

func TestRegisterAgentUpsertAndList(t *testing.T) {
	s := newTestStore(t)

	if err := s.RegisterAgent(Agent{Alias: "backend", Role: "producer", ModelType: "claude", SocketPath: "/s", PaneID: "%1", SessionName: "bhw"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	firstList, err := s.ListAgents()
	if err != nil {
		t.Fatalf("list (first): %v", err)
	}
	if len(firstList) != 1 {
		t.Fatalf("expected 1 agent after first register, got %d", len(firstList))
	}
	firstRegisteredAt := firstList[0].RegisteredAt

	// Re-register (restart) with a new pane — upsert, not duplicate.
	if err := s.RegisterAgent(Agent{Alias: "backend", Role: "producer", ModelType: "claude", SocketPath: "/s2", PaneID: "%9", SessionName: "bhw"}); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	agents, err := s.ListAgents()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent after upsert, got %d", len(agents))
	}
	if agents[0].PaneID != "%9" || agents[0].SocketPath != "/s2" {
		t.Fatalf("upsert did not refresh tuple: %+v", agents[0])
	}
	if agents[0].RegisteredAt == 0 || agents[0].LastSeen == 0 {
		t.Fatalf("timestamps not set: %+v", agents[0])
	}
	if agents[0].RegisteredAt != firstRegisteredAt {
		t.Fatalf("RegisteredAt should be immutable across upsert: first=%d second=%d", firstRegisteredAt, agents[0].RegisteredAt)
	}
	if agents[0].LastSeen < firstList[0].LastSeen {
		t.Fatalf("LastSeen should not go backwards across upsert: first=%d second=%d", firstList[0].LastSeen, agents[0].LastSeen)
	}
}

func TestRegisterAgentRoundTripsSessionIDAndGetAgent(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "backend", Role: "producer", ModelType: "claude", SocketPath: "/s", PaneID: "%1", SessionName: "muster", SessionID: "$3"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok, err := s.GetAgent("backend")
	if err != nil || !ok {
		t.Fatalf("GetAgent: ok=%v err=%v", ok, err)
	}
	if got.SessionID != "$3" || got.SessionName != "muster" {
		t.Fatalf("session fields not round-tripped: %+v", got)
	}
	if _, ok, _ := s.GetAgent("nope"); ok {
		t.Fatalf("GetAgent should report ok=false for unknown alias")
	}
}

func TestAgentLabelAndDelete(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{
		Alias: "muster-2", Role: "peer", ModelType: "codex",
		SocketPath: "/tmp/tmux-0/proj-muster", SessionID: "$1",
		Project: "muster", Label: "frontend", LabelManual: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetAgent("muster-2")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Project != "muster" || got.Label != "frontend" || !got.LabelManual {
		t.Fatalf("round-trip=%+v", got)
	}

	// upsert refreshes label fields
	if err := s.RegisterAgent(Agent{Alias: "muster-2", Label: "backend", LabelManual: false}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetAgent("muster-2")
	if got.Label != "backend" || got.LabelManual {
		t.Fatalf("after upsert=%+v", got)
	}

	// delete removes the row, leaves the table usable
	if err := s.DeleteAgent("muster-2"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetAgent("muster-2"); ok {
		t.Fatal("agent should be gone after DeleteAgent")
	}
	if err := s.DeleteAgent("nonexistent"); err != nil {
		t.Fatalf("DeleteAgent of unknown alias must be a no-op, got %v", err)
	}
}

// TestMarkReadRecordsEntryWatermark: no incrementing fake clock needed — the
// wall clock is frozen at one instant so every entry genuinely lands in the
// same millisecond, and the entry-ID watermark (not created_at) still tells
// them apart correctly.
func TestMarkReadRecordsEntryWatermark(t *testing.T) {
	clock.SetForTesting(func() int64 { return 1000 })
	t.Cleanup(clock.ResetForTesting)

	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "a"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateThread(Thread{Kind: "message", FromAgent: "x", ToKind: "agent", ToTarget: "a"}, "one")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRead("a"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.UnreadCount("a"); err != nil || n != 0 {
		t.Fatalf("unread right after MarkRead = %d (%v), want 0", n, err)
	}
	got, ok, err := s.GetAgent("a")
	if err != nil || !ok {
		t.Fatalf("GetAgent: ok=%v err=%v", ok, err)
	}
	if got.LastReadEntryID == 0 {
		t.Fatalf("MarkRead should have recorded a non-zero entry watermark, got %+v", got)
	}

	// A new entry, same frozen millisecond, but a higher entry id — must be
	// unread despite created_at being identical to the watermark snapshot.
	if _, err := s.AppendEntry(id, "x", "two", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.UnreadCount("a"); err != nil || n != 1 {
		t.Fatalf("same-millisecond entry after MarkRead unread = %d (%v), want 1", n, err)
	}
}

// TestSessionUnreadCountsDistinctThreads: a broadcast concerning both aliases
// of one session (split-alias identity) must count once, never twice — no
// summing of per-alias counts.
func TestSessionUnreadCountsDistinctThreads(t *testing.T) {
	s := newTestStore(t)
	for _, alias := range []string{"session-name", "chosen-alias"} {
		if err := s.RegisterAgent(Agent{Alias: alias, SocketPath: "/s", SessionID: "$1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "peer", ToKind: "broadcast"}, "hi all"); err != nil {
		t.Fatal(err)
	}
	total, action, err := s.SessionUnread("/s", "$1")
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("broadcast concerning both sibling aliases counted total=%d, want 1", total)
	}
	if action != 0 {
		t.Fatalf("plain message counted action=%d, want 0", action)
	}
}

// TestSessionUnreadExcludesSiblingAuthors: alias A (a session member) writes
// a thread; sibling alias B must not see it as unread — actor exclusion is
// session-based, not per-alias.
func TestSessionUnreadExcludesSiblingAuthors(t *testing.T) {
	s := newTestStore(t)
	for _, alias := range []string{"a1", "a2"} {
		if err := s.RegisterAgent(Agent{Alias: alias, SocketPath: "/s", SessionID: "$1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "outsider"}, "hello"); err != nil {
		t.Fatal(err)
	}
	total, action, err := s.SessionUnread("/s", "$1")
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || action != 0 {
		t.Fatalf("a session's own write must not flag its own thread unread, got total=%d action=%d", total, action)
	}
}

// TestSessionUnreadActionCount: a task thread (effective intent
// action-requested) addressed to a session alias, unread, counts in action.
func TestSessionUnreadActionCount(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "worker", SocketPath: "/s", SessionID: "$1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateThread(Thread{Kind: "task", FromAgent: "backend", ToKind: "agent", ToTarget: "worker", Status: "open"}, "please do X"); err != nil {
		t.Fatal(err)
	}
	total, action, err := s.SessionUnread("/s", "$1")
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || action != 1 {
		t.Fatalf("task addressed to session alias: total=%d action=%d, want 1,1", total, action)
	}
}

// TestSessionUnreadEmptyTupleNeverGroups: an agent registered without a live
// tmux identity (empty socket/session) is its own singleton — SessionUnread
// must never treat the empty tuple as a group to aggregate over.
func TestSessionUnreadEmptyTupleNeverGroups(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "solo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "x", ToKind: "agent", ToTarget: "solo"}, "hi"); err != nil {
		t.Fatal(err)
	}
	total, action, err := s.SessionUnread("", "")
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || action != 0 {
		t.Fatalf("empty socket/session tuple must never group, got total=%d action=%d", total, action)
	}
}

// TestThreadConcernsSessionJoinEquivalence: threadConcernsJoin (the JOIN form
// used by SessionUnread) must agree with threadConcerns (the literal-bind
// form used by Inbox/UnreadCount) across a fixture matrix of thread shapes
// and aliases — the "one canonical predicate" rule surviving a join.
func TestThreadConcernsSessionJoinEquivalence(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "rev1", Role: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "rev2", Role: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "other", Role: "producer"}); err != nil {
		t.Fatal(err)
	}

	mk := func(kind, fromAgent, toKind, toTarget string) {
		if _, err := s.CreateThread(Thread{Kind: kind, FromAgent: fromAgent, ToKind: toKind, ToTarget: toTarget}, "body"); err != nil {
			t.Fatal(err)
		}
	}
	mk("message", "backend", "agent", "rev1")
	mk("message", "backend", "role", "reviewer")
	mk("message", "backend", "broadcast", "")
	mk("message", "rev2", "agent", "someone-else")
	mk("task", "other", "agent", "rev1")

	idsMatching := func(query string, args ...any) []int64 {
		t.Helper()
		rows, err := s.DB().Query(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}

	for _, alias := range []string{"rev1", "rev2", "other", "nobody"} {
		want := idsMatching(`SELECT id FROM threads WHERE `+threadConcerns+` ORDER BY id`, alias, alias, alias)
		got := idsMatching(`WITH sess AS (SELECT ? AS alias) SELECT threads.id FROM threads JOIN sess ON `+threadConcernsJoin+` ORDER BY threads.id`, alias)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("alias=%q: threadConcerns=%v threadConcernsJoin=%v disagree", alias, want, got)
		}
	}
}
