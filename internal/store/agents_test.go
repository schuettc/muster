package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

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

// TestDepartAgentTombstonesPreservingFields covers the deregistration
// tombstone (spec: departed history must survive so it stays drillable): after
// DepartAgent, the row still exists (ListAgents/GetAgent both still find it),
// Departed is true, and every other field — project, label, label_manual, the
// read watermark — is untouched. A subsequent RegisterAgent for the same
// alias revives it (Departed back to false) without needing any of those
// fields re-supplied by the caller for them to still be present (RegisterAgent
// preserves them exactly like a plain restart-upsert already does).
func TestDepartAgentTombstonesPreservingFields(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{
		Alias: "muster-2", Role: "peer", ModelType: "codex",
		SocketPath: "/s", SessionID: "$1",
		Project: "muster", Label: "frontend", LabelManual: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "x", ToKind: "agent", ToTarget: "muster-2"}, "hi"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRead("muster-2"); err != nil {
		t.Fatal(err)
	}
	before, ok, err := s.GetAgent("muster-2")
	if err != nil || !ok {
		t.Fatalf("get before depart: ok=%v err=%v", ok, err)
	}
	if before.LastReadEntryID == 0 {
		t.Fatalf("setup: expected a non-zero read watermark, got %+v", before)
	}

	if err := s.DepartAgent("muster-2"); err != nil {
		t.Fatal(err)
	}
	after, ok, err := s.GetAgent("muster-2")
	if err != nil || !ok {
		t.Fatalf("expected the row to SURVIVE DepartAgent, got ok=%v err=%v", ok, err)
	}
	if !after.Departed {
		t.Fatalf("expected Departed=true after DepartAgent, got %+v", after)
	}
	if after.Project != "muster" || after.Label != "frontend" || !after.LabelManual {
		t.Fatalf("DepartAgent must preserve project/label/label_manual, got %+v", after)
	}
	if after.LastReadEntryID != before.LastReadEntryID {
		t.Fatalf("DepartAgent must preserve the read watermark: before=%d after=%d", before.LastReadEntryID, after.LastReadEntryID)
	}

	list, err := s.ListAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].Departed {
		t.Fatalf("ListAgents must still include the departed row, got %+v", list)
	}

	// DepartAgent on an unknown alias is a no-op, no error.
	if err := s.DepartAgent("nonexistent"); err != nil {
		t.Fatalf("DepartAgent of unknown alias must be a no-op, got %v", err)
	}

	// A returning session revives it: RegisterAgent resets Departed to false.
	if err := s.RegisterAgent(Agent{
		Alias: "muster-2", Role: "peer", ModelType: "codex",
		SocketPath: "/s", SessionID: "$1",
		Project: "muster", Label: "frontend", LabelManual: true,
	}); err != nil {
		t.Fatal(err)
	}
	revived, ok, err := s.GetAgent("muster-2")
	if err != nil || !ok {
		t.Fatalf("get after revive: ok=%v err=%v", ok, err)
	}
	if revived.Departed {
		t.Fatalf("re-registering a departed alias must revive it (Departed=false), got %+v", revived)
	}
	if revived.LastReadEntryID != before.LastReadEntryID {
		t.Fatalf("revival must not disturb the read watermark: before=%d after=%d", before.LastReadEntryID, revived.LastReadEntryID)
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
		if err := s.RegisterAgent(Agent{Alias: alias, SocketPath: "/s", SessionID: "$1", SessionCreated: 100}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "peer", ToKind: "broadcast"}, "hi all"); err != nil {
		t.Fatal(err)
	}
	total, action, err := s.SessionUnread("/s", "$1", 100)
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
		if err := s.RegisterAgent(Agent{Alias: alias, SocketPath: "/s", SessionID: "$1", SessionCreated: 100}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "outsider"}, "hello"); err != nil {
		t.Fatal(err)
	}
	total, action, err := s.SessionUnread("/s", "$1", 100)
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
	if err := s.RegisterAgent(Agent{Alias: "worker", SocketPath: "/s", SessionID: "$1", SessionCreated: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateThread(Thread{Kind: "task", FromAgent: "backend", ToKind: "agent", ToTarget: "worker", Status: "open"}, "please do X"); err != nil {
		t.Fatal(err)
	}
	total, action, err := s.SessionUnread("/s", "$1", 100)
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
	total, action, err := s.SessionUnread("", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || action != 0 {
		t.Fatalf("empty socket/session tuple must never group, got total=%d action=%d", total, action)
	}
}

// TestSessionUnreadRequiresIncarnationMatch pins spec §5.1 at the store
// layer: a tmux-tuple row only seeds the session's unread/alias math when
// its session_created matches the caller's live value; 0 never matches.
// Paneless tuples (empty socket) are exempt — harness UUIDs don't recycle.
func TestSessionUnreadRequiresIncarnationMatch(t *testing.T) {
	s := newTestStore(t)
	// current incarnation
	if err := s.RegisterAgent(Agent{Alias: "current", SocketPath: "/s", SessionID: "$1", SessionCreated: 200}); err != nil {
		t.Fatal(err)
	}
	// legacy ghost on the recycled ID
	if err := s.RegisterAgent(Agent{Alias: "ghost", SocketPath: "/s", SessionID: "$1", SessionCreated: 0}); err != nil {
		t.Fatal(err)
	}
	// mail for each
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "current"}, "for the live one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "ghost"}, "for the ghost"); err != nil {
		t.Fatal(err)
	}

	total, _, err := s.SessionUnread("/s", "$1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("live incarnation must see ONLY its own unread, got %d", total)
	}
	aliases, err := s.SessionAliasLineage("/s", "$1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 1 || aliases[0] != "current" {
		t.Fatalf("lineage must not include the ghost, got %v", aliases)
	}
	// paneless: created is irrelevant
	if err := s.RegisterAgent(Agent{Alias: "bg", SocketPath: "", SessionID: "uuid-9", SessionCreated: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "bg"}, "paneless mail"); err != nil {
		t.Fatal(err)
	}
	total, _, err = s.SessionUnread("", "uuid-9", 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("paneless tuple must be exempt from the incarnation check, got %d", total)
	}
}

// TestThreadConcernsSessionJoinEquivalence: threadConcernsJoin (the JOIN form
// used by SessionUnread) must agree with threadConcerns (the literal-bind
// form used by Inbox/UnreadCount) across a fixture matrix of thread shapes
// and aliases — the "one canonical predicate" rule surviving a join.
func TestThreadConcernsSessionJoinEquivalence(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "rev1", Role: "reviewer", Project: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "rev2", Role: "reviewer", Project: "api"}); err != nil {
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
	mk("message", "backend", "broadcast", "web")  // scoped: concerns rev1 only
	mk("message", "backend", "broadcast", "api")  // scoped: concerns rev2 only
	mk("message", "backend", "broadcast", "gone") // scoped: concerns nobody

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
		want := idsMatching(`SELECT id FROM threads WHERE `+threadConcerns+` ORDER BY id`, alias, alias, alias, alias)
		got := idsMatching(`WITH sess AS (SELECT ? AS alias) SELECT threads.id FROM threads JOIN sess ON `+threadConcernsJoin+` ORDER BY threads.id`, alias)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("alias=%q: threadConcerns=%v threadConcernsJoin=%v disagree", alias, want, got)
		}
	}
}

// TestSessionUnreadPerAliasWatermarks: two aliases in one session tuple must
// each respect their own last_read_entry_id watermark — alias A's read
// position must not suppress unread threads concerning alias B within the
// same session, and vice versa. SessionUnread evaluates each alias's
// watermark independently when building the set of unread threads.
func TestSessionUnreadPerAliasWatermarks(t *testing.T) {
	s := newTestStore(t)
	// Register two aliases sharing the same (socket_path, session_id) tuple.
	if err := s.RegisterAgent(Agent{Alias: "aliasA", SocketPath: "/s", SessionID: "$1", SessionCreated: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "aliasB", SocketPath: "/s", SessionID: "$1", SessionCreated: 100}); err != nil {
		t.Fatal(err)
	}

	// Create a thread addressed to A, from an outsider.
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "outsider", ToKind: "agent", ToTarget: "aliasA"}, "msg for A"); err != nil {
		t.Fatal(err)
	}

	// Create a thread addressed to B, from an outsider.
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "outsider", ToKind: "agent", ToTarget: "aliasB"}, "msg for B"); err != nil {
		t.Fatal(err)
	}

	// Before any MarkRead, both threads unread.
	total, _, err := s.SessionUnread("/s", "$1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("before MarkRead: expected 2 unread, got %d", total)
	}

	// Mark A as read — advances A's watermark past all existing entries.
	if err := s.MarkRead("aliasA"); err != nil {
		t.Fatal(err)
	}

	// SessionUnread should still see B's thread as unread (total=1).
	// A's read watermark must not suppress B's threads.
	total, _, err = s.SessionUnread("/s", "$1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("after MarkRead(aliasA): expected 1 unread, got %d", total)
	}

	// Now mark B as read.
	if err := s.MarkRead("aliasB"); err != nil {
		t.Fatal(err)
	}

	// SessionUnread should return total=0.
	total, _, err = s.SessionUnread("/s", "$1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("after MarkRead(aliasB): expected 0 unread, got %d", total)
	}
}

// TestSessionUnreadFollowsSupersessionLineage reproduces the live-rig gap:
// become + resume leaves the retired seed's row on its OLD (now-dead) tuple
// while its identity moved to a NEW tuple. Mail addressed to the retired
// seed alias must still count against the session now sitting on the new
// tuple — the lineage walk, not a flat tuple match.
func TestSessionUnreadFollowsSupersessionLineage(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "seed", SocketPath: "/old", SessionID: "$old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Become("seed", "claimed"); err != nil {
		t.Fatal(err)
	}
	// Simulate hookSessionStartResume's reclaim: the claimed alias's row
	// re-registers onto a brand-new tuple (the resumed session), while the
	// seed's row stays exactly where Become left it — on the OLD tuple,
	// departed, superseded_by="claimed".
	if err := s.RegisterAgent(Agent{Alias: "claimed", SocketPath: "/new", SessionID: "$new", SessionCreated: 100}); err != nil {
		t.Fatal(err)
	}
	seed, _, _ := s.GetAgent("seed")
	if !seed.Departed || seed.SocketPath != "/old" || seed.SessionID != "$old" {
		t.Fatalf("seed row expected to stay departed on the old tuple: %+v", seed)
	}

	// A straggler addressed to the RETIRED seed alias.
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "outsider", ToKind: "agent", ToTarget: "seed"}, "hi seed"); err != nil {
		t.Fatal(err)
	}

	total, _, err := s.SessionUnread("/new", "$new", 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("mail addressed to a superseded seed must count on the successor's live tuple: got total=%d, want 1", total)
	}
}

// TestSessionUnreadFollowsChainedLineage: A→B→C (A became B, B became C),
// with only C sitting on the live tuple. The lineage walk must resolve
// TRANSITIVELY through the middle link — mail addressed to the original
// alias A must still reach C's tuple, not just B's.
func TestSessionUnreadFollowsChainedLineage(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "a", SocketPath: "/old", SessionID: "$old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Become("a", "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Become("b", "c"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "c", SocketPath: "/new", SessionID: "$new", SessionCreated: 100}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "outsider", ToKind: "agent", ToTarget: "a"}, "hi a"); err != nil {
		t.Fatal(err)
	}

	total, _, err := s.SessionUnread("/new", "$new", 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("mail addressed to the ORIGINAL alias of a two-hop chain must reach the final tuple: got total=%d, want 1", total)
	}
}

// TestSessionUnreadLineageCycleGuard: a malformed superseded_by cycle
// (deliberately written past Become's own guards, via raw SQL — Become
// itself can never produce one) must not hang the recursive CTE and must
// not miscount. UNION's row-level dedup is what bounds the recursion.
func TestSessionUnreadLineageCycleGuard(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "x", SocketPath: "/s", SessionID: "$1", SessionCreated: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "y", SocketPath: "/other", SessionID: "$other"}); err != nil {
		t.Fatal(err)
	}
	// Force a cycle: x superseded_by y, y superseded_by x. Both rows sit on
	// the queried tuple's neighborhood is irrelevant here — the point is the
	// recursive walk from x's own tuple must terminate.
	if _, err := s.DB().Exec(`UPDATE agents SET departed=1, superseded_by='y' WHERE alias='x'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE agents SET departed=1, superseded_by='x' WHERE alias='y'`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "outsider", ToKind: "agent", ToTarget: "x"}, "hi x"); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var total, action int
	var err error
	go func() {
		total, action, err = s.SessionUnread("/s", "$1", 100)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SessionUnread hung on a superseded_by cycle")
	}
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("cycle must not miscount (double-add) the shared alias: got total=%d, want 1", total)
	}
	_ = action
}

// TestSetHarnessSessionID covers the hook-repair path of the durable-alias
// spec: an alias registered without a harness link (e.g. via the MCP tool in
// an env with no harness UUID) gets one stamped later by the Stop hook.
func TestSetHarnessSessionID(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "backend"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHarnessSessionID("backend", "uuid-1"); err != nil {
		t.Fatal(err)
	}
	ag, ok, err := s.GetAgent("backend")
	if err != nil || !ok {
		t.Fatalf("get: %v %v", ok, err)
	}
	if ag.HarnessSessionID != "uuid-1" {
		t.Fatalf("harness_session_id = %q, want uuid-1", ag.HarnessSessionID)
	}
	// Unknown alias is a no-op, mirroring TouchAgent's contract.
	if err := s.SetHarnessSessionID("ghost", "uuid-2"); err != nil {
		t.Fatal(err)
	}
}

// TestBecomeClonesIdentityAndRetiresSeed covers the become spec's core move:
// the claimed alias inherits the seed's full identity INCLUDING the read
// watermark (without it, all of history would flip unread), and the seed
// retires as a tombstone with its own history intact.
func TestBecomeClonesIdentityAndRetiresSeed(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{
		Alias: "muster-2", Role: "peer", ModelType: "claude",
		SocketPath: "/s", SessionID: "$1", SessionCreated: 111, PaneID: "%1",
		SessionName: "muster-2", HarnessSessionID: "uuid-1",
		Project: "muster", Label: "durable-alias", LabelManual: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRead("muster-2"); err != nil { // establish a nonzero watermark
		t.Fatal(err)
	}
	seed, _, _ := s.GetAgent("muster-2")

	if err := s.Become("muster-2", "alias-routing"); err != nil {
		t.Fatal(err)
	}
	to, ok, err := s.GetAgent("alias-routing")
	if err != nil || !ok {
		t.Fatalf("claimed alias missing: %v %v", ok, err)
	}
	if to.SocketPath != "/s" || to.SessionID != "$1" || to.SessionCreated != 111 ||
		to.PaneID != "%1" || to.HarnessSessionID != "uuid-1" ||
		to.Project != "muster" || to.Label != "durable-alias" || !to.LabelManual ||
		to.Role != "peer" || to.ModelType != "claude" || to.Departed {
		t.Fatalf("clone dropped identity fields: %+v", to)
	}
	if to.LastReadEntryID != seed.LastReadEntryID {
		t.Fatalf("watermark not carried: got %d want %d", to.LastReadEntryID, seed.LastReadEntryID)
	}
	from, _, _ := s.GetAgent("muster-2")
	if !from.Departed {
		t.Fatalf("seed not retired: %+v", from)
	}
}

// TestBecomeStampsSupersededBy: Become's ground-truth lineage marker (fix
// round 1, replacing hookResumeSuperseded's tuple-sharing heuristic) —
// the seed's superseded_by must name the claimed alias, the CLONE must NOT
// inherit it (a chained become's successor starts unsuperseded even though
// its own seed was itself superseded), and re-registering the retired seed
// must clear it (a revived/re-registered alias is no longer superseded — the
// operator may have purged the successor and taken the name back).
func TestBecomeStampsSupersededBy(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "muster-2", SocketPath: "/s", SessionID: "$1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Become("muster-2", "alias-routing"); err != nil {
		t.Fatal(err)
	}
	seed, _, _ := s.GetAgent("muster-2")
	if seed.SupersededBy != "alias-routing" {
		t.Fatalf("seed.SupersededBy = %q, want %q", seed.SupersededBy, "alias-routing")
	}
	successor, _, _ := s.GetAgent("alias-routing")
	if successor.SupersededBy != "" {
		t.Fatalf("clone must not inherit SupersededBy: %+v", successor)
	}

	// Chain the claim again: alias-routing -> final-name. The middle link
	// (alias-routing) becomes superseded too; the seed (muster-2) is
	// untouched by this second call and keeps pointing at alias-routing, not
	// final-name — superseded_by never chases the chain to its end.
	if err := s.Become("alias-routing", "final-name"); err != nil {
		t.Fatal(err)
	}
	middle, _, _ := s.GetAgent("alias-routing")
	if middle.SupersededBy != "final-name" {
		t.Fatalf("middle.SupersededBy = %q, want %q", middle.SupersededBy, "final-name")
	}
	seedAfterChain, _, _ := s.GetAgent("muster-2")
	if seedAfterChain.SupersededBy != "alias-routing" {
		t.Fatalf("seed.SupersededBy changed by a later become on its successor: %+v", seedAfterChain)
	}
	end, _, _ := s.GetAgent("final-name")
	if end.SupersededBy != "" {
		t.Fatalf("final clone must not inherit SupersededBy: %+v", end)
	}

	// Re-registering the retired seed clears it — a revived/re-registered
	// alias is no longer superseded.
	if err := s.RegisterAgent(Agent{Alias: "muster-2", SocketPath: "/s2", SessionID: "$9"}); err != nil {
		t.Fatal(err)
	}
	revived, _, _ := s.GetAgent("muster-2")
	if revived.SupersededBy != "" || revived.Departed {
		t.Fatalf("re-register must clear SupersededBy and revive: %+v", revived)
	}
}

// TestBecomeGuards: missing from and existing to (live OR tombstoned) both
// fail with the typed sentinels — become never silently fuses identities.
func TestBecomeGuards(t *testing.T) {
	s := newTestStore(t)
	if err := s.Become("ghost", "x"); !errors.Is(err, ErrBecomeFromMissing) {
		t.Fatalf("missing from: got %v", err)
	}
	_ = s.RegisterAgent(Agent{Alias: "a"})
	_ = s.RegisterAgent(Agent{Alias: "b"})
	if err := s.Become("a", "b"); !errors.Is(err, ErrBecomeToExists) {
		t.Fatalf("live to: got %v", err)
	}
	_ = s.DepartAgent("b")
	if err := s.Become("a", "b"); !errors.Is(err, ErrBecomeToExists) {
		t.Fatalf("tombstoned to must ALSO refuse: got %v", err)
	}
	// Departed FROM is fine: a session may claim after gc tombstoned its seed.
	_ = s.DepartAgent("a")
	if err := s.Become("a", "c"); err != nil {
		t.Fatalf("departed from should still clone: %v", err)
	}
}
