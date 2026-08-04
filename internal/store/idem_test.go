package store

import (
	"sync"
	"sync/atomic"
	"testing"
)

// These tests are the SQLite half of a pair: the same assertions run against
// the DynamoDB backend in internal/dynamostore/idem_test.go, and Task 9 folds
// both into the shared conformance suite. They need no endpoint, so they run
// in `just verify` on every machine.

func TestIdemBeginClaimsThenReportsDone(t *testing.T) {
	s := newTestStore(t)
	if _, _, found, err := s.IdemBegin("k1"); err != nil || found {
		t.Fatalf("first IdemBegin: found=%v err=%v, want found=false", found, err)
	}
	if _, done, found, err := s.IdemBegin("k1"); err != nil || !found || done {
		t.Fatalf("in-flight IdemBegin: found=%v done=%v err=%v, want found=true done=false", found, done, err)
	}
	if err := s.IdemComplete("k1", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("IdemComplete: %v", err)
	}
	resp, done, found, err := s.IdemBegin("k1")
	if err != nil || !found || !done {
		t.Fatalf("completed IdemBegin: found=%v done=%v err=%v", found, done, err)
	}
	if string(resp) != `{"ok":true}` {
		t.Fatalf("recorded response = %s", resp)
	}
}

func TestIdemBeginIsAtomicUnderConcurrency(t *testing.T) {
	s := newTestStore(t)
	const n = 8
	var claims int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, found, err := s.IdemBegin("race"); err == nil && !found {
				atomic.AddInt64(&claims, 1)
			}
		}()
	}
	wg.Wait()
	if claims != 1 {
		t.Fatalf("%d callers claimed the key, want exactly 1", claims)
	}
}

// TestIdemKeysAreIndependent guards the obvious way to get the claim wrong:
// a single-row table, or a key that is not actually part of the primary key.
func TestIdemKeysAreIndependent(t *testing.T) {
	s := newTestStore(t)
	if _, _, found, err := s.IdemBegin("a"); err != nil || found {
		t.Fatalf("claim a: found=%v err=%v", found, err)
	}
	if _, _, found, err := s.IdemBegin("b"); err != nil || found {
		t.Fatalf("claim b must be independent of a: found=%v err=%v", found, err)
	}
	if err := s.IdemComplete("a", []byte("ra")); err != nil {
		t.Fatal(err)
	}
	resp, done, found, err := s.IdemBegin("b")
	if err != nil || !found || done || resp != nil {
		t.Fatalf("b after completing a: found=%v done=%v resp=%q err=%v", found, done, resp, err)
	}
}

// TestIdemCompleteUnknownKeyIsNoOp pins the contract the DynamoDB backend has
// to work for: an UpdateItem is an upsert, so without a guard it would CREATE
// a 'done' record for a key nobody ever claimed — and the next caller would be
// told an op had already run when it never had. SQLite's UPDATE matches no
// rows, which is the semantics both backends must present.
func TestIdemCompleteUnknownKeyIsNoOp(t *testing.T) {
	s := newTestStore(t)
	if err := s.IdemComplete("never-claimed", []byte("x")); err != nil {
		t.Fatalf("IdemComplete on an unknown key: %v", err)
	}
	if _, _, found, err := s.IdemBegin("never-claimed"); err != nil || found {
		t.Fatalf("after a no-op complete the key must still be claimable: found=%v err=%v", found, err)
	}
}

// TestIdemCompleteEmptyResponse pins that an op recording no body still reads
// back as done. On DynamoDB a zero-length binary attribute is legal but easily
// confused with an absent one, so "done with no resp" must not degrade to
// "in flight".
func TestIdemCompleteEmptyResponse(t *testing.T) {
	s := newTestStore(t)
	if _, _, found, err := s.IdemBegin("empty"); err != nil || found {
		t.Fatalf("claim: found=%v err=%v", found, err)
	}
	if err := s.IdemComplete("empty", nil); err != nil {
		t.Fatalf("IdemComplete: %v", err)
	}
	resp, done, found, err := s.IdemBegin("empty")
	if err != nil || !found || !done {
		t.Fatalf("completed with an empty response: found=%v done=%v err=%v", found, done, err)
	}
	if len(resp) != 0 {
		t.Fatalf("resp = %q, want empty", resp)
	}
}
