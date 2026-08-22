package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/schuettc/muster/internal/clock"
)

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

	total, _, err := s.SessionUnread("", "/new", "$new", 100)
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

	total, _, err := s.SessionUnread("", "/new", "$new", 100)
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
		total, action, err = s.SessionUnread("", "/s", "$1", 100)
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

// TestStampHarness covers the hook-repair path of the durable-alias spec: an
// alias registered without a harness link (e.g. via the MCP tool in an env
// with no harness UUID) gets one stamped later by the Stop hook.
func TestStampHarness(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "backend"}); err != nil {
		t.Fatal(err)
	}
	if err := s.StampHarness("backend", "uuid-1", "/t/a.jsonl"); err != nil {
		t.Fatal(err)
	}
	ag, ok, err := s.GetAgent("backend")
	if err != nil || !ok {
		t.Fatalf("get: %v %v", ok, err)
	}
	if ag.HarnessSessionID != "uuid-1" || ag.TranscriptPath != "/t/a.jsonl" {
		t.Fatalf("harness_session_id = %q transcript_path = %q, want uuid-1 /t/a.jsonl", ag.HarnessSessionID, ag.TranscriptPath)
	}
	// Unknown alias is a no-op, mirroring TouchAgent's contract.
	if err := s.StampHarness("ghost", "uuid-2", "/t/x.jsonl"); err != nil {
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
// must revive it while KEEPING superseded_by (a returning session on the
// seed's old tuple is exactly the case become-lineage exists to route mail
// through, not a signal that the claim never happened — see
// TestRegisterKeepsSupersededBy in the conformance suite).
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

	// Re-registering the retired seed revives it but keeps SupersededBy — a
	// returning session must not forget its successor.
	if err := s.RegisterAgent(Agent{Alias: "muster-2", SocketPath: "/s2", SessionID: "$9"}); err != nil {
		t.Fatal(err)
	}
	revived, _, _ := s.GetAgent("muster-2")
	if revived.SupersededBy != "alias-routing" || revived.Departed {
		t.Fatalf("re-register must revive while keeping SupersededBy: %+v", revived)
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
