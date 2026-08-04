package store

import (
	"reflect"
	"testing"

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
