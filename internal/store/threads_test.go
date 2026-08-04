package store

import (
	"errors"
	"testing"

	"github.com/schuettc/muster/internal/clock"
)

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

// fakeTick installs a strictly-increasing fake clock, so a test that needs
// each write to land at a DISTINCT millisecond gets one: on fast hardware
// several writes share a real millisecond and any strict ">" comparison over
// timestamps then collides. TestPruneEventsExactBoundarySurvives is the
// caller — pruning is the one thing here still decided by wall clock rather
// than by row id.
func fakeTick(t *testing.T) {
	t.Helper()
	var tick int64
	clock.SetForTesting(func() int64 {
		tick++
		return tick
	})
	t.Cleanup(clock.ResetForTesting)
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
