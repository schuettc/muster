package store

import (
	"errors"
	"testing"

	"github.com/schuettc/muster/internal/clock"
)

func TestCreateThreadAppendAndGet(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(Thread{
		Kind: "task", FromAgent: "backend", ToKind: "role", ToTarget: "reviewer",
		Subject: "Review feat/wagers", Ref: "repo=bhw branch=feat/wagers", Status: "open",
	}, "please review the rename")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.AppendEntry(id, "reviewer", "looks good, one nit", "claimed"); err != nil {
		t.Fatalf("append: %v", err)
	}
	th, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if th.Subject != "Review feat/wagers" || len(entries) != 2 {
		t.Fatalf("unexpected thread/entries: %+v / %d", th, len(entries))
	}
	if th.UpdatedAt < th.CreatedAt {
		t.Fatalf("updated_at should advance on append")
	}
}

func TestUnreadCountAndMarkRead(t *testing.T) {
	// UnreadCount/MarkRead now compare entry IDs (monotonic, never colliding),
	// not millisecond timestamps, so no fake clock is needed here.
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "a", Role: "r"}); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"one", "two"} {
		if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "x", ToKind: "agent", ToTarget: "a"}, b); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.UnreadCount("a"); err != nil || n != 2 {
		t.Fatalf("unread before read = %d (%v), want 2", n, err)
	}
	if err := s.MarkRead("a"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("a"); n != 0 {
		t.Fatalf("unread after MarkRead = %d, want 0", n)
	}
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "x", ToKind: "agent", ToTarget: "a"}, "three"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("a"); n != 1 {
		t.Fatalf("unread after new msg = %d, want 1", n)
	}
}

func TestInboxMatchesAgentRoleAndBroadcast(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "rev1", Role: "reviewer", ModelType: "codex"}); err != nil {
		t.Fatal(err)
	}
	mk := func(toKind, toTarget string) {
		if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "backend", ToKind: toKind, ToTarget: toTarget}, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	mk("agent", "rev1")        // direct
	mk("role", "reviewer")     // by role
	mk("broadcast", "")        // to everyone
	mk("agent", "someoneelse") // not for rev1

	in, err := s.Inbox("rev1")
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(in) != 3 {
		t.Fatalf("expected 3 inbox threads for rev1, got %d", len(in))
	}
}

func TestAppendEntryOnMissingThreadReturnsErrThreadNotFoundAndNoOrphan(t *testing.T) {
	s := newTestStore(t)

	const missingThreadID = int64(999999)
	_, err := s.AppendEntry(missingThreadID, "backend", "hello", "")
	if !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("expected ErrThreadNotFound, got %v", err)
	}

	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM entries WHERE thread_id=?`, missingThreadID).Scan(&n); err != nil {
		t.Fatalf("query entries: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected no orphan entries for missing thread, got %d", n)
	}
}

// fakeTick installs a strictly-increasing fake clock (see
// TestUnreadCountAndMarkRead for why: strict ">" comparisons collide within
// one real millisecond on fast hardware).
func fakeTick(t *testing.T) {
	t.Helper()
	var tick int64
	clock.SetForTesting(func() int64 {
		tick++
		return tick
	})
	t.Cleanup(clock.ResetForTesting)
}

func TestInboxIncludesOriginatedThreads(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api"}, "req"); err != nil {
		t.Fatal(err)
	}
	in, err := s.Inbox("web")
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(in) != 1 {
		t.Fatalf("originator's inbox should include the thread it started, got %d threads", len(in))
	}
}

// TestUnreadCountOriginatorSeesPeerReply is the regression test for
// originator blindness: a reply on a thread you started must count as unread
// for you, so the notify fan-out lights your mailbox instead of clearing it.
func TestUnreadCountOriginatorSeesPeerReply(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "web"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateThread(Thread{Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api"}, "req")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.UnreadCount("web"); err != nil || n != 0 {
		t.Fatalf("own send must not count as unread, got %d (%v)", n, err)
	}
	if _, err := s.AppendEntry(id, "api", "done", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.UnreadCount("web"); err != nil || n != 1 {
		t.Fatalf("peer reply on originated thread = %d unread (%v), want 1", n, err)
	}
	if err := s.MarkRead("web"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount("web"); n != 0 {
		t.Fatalf("unread after MarkRead = %d, want 0", n)
	}
}

// TestUnreadCountIgnoresOwnReply: an agent replying on a thread addressed to
// it must not re-flag its own inbox.
func TestUnreadCountIgnoresOwnReply(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "api"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateThread(Thread{Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api"}, "req")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRead("api"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEntry(id, "api", "done", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.UnreadCount("api"); err != nil || n != 0 {
		t.Fatalf("own reply re-flagged own inbox: %d unread (%v), want 0", n, err)
	}
}

// TestThreadsLastEntrySameMillisecond: two entries land in the same
// millisecond (frozen clock) — the last entry identified by Threads() must be
// the one with the higher id (append order), never an ambiguous pick off
// MAX(created_at).
func TestThreadsLastEntrySameMillisecond(t *testing.T) {
	clock.SetForTesting(func() int64 { return 5000 })
	t.Cleanup(clock.ResetForTesting)

	s := newTestStore(t)
	id, err := s.CreateThread(Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast"}, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEntry(id, "b", "second", ""); err != nil {
		t.Fatal(err)
	}
	threads, err := s.Threads(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 {
		t.Fatalf("expected 1 thread, got %d", len(threads))
	}
	if threads[0].LastFrom != "b" {
		t.Fatalf("last entry from = %q, want %q (the higher-id entry, despite same-millisecond created_at)", threads[0].LastFrom, "b")
	}
	if threads[0].EntryCount != 2 {
		t.Fatalf("entry count = %d, want 2", threads[0].EntryCount)
	}
}

// TestThreadsOrderingTiesByID: threads sharing the same updated_at (frozen
// clock) must order newest-id-first.
func TestThreadsOrderingTiesByID(t *testing.T) {
	clock.SetForTesting(func() int64 { return 9000 })
	t.Cleanup(clock.ResetForTesting)

	s := newTestStore(t)
	firstID, err := s.CreateThread(Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast"}, "one")
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := s.CreateThread(Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast"}, "two")
	if err != nil {
		t.Fatal(err)
	}
	threads, err := s.Threads(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 || threads[0].ID != secondID || threads[1].ID != firstID {
		t.Fatalf("tie-break order = %+v, want [%d, %d]", threads, secondID, firstID)
	}
}

// TestThreadsLimitClamp exercises the documented clamp: <=0 defaults to 100,
// over 500 clamps to 500, everything else passes through.
func TestThreadsLimitClamp(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 100}, {-5, 100}, {1, 1}, {500, 500}, {501, 500}, {10000, 500},
	}
	for _, c := range cases {
		if got := clampThreadsLimit(c.in); got != c.want {
			t.Fatalf("clampThreadsLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestThreadsAggregatesOnlyLimitedSet: entries belonging to a thread outside
// the limited window must never be scanned — Threads() must still return the
// correct last-entry/entry-count for the threads it DOES return.
func TestThreadsAggregatesOnlyLimitedSet(t *testing.T) {
	s := newTestStore(t)
	// Oldest thread, several entries — will be excluded once newer threads
	// push it outside limit=1.
	oldID, err := s.CreateThread(Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast"}, "old-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEntry(oldID, "a", "old-2", ""); err != nil {
		t.Fatal(err)
	}
	newID, err := s.CreateThread(Thread{Kind: "message", FromAgent: "b", ToKind: "broadcast"}, "new-1")
	if err != nil {
		t.Fatal(err)
	}
	threads, err := s.Threads(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].ID != newID || threads[0].EntryCount != 1 {
		t.Fatalf("limit=1 result = %+v, want just thread %d with 1 entry", threads, newID)
	}
}

// TestIntentValidationAtStore: CreateThread accepts the empty string and the
// three named intents, and rejects anything else — the validation boundary
// so MCP, CLI, and station cannot diverge (spec §2).
func TestIntentValidationAtStore(t *testing.T) {
	s := newTestStore(t)
	for _, ok := range []string{"", IntentFYI, IntentReply, IntentAction} {
		if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast", Intent: ok}, "body"); err != nil {
			t.Fatalf("intent %q should be valid: %v", ok, err)
		}
	}
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast", Intent: "urgent"}, "body"); err == nil {
		t.Fatal("unknown intent should be rejected")
	}
}

// TestEffectiveIntentOldTasksAreAction: a task row with intent ” (the
// pre-migration state — every v0.5 task) must read as action-requested via
// effectiveIntent, with no retroactive migration backfill needed. A message
// with intent ” stays unspecified — only 'task' triggers the default.
func TestEffectiveIntentOldTasksAreAction(t *testing.T) {
	s := newTestStore(t)
	res, err := s.DB().Exec(`
INSERT INTO threads (kind, from_agent, to_kind, to_target, subject, ref, status, created_at, updated_at)
VALUES ('task', 'backend', 'role', 'reviewer', 'old task', '', 'open', 1, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := res.LastInsertId()
	if _, err := s.DB().Exec(`INSERT INTO entries (thread_id, from_agent, body, created_at) VALUES (?, 'backend', 'please review', 1)`, taskID); err != nil {
		t.Fatal(err)
	}

	msgID, err := s.CreateThread(Thread{Kind: "message", FromAgent: "backend", ToKind: "broadcast"}, "fyi-ish")
	if err != nil {
		t.Fatal(err)
	}

	threads, err := s.Threads(10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]Thread{}
	for _, th := range threads {
		byID[th.ID] = th
	}
	if got := byID[taskID].Intent; got != IntentAction {
		t.Fatalf("old task row effective intent = %q, want %q", got, IntentAction)
	}
	if got := byID[msgID].Intent; got != "" {
		t.Fatalf("message with unset intent effective value = %q, want \"\"", got)
	}
}

// TestInboxAnnotatesLastFromAndUnread is the defect reproduction (bus report,
// live production): get_inbox previously returned only thread metadata
// (from_agent = the ORIGINATOR, subject, updated_at), so an agent listing its
// own inbox could not tell "a peer replied on my originated thread" from "my
// own last send" without a get_thread drill-in. Inbox() must now annotate
// each row with last_from (like Threads()) and a caller-relative unread
// count: a thread whose last entry is a peer's reply shows LastFrom=peer and
// Unread=1 for the originator, while a thread whose last entry is the
// caller's own send shows Unread=0 even though the caller "originated" it.
func TestInboxAnnotatesLastFromAndUnread(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "api"}); err != nil {
		t.Fatal(err)
	}

	// Thread 1: web originates, api replies — last entry is api's, so web's
	// inbox must show last_from=api and unread=1 (the defect: this used to
	// be indistinguishable from thread 2 below).
	repliedID, err := s.CreateThread(Thread{Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api"}, "req")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEntry(repliedID, "api", "done", ""); err != nil {
		t.Fatal(err)
	}

	// Thread 2: web originates and that's the only entry so far — last
	// entry is web's own, so web's inbox must show last_from=web and
	// unread=0 (awaiting a reply, not "answered").
	ownLastID, err := s.CreateThread(Thread{Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api"}, "another req")
	if err != nil {
		t.Fatal(err)
	}

	in, err := s.Inbox("web")
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	byID := map[int64]Thread{}
	for _, th := range in {
		byID[th.ID] = th
	}

	replied := byID[repliedID]
	if replied.LastFrom != "api" {
		t.Fatalf("replied thread last_from = %q, want %q", replied.LastFrom, "api")
	}
	if replied.Unread != 1 {
		t.Fatalf("replied thread unread = %d, want 1 (the defect: peer reply must be visible without get_thread)", replied.Unread)
	}
	if replied.EntryCount != 2 {
		t.Fatalf("replied thread entry_count = %d, want 2", replied.EntryCount)
	}

	ownLast := byID[ownLastID]
	if ownLast.LastFrom != "web" {
		t.Fatalf("own-last thread last_from = %q, want %q", ownLast.LastFrom, "web")
	}
	if ownLast.Unread != 0 {
		t.Fatalf("own-last thread unread = %d, want 0 (own send must never read as unread)", ownLast.Unread)
	}
}

// TestInboxUnreadDropsAfterMarkRead: after MarkRead, a subsequent Inbox call
// for the same alias must show unread=0 on a thread that previously showed
// unread=1 — proving the annotation is watermark-relative, not sticky.
func TestInboxUnreadDropsAfterMarkRead(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "web"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateThread(Thread{Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api"}, "req")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEntry(id, "api", "done", ""); err != nil {
		t.Fatal(err)
	}

	before, err := s.Inbox("web")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].Unread != 1 {
		t.Fatalf("before mark-read: unread = %+v, want 1", before)
	}

	if err := s.MarkRead("web"); err != nil {
		t.Fatal(err)
	}

	after, err := s.Inbox("web")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Unread != 0 {
		t.Fatalf("after mark-read: unread = %+v, want 0", after)
	}
}

func TestInboxScopedBroadcastMatchesProjectOnly(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "in-proj", Project: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "other-proj", Project: "api"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAgent(Agent{Alias: "no-proj"}); err != nil {
		t.Fatal(err)
	}
	mk := func(toTarget string) {
		if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "sender", ToKind: "broadcast", ToTarget: toTarget}, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	mk("web") // scoped to web
	mk("")    // global

	for alias, want := range map[string]int{"in-proj": 2, "other-proj": 1, "no-proj": 1} {
		in, err := s.Inbox(alias)
		if err != nil {
			t.Fatalf("inbox(%s): %v", alias, err)
		}
		if len(in) != want {
			t.Fatalf("inbox(%s) = %d threads, want %d", alias, len(in), want)
		}
	}

	// UnreadCount agrees with Inbox (same canonical predicate).
	if n, err := s.UnreadCount("other-proj"); err != nil || n != 1 {
		t.Fatalf("UnreadCount(other-proj) = %d (%v), want 1", n, err)
	}
	if n, err := s.UnreadCount("in-proj"); err != nil || n != 2 {
		t.Fatalf("UnreadCount(in-proj) = %d (%v), want 2", n, err)
	}
}

func TestScopedBroadcastConcernsDepartedAgentsProject(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(Agent{Alias: "ghost", Project: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DepartAgent("ghost"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateThread(Thread{Kind: "message", FromAgent: "sender", ToKind: "broadcast", ToTarget: "web"}, "hi"); err != nil {
		t.Fatal(err)
	}
	// Tombstoned rows preserve project; a departed agent that re-registers
	// into the same alias sees the scoped broadcast (read-time semantics).
	in, err := s.Inbox("ghost")
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 1 {
		t.Fatalf("departed ghost inbox = %d threads, want 1", len(in))
	}
}
