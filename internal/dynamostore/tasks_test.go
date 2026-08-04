package dynamostore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/schuettc/muster/internal/store"
)

// newTestTask creates an open task addressed to a role, mirroring the SQLite
// tasks_test.go fixture so the two suites exercise the same shape.
func newTestTask(t *testing.T, s *Store) int64 {
	t.Helper()
	id, err := s.CreateThread(store.Thread{
		Kind: "task", FromAgent: "backend", ToKind: "role", ToTarget: "reviewer",
		Status: "open",
	}, "review please")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return id
}

// --- pure tests (no endpoint; these RUN in `just verify`) --------------------

// TestTransitionTaskRejectsInvalidStatus pins the validation gate. It fires
// BEFORE any DynamoDB call, so a zero Store (nil client — reaching it would
// panic, which is itself the assertion that the gate short-circuits) is enough.
// This runs on every developer machine, unlike the endpoint-backed tests.
func TestTransitionTaskRejectsInvalidStatus(t *testing.T) {
	for _, bad := range []string{"bogus", "CLAIMED", "done", ""} {
		err := (&Store{}).TransitionTask(1, "rev1", bad, "note")
		if err == nil {
			t.Errorf("TransitionTask(%q) = nil, want an invalid-status error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "invalid task status") {
			t.Errorf("TransitionTask(%q) error = %v, want it to name the invalid status", bad, err)
		}
	}
}

// TestTransitionTaskAcceptsEveryTaskState guards against the validation set
// drifting from store.TaskStates. A valid status must get PAST the gate, which
// here means reaching the nil client and panicking — the inverse of the test
// above.
func TestTransitionTaskAcceptsEveryTaskState(t *testing.T) {
	for status := range store.TaskStates {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("TransitionTask(%q) returned instead of reaching the client; "+
						"the validation gate rejected a valid store.TaskStates value", status)
				}
			}()
			_ = (&Store{}).TransitionTask(1, "rev1", status, "note")
		}()
	}
}

// TestTaskUpdateExpressions pins the three guards the update carries, because
// none of them fails loudly if it silently disappears:
//
//   - "status" is a DynamoDB RESERVED WORD, so it must be aliased.
//   - the open guard (#status = :open) is ClaimTask's entire compare-and-swap.
//   - the forward-only last_entry_id guard is the MAX(entries.id) rule.
func TestTaskUpdateExpressions(t *testing.T) {
	const openGuard = "#status = :open"
	const lastGuard = "#last_entry_id < :eid"

	claim := taskWrite{threadID: 7, byAgent: "rev1", newStatus: "claimed", onlyIfOpen: true}
	trans := taskWrite{threadID: 7, byAgent: "rev1", newStatus: "completed", body: "LGTM"}

	tests := []struct {
		name                   string
		w                      taskWrite
		advanceLast            bool
		wantOpen, wantLastGrd  bool
		wantValueOpen, wantEID bool
	}{
		{"claim advancing", claim, true, true, true, true, true},
		{"claim not advancing", claim, false, true, false, true, false},
		{"transition advancing", trans, true, false, true, false, true},
		{"transition not advancing", trans, false, false, false, false, false},
	}
	for _, tc := range tests {
		u := taskUpdate("tbl", tc.w, 1234, 9, tc.advanceLast)
		cond := aws.ToString(u.ConditionExpression)
		upd := aws.ToString(u.UpdateExpression)

		if !strings.Contains(cond, "attribute_exists(pk)") {
			t.Errorf("%s: condition %q must require the thread to exist", tc.name, cond)
		}
		if got := strings.Contains(cond, openGuard); got != tc.wantOpen {
			t.Errorf("%s: condition %q contains %q = %v, want %v", tc.name, cond, openGuard, got, tc.wantOpen)
		}
		if got := strings.Contains(cond, lastGuard); got != tc.wantLastGrd {
			t.Errorf("%s: condition %q contains %q = %v, want %v", tc.name, cond, lastGuard, got, tc.wantLastGrd)
		}
		if got := u.ExpressionAttributeNames["#status"]; got != "status" {
			t.Errorf("%s: #status aliases %q, want \"status\" — it is a reserved word", tc.name, got)
		}
		if _, got := u.ExpressionAttributeValues[":open"]; got != tc.wantValueOpen {
			t.Errorf("%s: :open bound = %v, want %v", tc.name, got, tc.wantValueOpen)
		}
		if _, got := u.ExpressionAttributeValues[":eid"]; got != tc.wantEID {
			t.Errorf("%s: :eid bound = %v, want %v", tc.name, got, tc.wantEID)
		}
		if !strings.Contains(upd, "#status = :status") {
			t.Errorf("%s: update %q must set the new status", tc.name, upd)
		}
		if !strings.Contains(upd, "#updated_at = :now") {
			t.Errorf("%s: update %q must advance updated_at", tc.name, upd)
		}
		if !strings.Contains(upd, "ADD #entry_count :one") {
			t.Errorf("%s: update %q must count the status-change entry", tc.name, upd)
		}
		if got := strings.Contains(upd, "#last_from = :from"); got != tc.advanceLast {
			t.Errorf("%s: update %q advances last_from = %v, want %v", tc.name, upd, got, tc.advanceLast)
		}
	}
}

// TestRunTaskWriteRetryStateMachine drives the retry loop with scripted
// failures, which is the only way to reach two of its branches at all:
// DynamoDB Local never emits a TransactionConflict, and a SECOND consecutive
// condition failure needs an ABA flip of `status` between the cancelled
// transaction and classifyTaskFailure's re-read — a race no endpoint test can
// stage deterministically.
//
// What it pins is that the loop terminates and that a lost claim is never
// retried. There is exactly one concession (advanceLast true → false) and no
// path back, so the condition-failed branch cannot spin.
func TestRunTaskWriteRetryStateMachine(t *testing.T) {
	cancelled := func(codes ...string) error {
		reasons := make([]types.CancellationReason, 0, len(codes))
		for _, c := range codes {
			reasons = append(reasons, types.CancellationReason{Code: aws.String(c)})
		}
		return &types.TransactionCanceledException{CancellationReasons: reasons}
	}
	condFail := cancelled("None", "ConditionalCheckFailed")
	conflict := cancelled("TransactionConflict", "None")
	boom := errors.New("boom")

	claim := taskWrite{threadID: 7, byAgent: "rev1", newStatus: "claimed",
		onlyIfOpen: true, missing: store.ErrNotClaimable}
	trans := taskWrite{threadID: 7, byAgent: "rev1", newStatus: "completed",
		missing: store.ErrThreadNotFound}

	stillEligible := func() (bool, error) { return true, nil }
	lostTheClaim := func() (bool, error) { return false, store.ErrNotClaimable }
	threadGone := func() (bool, error) { return false, store.ErrThreadNotFound }

	// manyConflicts is one more conflict than the loop is allowed to retry.
	manyConflicts := make([]error, appendMaxRetries+1)
	for i := range manyConflicts {
		manyConflicts[i] = conflict
	}

	type call struct {
		attempt     int
		advanceLast bool
	}
	tests := []struct {
		name      string
		w         taskWrite
		script    []error // one per attempt; nil means the write committed
		classify  func() (bool, error)
		wantErr   error // errors.Is target; nil means success
		wantCalls []call
	}{
		{
			name: "commits on the first attempt", w: claim,
			script: []error{nil}, wantCalls: []call{{0, true}},
		},
		{
			// A conflict wrote nothing, so the retry re-competes for the
			// last-entry trio: advanceLast stays true.
			name: "a conflict retries the whole write unchanged", w: claim,
			script: []error{conflict, nil}, wantCalls: []call{{0, true}, {1, true}},
		},
		{
			// The one concession: stop competing for the trio, keep the open
			// guard (taskUpdate carries it on every attempt).
			name: "a condition failure concedes the last-entry trio", w: claim,
			script: []error{condFail, nil}, classify: stillEligible,
			wantCalls: []call{{0, true}, {1, false}},
		},
		{
			// Finding: the branch that stops the concession looping.
			name: "a second condition failure is terminal", w: claim,
			script: []error{condFail, condFail}, classify: stillEligible,
			wantErr: condFail, wantCalls: []call{{0, true}, {1, false}},
		},
		{
			// The load-bearing one. Retrying here would let a second claimant
			// win a task the first already holds.
			name: "a lost claim is never retried", w: claim,
			script: []error{condFail, nil}, classify: lostTheClaim,
			wantErr: store.ErrNotClaimable, wantCalls: []call{{0, true}},
		},
		{
			name: "a vanished thread returns the caller's own sentinel", w: trans,
			script: []error{condFail, nil}, classify: threadGone,
			wantErr: store.ErrThreadNotFound, wantCalls: []call{{0, true}},
		},
		{
			name: "conflicts are bounded", w: claim,
			script: append(manyConflicts, nil), wantErr: conflict,
			wantCalls: []call{{0, true}, {1, true}, {2, true}, {3, true}, {4, true}, {5, true}},
		},
		{
			name: "an unrelated error is not retried", w: claim,
			script: []error{boom, nil}, wantErr: boom, wantCalls: []call{{0, true}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls []call
			err := runTaskWrite(tc.w,
				func(attempt int, advanceLast bool) error {
					calls = append(calls, call{attempt, advanceLast})
					if attempt >= len(tc.script) {
						t.Fatalf("attempt %d ran past the script — the loop did not terminate", attempt)
					}
					return tc.script[attempt]
				},
				func() (bool, error) {
					if tc.classify == nil {
						t.Fatal("classify called on a path that must not re-read the thread")
					}
					return tc.classify()
				})

			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("err = %v, want it to wrap %v", err, tc.wantErr)
			}
			if fmt.Sprint(calls) != fmt.Sprint(tc.wantCalls) {
				t.Fatalf("attempts = %v, want %v", calls, tc.wantCalls)
			}
		})
	}
}

// --- endpoint-backed tests --------------------------------------------------

// TestClaimTaskSucceedsOnceThenFails is the compare-and-swap contract itself,
// ported from internal/store/tasks_test.go's TestClaimTaskIsAtomic.
func TestClaimTaskSucceedsOnceThenFails(t *testing.T) {
	s := newTestStore(t)
	id := newTestTask(t, s)

	if err := s.ClaimTask(id, "rev1"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := s.ClaimTask(id, "rev2"); !errors.Is(err, store.ErrNotClaimable) {
		t.Fatalf("second claim err = %v, want ErrNotClaimable", err)
	}
	th, _, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.Status != "claimed" {
		t.Fatalf("status = %q, want claimed", th.Status)
	}
}

// TestClaimTaskIsAtomicUnderConcurrency is the reason ClaimTask is a
// ConditionExpression and not a read-then-write. There is no
// reserved-concurrency-1 backstop on this backend, so this is the ONLY thing
// stopping two agents from picking up the same work.
func TestClaimTaskIsAtomicUnderConcurrency(t *testing.T) {
	s := newTestStore(t)
	id := newTestTask(t, s)

	const n = 8
	var wins int64
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.ClaimTask(id, fmt.Sprintf("a%d", i))
			errs[i] = err
			if err == nil {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d of %d concurrent claims succeeded, want exactly 1 (errs: %v)", wins, n, errs)
	}
	// Every loser must lose the DOCUMENTED way. An infrastructure error that
	// happened to fail would satisfy the count above while meaning something
	// entirely different to the daemon.
	for i, err := range errs {
		if err != nil && !errors.Is(err, store.ErrNotClaimable) {
			t.Fatalf("claim %d failed with %v, want ErrNotClaimable", i, err)
		}
	}
	// A cancelled transaction writes NOTHING: the opener plus exactly one
	// status-change entry.
	_, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("thread has %d entries, want 2 — a losing claim must write no entry", len(entries))
	}
}

// TestClaimTaskRecordsStatusChangeEntry pins the SQLite INSERT: an empty body,
// status_change "claimed", attributed to the CLAIMANT (not the originator).
func TestClaimTaskRecordsStatusChangeEntry(t *testing.T) {
	s := newTestStore(t)
	id := newTestTask(t, s)

	if err := s.ClaimTask(id, "rev1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	_, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	last := entries[len(entries)-1]
	if last.StatusChange != "claimed" || last.FromAgent != "rev1" || last.Body != "" {
		t.Fatalf("last entry = %+v, want an empty-bodied \"claimed\" entry from rev1", last)
	}
}

// TestClaimTaskOnMissingThreadIsNotClaimable pins a divergence the obvious
// implementation gets wrong. The SQLite UPDATE matches no rows for an unknown
// id and returns ErrNotClaimable — NOT ErrThreadNotFound, which is what a
// metadata read would naturally produce here.
func TestClaimTaskOnMissingThreadIsNotClaimable(t *testing.T) {
	s := newTestStore(t)
	err := s.ClaimTask(999999, "rev1")
	if !errors.Is(err, store.ErrNotClaimable) {
		t.Fatalf("ClaimTask on a missing thread = %v, want ErrNotClaimable", err)
	}
	if errors.Is(err, store.ErrThreadNotFound) {
		t.Fatalf("ClaimTask on a missing thread leaked ErrThreadNotFound: %v", err)
	}
}

// TestClaimTaskOnATerminalTaskIsNotClaimable covers the states a real bus
// spends most of its life in. The open guard is what stops finished work being
// picked back up, and only `claimed` was exercised — a guard written as
// `#status <> :claimed` would have passed every other test in this file.
func TestClaimTaskOnATerminalTaskIsNotClaimable(t *testing.T) {
	for _, terminal := range []string{"completed", "cancelled", "declined"} {
		t.Run(terminal, func(t *testing.T) {
			s := newTestStore(t)
			id := newTestTask(t, s)
			if err := s.TransitionTask(id, "rev1", terminal, "that's a wrap"); err != nil {
				t.Fatalf("transition to %q: %v", terminal, err)
			}
			_, before, err := s.GetThread(id)
			if err != nil {
				t.Fatalf("GetThread: %v", err)
			}

			if err := s.ClaimTask(id, "rev2"); !errors.Is(err, store.ErrNotClaimable) {
				t.Fatalf("claim of a %q task = %v, want ErrNotClaimable", terminal, err)
			}

			th, after, err := s.GetThread(id)
			if err != nil {
				t.Fatalf("GetThread after the refused claim: %v", err)
			}
			if th.Status != terminal {
				t.Fatalf("status = %q, want %q — the refused claim moved it anyway", th.Status, terminal)
			}
			// A cancelled transaction writes NOTHING, entry included.
			if len(after) != len(before) {
				t.Fatalf("thread has %d entries, want %d — a refused claim must write no entry",
					len(after), len(before))
			}
		})
	}
}

// TestClaimTaskAnnotatesLastEntry pins the denormalized last-entry trio, which
// SQLite recomputes from MAX(entries.id) at read time and this backend must
// maintain on write. Without it, a claimed task's inbox row still credits the
// ORIGINATOR's opening entry as the latest activity.
func TestClaimTaskAnnotatesLastEntry(t *testing.T) {
	s := newTestStore(t)
	id := newTestTask(t, s)

	if err := s.ClaimTask(id, "rev1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	threads, err := s.Threads(10)
	if err != nil {
		t.Fatalf("Threads: %v", err)
	}
	var found bool
	for _, th := range threads {
		if th.ID != id {
			continue
		}
		found = true
		if th.LastFrom != "rev1" {
			t.Fatalf("LastFrom = %q, want rev1", th.LastFrom)
		}
		if th.EntryCount != 2 {
			t.Fatalf("EntryCount = %d, want 2", th.EntryCount)
		}
		if th.LastAt == 0 {
			t.Fatal("LastAt = 0; the claim must stamp it")
		}
	}
	if !found {
		t.Fatalf("thread %d missing from Threads()", id)
	}
}

// TestClaimTaskKeepsMaxEntryAsLast is the forward-only guard on the claim path.
// Entry ids are allocated before the write, so a concurrent AppendEntry holding
// a HIGHER id can commit first; the claim must then record its entry without
// dragging the last-entry trio backwards. Reproduced deterministically by
// pre-seeding a high-id entry through appendTransact.
func TestClaimTaskKeepsMaxEntryAsLast(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := newTestTask(t, s)

	meta, err := s.threadMeta(ctx, id)
	if err != nil {
		t.Fatalf("threadMeta: %v", err)
	}
	recipient := rcpt(strAttr(meta, "to_kind"), strAttr(meta, "to_target"))
	const highID = 900
	if err := s.appendTransact(ctx,
		entryItem(id, highID, "peer", "later", "", 5000, recipient),
		id, 5000, highID, 0, "peer", true); err != nil {
		t.Fatalf("seed high-id entry: %v", err)
	}

	if err := s.ClaimTask(id, "rev1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	meta, err = s.threadMeta(ctx, id)
	if err != nil {
		t.Fatalf("threadMeta after claim: %v", err)
	}
	if got := numAttr(meta, "last_entry_id"); got != highID {
		t.Fatalf("last_entry_id = %d after the claim, want %d — the claim dragged it backwards", got, highID)
	}
	if got := strAttr(meta, "last_from"); got != "peer" {
		t.Fatalf("last_from = %q, want peer", got)
	}
	// The claim still happened, and still counted its entry.
	if got := strAttr(meta, "status"); got != "claimed" {
		t.Fatalf("status = %q, want claimed", got)
	}
	if got := numAttr(meta, "entry_count"); got != 3 {
		t.Fatalf("entry_count = %d, want 3", got)
	}
	_, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("thread has %d entries, want 3", len(entries))
	}
}

// TestTaskTransactSurvivesARepeatedSend is the ClientRequestToken, exercised
// the way it actually fires: the SDK's standard retryer retries connection
// resets and 5xx on EVERY operation, so a transaction that commits and then
// fails to acknowledge is re-sent verbatim — same arguments, therefore same
// token. Calling taskTransact twice with identical arguments is that re-send.
//
// Both halves fail loudly without the token, in different ways, and the claim
// one is the reason this matters at all.
func TestTaskTransactSurvivesARepeatedSend(t *testing.T) {
	setup := func(t *testing.T, s *Store, w taskWrite) (context.Context, map[string]types.AttributeValue, taskWrite, int64) {
		t.Helper()
		ctx := context.Background()
		id := newTestTask(t, s)
		meta, err := s.threadMeta(ctx, id)
		if err != nil {
			t.Fatalf("threadMeta: %v", err)
		}
		w.threadID = id
		const entryID = 500
		entry := entryItem(id, entryID, w.byAgent, w.body, w.newStatus, 5000,
			rcpt(strAttr(meta, "to_kind"), strAttr(meta, "to_target")))
		return ctx, entry, w, entryID
	}

	// A transition carries no open guard, but while it is advancing the trio it
	// still carries the forward-only one — and the FIRST send satisfied that by
	// setting last_entry_id to this entry's id. So an un-tokenized re-send is
	// cancelled by a guard the write itself just closed behind it, and a caller
	// is told its durably-written transition failed.
	t.Run("a transition re-send is not a failure", func(t *testing.T) {
		s := newTestStore(t)
		ctx, entry, w, entryID := setup(t, s, taskWrite{
			byAgent: "rev1", body: "done", newStatus: "completed",
			missing: store.ErrThreadNotFound,
		})

		for i := range 2 {
			if err := s.taskTransact(ctx, entry, w, 5000, entryID, 0, true); err != nil {
				t.Fatalf("send %d: %v", i, err)
			}
		}
		meta, err := s.threadMeta(ctx, w.threadID)
		if err != nil {
			t.Fatalf("threadMeta: %v", err)
		}
		if got := numAttr(meta, "entry_count"); got != 2 {
			t.Fatalf("entry_count = %d, want 2 — the re-send was applied a second time", got)
		}
	})

	// With the trio conceded the ONLY condition left is attribute_exists(pk),
	// which a re-send satisfies. Nothing but the token stops the second send
	// from committing, so this isolates the property the other two subtests
	// cannot: `ADD #entry_count :one` is not applied twice. It is also the
	// realistic shape, since a concession is exactly when a lost response is
	// most likely — the writer has already been round-tripping under contention.
	t.Run("a conceded re-send does not double-count the entry", func(t *testing.T) {
		s := newTestStore(t)
		ctx, entry, w, entryID := setup(t, s, taskWrite{
			byAgent: "rev1", body: "done", newStatus: "completed",
			missing: store.ErrThreadNotFound,
		})

		for i := range 2 {
			if err := s.taskTransact(ctx, entry, w, 5000, entryID, 1, false); err != nil {
				t.Fatalf("send %d: %v", i, err)
			}
		}
		meta, err := s.threadMeta(ctx, w.threadID)
		if err != nil {
			t.Fatalf("threadMeta: %v", err)
		}
		if got := numAttr(meta, "entry_count"); got != 2 {
			t.Fatalf("entry_count = %d, want 2 — the re-send's ADD was applied a second time", got)
		}
	})

	// The claim path is the one that hurts. Without a token the re-send
	// re-evaluates `#status = :open`, which the FIRST send just falsified, so
	// the transaction is cancelled and the winner is told it lost — the task
	// then sits claimed by an agent that believes it does not hold it.
	t.Run("a claim's winner is not told it lost", func(t *testing.T) {
		s := newTestStore(t)
		ctx, entry, w, entryID := setup(t, s, taskWrite{
			byAgent: "rev1", newStatus: "claimed",
			onlyIfOpen: true, missing: store.ErrNotClaimable,
		})

		if err := s.taskTransact(ctx, entry, w, 5000, entryID, 0, true); err != nil {
			t.Fatalf("first send: %v", err)
		}
		if err := s.taskTransact(ctx, entry, w, 5000, entryID, 0, true); err != nil {
			t.Fatalf("re-send of a COMMITTED claim = %v, want nil — "+
				"the winner would be handed ErrNotClaimable for a task it holds", err)
		}
		meta, err := s.threadMeta(ctx, w.threadID)
		if err != nil {
			t.Fatalf("threadMeta: %v", err)
		}
		if got := strAttr(meta, "status"); got != "claimed" {
			t.Fatalf("status = %q, want claimed", got)
		}
		if got := numAttr(meta, "entry_count"); got != 2 {
			t.Fatalf("entry_count = %d, want 2", got)
		}
	})
}

// TestTransitionTaskValidatesAndRecords is ported one-for-one from
// internal/store/tasks_test.go.
func TestTransitionTaskValidatesAndRecords(t *testing.T) {
	s := newTestStore(t)
	id := newTestTask(t, s)
	if err := s.ClaimTask(id, "rev1"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if err := s.TransitionTask(id, "rev1", "bogus", ""); err == nil {
		t.Fatal("expected an error for an invalid status")
	}
	if err := s.TransitionTask(id, "rev1", "completed", "LGTM"); err != nil {
		t.Fatalf("valid transition: %v", err)
	}
	th, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.Status != "completed" {
		t.Fatalf("status = %q, want completed", th.Status)
	}
	last := entries[len(entries)-1]
	if last.StatusChange != "completed" || last.Body != "LGTM" || last.FromAgent != "rev1" {
		t.Fatalf("transition not recorded as an entry: %+v", last)
	}
}

// TestTransitionTaskOnMissingThreadReturnsErrThreadNotFound — and note the
// contrast with ClaimTask, whose SQLite counterpart returns ErrNotClaimable for
// the same input.
func TestTransitionTaskOnMissingThreadReturnsErrThreadNotFound(t *testing.T) {
	s := newTestStore(t)
	if err := s.TransitionTask(999999, "rev1", "completed", "LGTM"); !errors.Is(err, store.ErrThreadNotFound) {
		t.Fatalf("TransitionTask on a missing thread = %v, want ErrThreadNotFound", err)
	}
}

// TestTransitionTaskIgnoresCurrentStatus is the deliberate asymmetry with
// ClaimTask: the SQLite UPDATE carries no status predicate, so any state may
// move to any other, including back to open.
func TestTransitionTaskIgnoresCurrentStatus(t *testing.T) {
	s := newTestStore(t)
	id := newTestTask(t, s)

	for _, status := range []string{"claimed", "blocked", "needs_info", "open", "cancelled", "declined", "completed"} {
		if err := s.TransitionTask(id, "rev1", status, "n"); err != nil {
			t.Fatalf("transition to %q: %v", status, err)
		}
		th, _, err := s.GetThread(id)
		if err != nil {
			t.Fatalf("GetThread: %v", err)
		}
		if th.Status != status {
			t.Fatalf("status = %q, want %q", th.Status, status)
		}
	}
}

// TestTransitionTaskBackToOpenIsClaimableAgain closes the loop between the two
// methods: the claim guard reads the LIVE status, so re-opening a task makes it
// claimable again rather than permanently burnt.
func TestTransitionTaskBackToOpenIsClaimableAgain(t *testing.T) {
	s := newTestStore(t)
	id := newTestTask(t, s)

	if err := s.ClaimTask(id, "rev1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.TransitionTask(id, "rev1", "open", "handing it back"); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := s.ClaimTask(id, "rev2"); err != nil {
		t.Fatalf("re-claim after reopen: %v", err)
	}
	th, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.Status != "claimed" {
		t.Fatalf("status = %q, want claimed", th.Status)
	}
	if len(entries) != 4 {
		t.Fatalf("thread has %d entries, want 4 (open, claim, reopen, re-claim)", len(entries))
	}
}

// TestTaskEntriesLandInTheRecipientPartition is the unread-math consequence: a
// status-change entry is an entry like any other, so it must carry the thread's
// recipient denormalization or the assignee never sees the state change.
func TestTaskEntriesLandInTheRecipientPartition(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "rev1", Role: "reviewer"}); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if err := s.RegisterAgent(store.Agent{Alias: "watcher", Role: "reviewer"}); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	id := newTestTask(t, s)
	if err := s.MarkRead("watcher"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if n, err := s.UnreadCount("watcher"); err != nil || n != 0 {
		t.Fatalf("UnreadCount after drain = %d, %v; want 0", n, err)
	}

	if err := s.ClaimTask(id, "rev1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if n, err := s.UnreadCount("watcher"); err != nil || n != 1 {
		t.Fatalf("UnreadCount after a peer's claim = %d, %v; want 1 — "+
			"the status-change entry missed the recipient partition", n, err)
	}
}
