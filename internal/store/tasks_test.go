package store

import (
	"errors"
	"testing"
)

func newTask(t *testing.T, s *Store) int64 {
	t.Helper()
	id, err := s.CreateThread(Thread{Kind: "task", FromAgent: "backend", ToKind: "role", ToTarget: "reviewer", Status: "open"}, "review please")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestClaimTaskIsAtomic(t *testing.T) {
	s := newTestStore(t)
	id := newTask(t, s)

	if err := s.ClaimTask(id, "rev1"); err != nil {
		t.Fatalf("first claim should succeed: %v", err)
	}
	if err := s.ClaimTask(id, "rev2"); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("second claim should be ErrNotClaimable, got %v", err)
	}
	th, _, _ := s.GetThread(id)
	if th.Status != "claimed" {
		t.Fatalf("status should be claimed, got %q", th.Status)
	}
}

func TestTransitionTaskValidatesAndRecords(t *testing.T) {
	s := newTestStore(t)
	id := newTask(t, s)
	_ = s.ClaimTask(id, "rev1")

	if err := s.TransitionTask(id, "rev1", "bogus", ""); err == nil {
		t.Fatalf("expected error for invalid status")
	}
	if err := s.TransitionTask(id, "rev1", "completed", "LGTM"); err != nil {
		t.Fatalf("valid transition: %v", err)
	}
	th, entries, _ := s.GetThread(id)
	if th.Status != "completed" {
		t.Fatalf("status should be completed, got %q", th.Status)
	}
	last := entries[len(entries)-1]
	if last.StatusChange != "completed" || last.Body != "LGTM" {
		t.Fatalf("transition not recorded as entry: %+v", last)
	}
}

func TestTransitionTaskOnMissingThreadReturnsErrThreadNotFound(t *testing.T) {
	s := newTestStore(t)

	const missingThreadID = int64(999999)
	err := s.TransitionTask(missingThreadID, "rev1", "completed", "LGTM")
	if !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("expected ErrThreadNotFound, got %v", err)
	}
}
