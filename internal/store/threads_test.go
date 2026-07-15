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
	// UnreadCount/MarkRead compare millisecond timestamps with a strict ">",
	// so this test needs each clock.NowMillis() call (CreateThread,
	// MarkRead, CreateThread) to land on a distinct tick. The real clock
	// can collide within the same millisecond on fast hardware, so drive
	// a fake, strictly-increasing clock instead of the wall clock.
	var tick int64
	clock.SetForTesting(func() int64 {
		tick++
		return tick
	})
	t.Cleanup(clock.ResetForTesting)

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
