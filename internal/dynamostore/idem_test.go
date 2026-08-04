package dynamostore

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// TestIdemOutcome covers the four ways a condition-failed claim can resolve.
// Two of them cannot be provoked against a live DynamoDB from a test — a
// PutItem that commits and loses its acknowledgement, and a TTL that reaps the
// record inside the gap between the write and the read — which is why the
// decision is a pure function. It needs no endpoint.
func TestIdemOutcome(t *testing.T) {
	const mine = "MYCLAIM"
	rec := func(state, claim string, resp []byte) map[string]types.AttributeValue {
		item := map[string]types.AttributeValue{
			"state": attrS(state),
			"claim": attrS(claim),
		}
		if len(resp) > 0 {
			item["resp"] = attrB(resp)
		}
		return item
	}
	tests := []struct {
		name      string
		item      map[string]types.AttributeValue
		wantResp  string
		wantDone  bool
		wantFound bool
	}{
		// The record was reaped between our write and our read: in-flight, so
		// a retry claims it cleanly rather than two callers executing.
		{"reaped in the gap", nil, "", false, true},
		// Our own committed write, seen only by the SDK's retry. We hold the
		// claim — reporting in-flight here wedges the op until the TTL.
		{"our own lost acknowledgement", rec(idemPending, mine, nil), "", false, false},
		{"another caller in flight", rec(idemPending, "THEIRS", nil), "", false, true},
		{"already done", rec(idemDone, "THEIRS", []byte(`{"ok":true}`)), `{"ok":true}`, true, true},
		// Done with no recorded body must stay done, not degrade to in-flight.
		{"done with an empty response", rec(idemDone, "THEIRS", nil), "", true, true},
	}
	for _, tc := range tests {
		resp, done, found := idemOutcome(tc.item, mine)
		if string(resp) != tc.wantResp || done != tc.wantDone || found != tc.wantFound {
			t.Errorf("%s: resp=%q done=%v found=%v, want resp=%q done=%v found=%v",
				tc.name, resp, done, found, tc.wantResp, tc.wantDone, tc.wantFound)
		}
	}
}

// The DynamoDB half of the idempotency pair. Every assertion here has a twin
// in internal/store/idem_test.go; Task 9 folds both into the shared
// conformance suite, which is why they are written to the store.API surface
// and never reach into either backend's storage.

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

// TestIdemBeginIsAtomicUnderConcurrency is the test the whole design exists
// for: N callers race for one key and exactly one may execute the op. On this
// backend the arbiter is the conditional PutItem, and DynamoDB Local evaluates
// conditions serially per item, so this is a genuine race and not a
// same-process lock in disguise.
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

// TestIdemCompleteUnknownKeyIsNoOp is the DynamoDB-specific hazard: UpdateItem
// is an upsert, so an unguarded IdemComplete would CREATE a 'done' record for
// a key nobody claimed, and the next caller would be told its op had already
// run. SQLite's UPDATE matches no rows; this must too.
func TestIdemCompleteUnknownKeyIsNoOp(t *testing.T) {
	s := newTestStore(t)
	if err := s.IdemComplete("never-claimed", []byte("x")); err != nil {
		t.Fatalf("IdemComplete on an unknown key: %v", err)
	}
	if _, _, found, err := s.IdemBegin("never-claimed"); err != nil || found {
		t.Fatalf("after a no-op complete the key must still be claimable: found=%v err=%v", found, err)
	}
}

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
