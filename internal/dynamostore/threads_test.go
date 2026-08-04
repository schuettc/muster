package dynamostore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/schuettc/muster/internal/clock"
	"github.com/schuettc/muster/internal/store"
)

// --- pure tests (no endpoint; these RUN in `just verify`) --------------------

// TestEffectiveIntent pins the Go translation of the SQL effectiveIntent CASE
// (internal/store/threads.go): a task with no explicit intent is
// action-requested, everything else is its stored value.
func TestEffectiveIntent(t *testing.T) {
	tests := []struct {
		kind, intent, want string
	}{
		{"task", "", store.IntentAction},
		{"task", store.IntentFYI, store.IntentFYI},
		{"task", store.IntentReply, store.IntentReply},
		{"message", "", ""},
		{"message", store.IntentFYI, store.IntentFYI},
		{"message", store.IntentAction, store.IntentAction},
	}
	for _, tc := range tests {
		if got := effectiveIntent(tc.kind, tc.intent); got != tc.want {
			t.Errorf("effectiveIntent(%q, %q) = %q, want %q", tc.kind, tc.intent, got, tc.want)
		}
	}
}

func TestValidIntent(t *testing.T) {
	for _, ok := range []string{"", store.IntentFYI, store.IntentReply, store.IntentAction} {
		if !validIntent(ok) {
			t.Errorf("validIntent(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"urgent", "ACTION-REQUESTED", "reply"} {
		if validIntent(bad) {
			t.Errorf("validIntent(%q) = true, want false", bad)
		}
	}
}

func TestClampThreadsLimit(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 100}, {-5, 100}, {1, 1}, {500, 500}, {501, 500}, {10000, 500},
	}
	for _, c := range cases {
		if got := clampThreadsLimit(c.in); got != c.want {
			t.Errorf("clampThreadsLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestSortThreadsRecent pins the ORDER BY updated_at DESC, id DESC that both
// Threads() and Inbox() apply — DynamoDB cannot order on a non-key attribute,
// so this happens in memory and a bug here is silent.
func TestSortThreadsRecent(t *testing.T) {
	in := []store.Thread{
		{ID: 1, UpdatedAt: 100},
		{ID: 3, UpdatedAt: 100},
		{ID: 2, UpdatedAt: 300},
		{ID: 4, UpdatedAt: 50},
	}
	sortThreadsRecent(in)
	var ids []int64
	for _, th := range in {
		ids = append(ids, th.ID)
	}
	if want := []int64{2, 3, 1, 4}; !slices.Equal(ids, want) {
		t.Fatalf("order = %v, want %v (updated_at DESC, then id DESC)", ids, want)
	}
}

// TestUnreadByThread is the pure core of the unread math: entries strictly
// after the watermark, on a concerning thread, written by someone the caller
// is not. An agent's own reply must never re-flag its own inbox.
func TestUnreadByThread(t *testing.T) {
	concerning := map[int64]bool{10: true, 11: true}
	entries := []store.Entry{
		{ID: 1, ThreadID: 10, FromAgent: "peer"}, // at/below watermark
		{ID: 5, ThreadID: 10, FromAgent: "peer"}, // counts
		{ID: 6, ThreadID: 10, FromAgent: "me"},   // own write, never counts
		{ID: 7, ThreadID: 11, FromAgent: "peer"}, // counts
		{ID: 8, ThreadID: 11, FromAgent: "peer"}, // counts
		{ID: 9, ThreadID: 12, FromAgent: "peer"}, // thread does not concern me
		{ID: 4, ThreadID: 11, FromAgent: "peer"}, // exactly at watermark, excluded
	}
	got := unreadByThread(entries, concerning, map[string]bool{"me": true}, 4)
	want := map[int64]int{10: 1, 11: 2}
	if len(got) != len(want) {
		t.Fatalf("unreadByThread = %v, want %v", got, want)
	}
	for id, n := range want {
		if got[id] != n {
			t.Errorf("thread %d unread = %d, want %d", id, got[id], n)
		}
	}
}

// TestUnreadByThreadExcludesEverySessionAlias covers SessionUnread's actor
// exclusion: a session's own writes under EITHER alias never make its own
// threads unread.
func TestUnreadByThreadExcludesEverySessionAlias(t *testing.T) {
	entries := []store.Entry{
		{ID: 2, ThreadID: 1, FromAgent: "a1"},
		{ID: 3, ThreadID: 1, FromAgent: "a2"},
	}
	got := unreadByThread(entries, map[int64]bool{1: true}, map[string]bool{"a1": true, "a2": true}, 0)
	if len(got) != 0 {
		t.Fatalf("unreadByThread = %v, want empty — sibling aliases are one actor", got)
	}
}

// TestEntryItemIndexMapping pins the denormalization the whole design rests
// on: an entry lands in its THREAD partition by id, in its thread's RECIPIENT
// partition on gsi1 (which is what makes unread a bounded query), and in the
// global ENTRIES partition on gsi2 (which is what Task 14's device poller
// reads).
func TestEntryItemIndexMapping(t *testing.T) {
	item := entryItem(7, 42, "backend", "hello", "claimed", 1234, rcpt("agent", "rev1"))
	checks := map[string]string{
		"pk":     "THREAD#7",
		"gsi1pk": "RCPT#agent#rev1",
		"gsi2pk": entriesPartition,
	}
	for name, want := range checks {
		if got := strAttr(item, name); got != want {
			t.Errorf("entry %s = %q, want %q", name, got, want)
		}
	}
	nums := map[string]int64{"sk": 42, "gsi1sk": 42, "gsi2sk": 42, "id": 42, "thread_id": 7, "created_at": 1234}
	for name, want := range nums {
		if got := numAttr(item, name); got != want {
			t.Errorf("entry %s = %d, want %d", name, got, want)
		}
	}
	if got := strAttr(item, "status_change"); got != "claimed" {
		t.Errorf("entry status_change = %q, want claimed", got)
	}
}

// TestThreadMetaItemIndexMapping: the metadata item sits at sort key 0 in its
// own partition, at sort key 0 of its recipient's gsi1 partition (so "which
// threads are addressed to me" is one query that reads no entries), and in the
// global THREADS partition on gsi2 (so Threads() is one query, not a scan).
func TestThreadMetaItemIndexMapping(t *testing.T) {
	item := threadMetaItem(store.Thread{
		Kind: "task", FromAgent: "backend", ToKind: "role", ToTarget: "reviewer",
		Subject: "s", Ref: "r", Status: "open", Intent: store.IntentFYI, OriginProject: "muster",
	}, 9, 3, 500)
	if got := strAttr(item, "pk"); got != "THREAD#9" {
		t.Errorf("pk = %q, want THREAD#9", got)
	}
	if got := numAttr(item, "sk"); got != metaSK {
		t.Errorf("sk = %d, want %d", got, metaSK)
	}
	if got := strAttr(item, "gsi1pk"); got != "RCPT#role#reviewer" {
		t.Errorf("gsi1pk = %q, want RCPT#role#reviewer", got)
	}
	if got := numAttr(item, "gsi1sk"); got != metaSK {
		t.Errorf("gsi1sk = %d, want %d — metadata must sort before every entry", got, metaSK)
	}
	if got := strAttr(item, "gsi2pk"); got != threadsPartition {
		t.Errorf("gsi2pk = %q, want %q", got, threadsPartition)
	}
	if got := numAttr(item, "gsi2sk"); got != 9 {
		t.Errorf("gsi2sk = %d, want 9", got)
	}
	// The RAW intent is stored; effectiveIntent is applied on read.
	if got := strAttr(item, "intent"); got != store.IntentFYI {
		t.Errorf("stored intent = %q, want %q", got, store.IntentFYI)
	}
	if got := numAttr(item, "entry_count"); got != 1 {
		t.Errorf("entry_count = %d, want 1 — a thread is born with its first entry", got)
	}
	if got := numAttr(item, "last_entry_id"); got != 3 {
		t.Errorf("last_entry_id = %d, want 3", got)
	}
}

// TestItemToThreadAppliesEffectiveIntentAndLeavesAnnotationsZero: every read
// surface goes through itemToThread, so the effective-intent rule cannot be
// forgotten on one of them — and the query-time-only annotation fields stay
// zero until a caller explicitly asks for them (GetThread must not set them).
func TestItemToThreadAppliesEffectiveIntentAndLeavesAnnotationsZero(t *testing.T) {
	item := threadMetaItem(store.Thread{Kind: "task", FromAgent: "a", ToKind: "broadcast"}, 1, 1, 10)
	got := itemToThread(item)
	if got.Intent != store.IntentAction {
		t.Errorf("Intent = %q, want %q — a stored-'' task reads as action-requested", got.Intent, store.IntentAction)
	}
	if got.LastFrom != "" || got.LastAt != 0 || got.EntryCount != 0 || got.Unread != 0 {
		t.Errorf("itemToThread populated query-time-only fields: %+v", got)
	}
}

// TestTransactionCancellationClassifiers: a cancelled transaction carries its
// reason in a per-item list, not in the error type, so the two outcomes
// AppendEntry must tell apart — "a guard rejected me" and "someone else was
// mid-write" — both arrive as one TransactionCanceledException. DynamoDB Local
// never emits TransactionConflict, so this is the only place that behaviour is
// pinned at all.
func TestTransactionCancellationClassifiers(t *testing.T) {
	cancelled := func(codes ...string) error {
		reasons := make([]types.CancellationReason, 0, len(codes))
		for _, c := range codes {
			reasons = append(reasons, types.CancellationReason{Code: aws.String(c)})
		}
		return &types.TransactionCanceledException{CancellationReasons: reasons}
	}
	tests := []struct {
		name                  string
		err                   error
		wantCond, wantConflic bool
	}{
		{"condition failed", cancelled("None", "ConditionalCheckFailed"), true, false},
		{"conflict", cancelled("TransactionConflict", "None"), false, true},
		{"unrelated cancellation", cancelled("ThrottlingError"), false, false},
		{"not a transaction error", errors.New("boom"), false, false},
		{"nil", nil, false, false},
	}
	for _, tc := range tests {
		if got := isTransactionConditionFailed(tc.err); got != tc.wantCond {
			t.Errorf("%s: isTransactionConditionFailed = %v, want %v", tc.name, got, tc.wantCond)
		}
		if got := isTransactionConflict(tc.err); got != tc.wantConflic {
			t.Errorf("%s: isTransactionConflict = %v, want %v", tc.name, got, tc.wantConflic)
		}
	}
}

// TestRequestTokenIsStableUniqueAndShortEnough pins the properties a
// ClientRequestToken has to have, none of which fails visibly if it breaks.
//
// STABLE for one attempt is the whole point: without it the SDK's own retry of
// a committed-but-unacknowledged transaction re-evaluates the guards, and a
// claim's winner is told it lost. DISTINCT whenever anything moves is the other
// half, and it has two failure modes of opposite character — DynamoDB answers a
// re-used token carrying DIFFERENT content with IdempotentParameterMismatch
// (loud), and one carrying the SAME content with the original result, silently
// dropping the write. SHORT because the field caps at 36 characters, which is
// why this is a hash and not a concatenation.
func TestRequestTokenIsStableUniqueAndShortEnough(t *testing.T) {
	const maxLen = 36
	// The formats below are the live call sites, writer id first. If one of
	// them changes, change it here too.
	const task = "task|%s|%d|%d|%d|%t"

	// The same inputs must always produce the same token — that is what makes
	// computing it once per call, rather than per physical request, the whole
	// protection.
	a := aws.ToString(requestToken(task, "w1", 7, 42, 0, true))
	if b := aws.ToString(requestToken(task, "w1", 7, 42, 0, true)); a != b {
		t.Fatalf("token is not deterministic: %q then %q", a, b)
	}

	seen := map[string]string{}
	add := func(label string, tok *string) {
		t.Helper()
		s := aws.ToString(tok)
		if s == "" {
			t.Fatalf("%s: empty token", label)
		}
		if len(s) > maxLen {
			t.Fatalf("%s: token %q is %d chars, over DynamoDB's %d limit", label, s, len(s), maxLen)
		}
		if prev, dup := seen[s]; dup {
			t.Fatalf("%s and %s share token %q — one of them would be swallowed as a duplicate",
				prev, label, s)
		}
		seen[s] = label
	}
	add("claim: writer w1, thread 7, entry 42, attempt 0, advancing",
		requestToken(task, "w1", 7, 42, 0, true))
	add("same attempt with the trio conceded — a DIFFERENT expression",
		requestToken(task, "w1", 7, 42, 0, false))
	add("same shape, next attempt",
		requestToken(task, "w1", 7, 42, 1, true))
	add("different entry id",
		requestToken(task, "w1", 7, 43, 0, true))
	add("different thread",
		requestToken(task, "w1", 8, 42, 0, true))
	// The namespace that stops one Store's ids colliding with another's. The
	// idempotency window is neither table-scoped nor cleared by DeleteTable, so
	// two Stores that both allocate thread 1 / entry 1 collide without it —
	// which is exactly what the endpoint suite did before writer was added.
	add("another writer, identical ids",
		requestToken(task, "w2", 7, 42, 0, true))
	add("an append, not a task write",
		requestToken("append|%s|%d|%d|%d|%t", "w1", 7, 42, 0, true))
	add("a thread creation",
		requestToken("create-thread|%s|%d|%d", "w1", 7, 42))
}

// TestOpenGivesEachStoreItsOwnWriterID: the writer id namespaces every
// ClientRequestToken this Store issues, so two Stores sharing one would
// reintroduce the collision it exists to prevent.
func TestOpenGivesEachStoreItsOwnWriterID(t *testing.T) {
	a := newTestStore(t)
	b, err := Open(context.Background(), testTableName(t)+"-b")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := b.DropTable(context.Background()); err != nil {
			t.Errorf("DropTable: %v", err)
		}
	})
	if a.writer == "" {
		t.Fatal("Open left writer empty; every token would collapse into one namespace")
	}
	if a.writer == b.writer {
		t.Fatalf("two Stores share writer %q", a.writer)
	}
}

func TestAppendBackoffGrowsAndIsBounded(t *testing.T) {
	prev := time.Duration(0)
	for attempt := range appendMaxRetries {
		got := appendBackoff(attempt)
		if got <= prev {
			t.Fatalf("appendBackoff(%d) = %v, not greater than %v", attempt, got, prev)
		}
		prev = got
	}
	if prev > 100*time.Millisecond {
		t.Fatalf("longest backoff = %v; a conflict resolves in milliseconds, this is a hang", prev)
	}
}

// TestCreateThreadRejectsInvalidIntent pins the validIntent gate. That gate
// fires BEFORE CreateThread makes any client call, so this needs no endpoint
// and no table — it runs on every developer machine and in the normal
// `just verify` gate, which is worth more than the same assertion behind a
// skip. (The accepted values are exercised end-to-end by
// TestCreateThreadAcceptsEveryValidIntent.)
func TestCreateThreadRejectsInvalidIntent(t *testing.T) {
	for _, bad := range []string{"urgent", "ACTION-REQUESTED", "reply"} {
		// A zero Store has a nil client: reaching one would panic, which is
		// itself the assertion that the gate short-circuits first.
		if _, err := (&Store{}).CreateThread(store.Thread{
			Kind: "message", FromAgent: "a", ToKind: "broadcast", Intent: bad,
		}, "body"); err == nil {
			t.Errorf("intent %q should be rejected", bad)
		}
	}
}

// TestSessionUnreadEmptyTupleNeverGroups: an empty socket path or session id is
// never a group. The guard returns before any client call, so — like the intent
// gate — this is pinned with a zero Store and no endpoint; a nil client would
// panic if the short-circuit ever regressed.
func TestSessionUnreadEmptyTupleNeverGroups(t *testing.T) {
	for _, tc := range []struct{ sock, sess string }{{"", ""}, {"", "$1"}, {"/s", ""}} {
		total, action, err := (&Store{}).SessionUnread("", tc.sock, tc.sess)
		if err != nil {
			t.Fatalf("SessionUnread(%q,%q): %v", tc.sock, tc.sess, err)
		}
		if total != 0 || action != 0 {
			t.Fatalf("SessionUnread(%q,%q) = %d,%d, want 0,0", tc.sock, tc.sess, total, action)
		}
	}
}

// --- endpoint-backed tests (SKIP without MUSTER_DDB_ENDPOINT) ---------------

func TestGetThreadUnknownIDIsNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.GetThread(999999); !errors.Is(err, store.ErrThreadNotFound) {
		t.Fatalf("GetThread on unknown id = %v, want ErrThreadNotFound", err)
	}
}

// TestAppendEntryOnMissingThreadReturnsErrThreadNotFoundAndNoOrphan mirrors
// the SQLite transaction guarantee: the entry must not survive the failure.
func TestAppendEntryOnMissingThreadReturnsErrThreadNotFoundAndNoOrphan(t *testing.T) {
	s := newTestStore(t)
	const missing = int64(999999)
	if _, err := s.AppendEntry(missing, "backend", "hello", ""); !errors.Is(err, store.ErrThreadNotFound) {
		t.Fatalf("AppendEntry on missing thread = %v, want ErrThreadNotFound", err)
	}
	if maxID := lastEntryLogID(t, s); maxID != 0 {
		t.Fatalf("an orphan entry was written (max entry id = %d)", maxID)
	}
}

// lastEntryLogID is the highest entry id in gsi2's global entry log, read the
// way Task 14's device poller will read it. Tests use it as ground truth for
// "what has actually been WRITTEN" — production code deliberately has no such
// helper, because a global maximum is not a safe read watermark for any one
// alias (see MarkRead).
func lastEntryLogID(t *testing.T, s *Store) int64 {
	t.Helper()
	out, err := s.c.Query(t.Context(), &dynamodb.QueryInput{
		TableName:                 aws.String(s.table),
		IndexName:                 aws.String(gsi2Name),
		KeyConditionExpression:    aws.String("gsi2pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": attrS(entriesPartition)},
		ScanIndexForward:          aws.Bool(false),
		Limit:                     aws.Int32(1),
	})
	if err != nil {
		t.Fatalf("query entry log tail: %v", err)
	}
	if len(out.Items) == 0 {
		return 0
	}
	return numAttr(out.Items[0], "id")
}

// TestAppendTransactSurvivesARepeatedSend is the entry path's half of the
// ClientRequestToken. The SDK's standard retryer re-sends a transaction whose
// response was lost — same arguments, so the same token — and an un-tokenized
// append applies its `ADD #entry_count :one` twice, or trips its own
// forward-only guard and reports a failure for an entry that is already
// durably written.
func TestAppendTransactSurvivesARepeatedSend(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	id, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a", ToKind: "agent", ToTarget: "b",
	}, "first")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	const entryID = 400
	entry := entryItem(id, entryID, "a", "again", "", 5000, rcpt("agent", "b"))
	for i := range 2 {
		if err := s.appendTransact(ctx, entry, id, 5000, entryID, 0, "a", true); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	meta, err := s.threadMeta(ctx, id)
	if err != nil {
		t.Fatalf("threadMeta: %v", err)
	}
	if got := numAttr(meta, "entry_count"); got != 2 {
		t.Fatalf("entry_count = %d, want 2 — the re-send was applied a second time", got)
	}
}

// TestAppendTransactGuardIsForwardOnly is the DETERMINISTIC check on the
// forward-only guard, driving appendTransact directly so the out-of-order
// commit is forced rather than hoped for. Its companion
// TestConcurrentAppendsKeepMaxEntryAsLast races 8 real appends, but if those
// happen to commit in id order the guard never fires — that test passes even
// with the condition at appendTransact deleted. This one does not: the second
// call is a strictly LOWER id, so it must be refused, and the thread's last
// entry must remain the higher one.
func TestAppendTransactGuardIsForwardOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	id, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a", ToKind: "agent", ToTarget: "b",
	}, "first")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := clock.NowMillis()
	recipient := rcpt("agent", "b")

	// The higher id commits first and claims the last-entry trio.
	high := entryItem(id, 7, "hi", "seven", "", now, recipient)
	if err := s.appendTransact(ctx, high, id, now, 7, 0, "hi", true); err != nil {
		t.Fatalf("appendTransact(entryID=7): %v", err)
	}
	meta, err := s.threadMeta(ctx, id)
	if err != nil {
		t.Fatalf("threadMeta: %v", err)
	}
	if got := numAttr(meta, "last_entry_id"); got != 7 {
		t.Fatalf("last_entry_id = %d, want 7", got)
	}

	// A lower id committing afterwards must be REFUSED, not silently recorded
	// as the thread's last entry — that mis-pick is exactly what the SQLite
	// MAX(entries.id) rule forbids.
	low := entryItem(id, 5, "lo", "five", "", now+1, recipient)
	err = s.appendTransact(ctx, low, id, now+1, 5, 0, "lo", true)
	if err == nil {
		t.Fatal("appendTransact(entryID=5) succeeded; the forward-only guard did not fire")
	}
	if !isTransactionConditionFailed(err) {
		t.Fatalf("appendTransact(entryID=5) failed with %v, want a ConditionalCheckFailed cancellation", err)
	}

	meta, err = s.threadMeta(ctx, id)
	if err != nil {
		t.Fatalf("threadMeta after refusal: %v", err)
	}
	if got := numAttr(meta, "last_entry_id"); got != 7 {
		t.Fatalf("last_entry_id = %d after a lower-id write, want 7", got)
	}
	if got := strAttr(meta, "last_from"); got != "hi" {
		t.Fatalf("last_from = %q after a lower-id write, want %q", got, "hi")
	}
	// The transaction is atomic: a refused guard writes NO entry and does not
	// bump the count either.
	if got := numAttr(meta, "entry_count"); got != 2 {
		t.Fatalf("entry_count = %d, want 2 — a refused append must write nothing", got)
	}
	_, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("thread has %d entries, want 2 (the opener and id 7)", len(entries))
	}
}

// TestConcurrentAppendsKeepMaxEntryAsLast is the durability check on the
// MAX(entries.id) rule. Entry ids are allocated before the write, so
// concurrent appends can commit out of order; the thread's recorded last entry
// must still be the HIGHEST id, never whichever write landed last, and no
// append may be dropped.
func TestConcurrentAppendsKeepMaxEntryAsLast(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a", ToKind: "broadcast",
	}, "first")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.AppendEntry(id, fmt.Sprintf("peer-%d", i), "body", "")
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent AppendEntry %d: %v", i, err)
		}
	}

	_, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(entries) != n+1 {
		t.Fatalf("thread has %d entries, want %d — a concurrent append was dropped", len(entries), n+1)
	}
	threads, err := s.Threads(10)
	if err != nil {
		t.Fatalf("Threads: %v", err)
	}
	if len(threads) != 1 {
		t.Fatalf("Threads returned %d, want 1", len(threads))
	}
	if threads[0].EntryCount != n+1 {
		t.Fatalf("entry_count = %d, want %d", threads[0].EntryCount, n+1)
	}
	// entries came back in sort-key (id) order, so the last one is the max id.
	if want := entries[len(entries)-1].FromAgent; threads[0].LastFrom != want {
		t.Fatalf("last_from = %q, want %q (the highest-id entry, whatever order they committed in)",
			threads[0].LastFrom, want)
	}
}

// TestEntriesLandInTheGlobalEntryLog pins the gsi2 ENTRIES partition Task 14's
// device poller reads: every entry, in id order, one query.
func TestEntriesLandInTheGlobalEntryLog(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a", ToKind: "agent", ToTarget: "b",
	}, "one")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := s.AppendEntry(id, "b", "two", ""); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	// The gsi2 ENTRIES partition, read exactly the way the device poller
	// (Task 14) will: one query, id order, no join.
	items, err := s.queryAll(t.Context(), &dynamodb.QueryInput{
		TableName:                 aws.String(s.table),
		IndexName:                 aws.String(gsi2Name),
		KeyConditionExpression:    aws.String("gsi2pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": attrS(entriesPartition)},
	})
	if err != nil {
		t.Fatalf("query entry log: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("global entry log has %d entries, want 2", len(items))
	}
	if numAttr(items[0], "id") != 1 || numAttr(items[1], "id") != 2 {
		t.Fatalf("entry log out of id order: %d then %d",
			numAttr(items[0], "id"), numAttr(items[1], "id"))
	}
	if maxID := lastEntryLogID(t, s); maxID != 2 {
		t.Fatalf("entry log tail = %d, want 2", maxID)
	}
}

// TestMarkReadIgnoresEntriesOutsideTheAliasesPartitions is the regression test
// for the watermark fix: MarkRead derives its watermark from the alias's OWN
// gsi1 partitions, so traffic between two OTHER agents can no longer bury an
// entry that is still in flight to this one.
//
// The interleave is controlled, not raced: an entry id is allocated and held
// uncommitted (exactly what AppendEntry does across its retry backoff) while a
// higher id commits elsewhere and MarkRead runs in between. No fault injection
// and no timing assumption — it reproduces against DynamoDB Local.
func TestMarkReadIgnoresEntriesOutsideTheAliasesPartitions(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if err := s.RegisterAgent(store.Agent{Alias: "r"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	mine, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "r",
	}, "opener")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := s.MarkRead("r"); err != nil {
		t.Fatalf("MarkRead (drain the opener): %v", err)
	}

	// A reply to r is allocated an id and then stalls before committing.
	lowID, err := s.nextID(ctx, "entry")
	if err != nil {
		t.Fatalf("nextID: %v", err)
	}
	// Meanwhile two agents r has nothing to do with exchange a message, which
	// takes a HIGHER entry id and commits first.
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "x", ToKind: "agent", ToTarget: "y",
	}, "none of r's business"); err != nil {
		t.Fatalf("CreateThread (unrelated): %v", err)
	}
	// r reads its inbox in the gap. The global entry log now holds an id above
	// lowID; r's own partitions do not, so r's watermark must not move.
	if err := s.MarkRead("r"); err != nil {
		t.Fatalf("MarkRead (in the gap): %v", err)
	}

	// The stalled reply finally commits, under the watermark MarkRead wrote.
	now := clock.NowMillis()
	late := entryItem(mine, lowID, "peer", "the late reply", "", now, rcpt("agent", "r"))
	if err := s.appendTransact(ctx, late, mine, now, lowID, 0, "peer", true); err != nil {
		t.Fatalf("appendTransact (the late reply): %v", err)
	}

	n, err := s.UnreadCount("r")
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("UnreadCount = %d, want 1 — an entry to r was buried by unrelated traffic between x and y", n)
	}
}

// TestMarkReadStillOvershootsWithinOnePartition documents the window the fix
// does NOT close. Deriving the watermark from the alias's own partitions
// removes the cross-partition coupling; it does not order two writers racing
// into the SAME recipient partition, because the ids are still allocated
// before the commits. This test is the known limit, written down — if it ever
// starts failing, the remaining window closed and the package comment is stale.
func TestMarkReadStillOvershootsWithinOnePartition(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if err := s.RegisterAgent(store.Agent{Alias: "r"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	mine, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "r",
	}, "opener")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := s.MarkRead("r"); err != nil {
		t.Fatalf("MarkRead (drain the opener): %v", err)
	}

	lowID, err := s.nextID(ctx, "entry")
	if err != nil {
		t.Fatalf("nextID: %v", err)
	}
	// A second writer, addressing the SAME alias, takes a higher id and lands
	// first — same gsi1 partition, so MarkRead sees it.
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "peer2", ToKind: "agent", ToTarget: "r",
	}, "overtakes the stalled one"); err != nil {
		t.Fatalf("CreateThread (overtaker): %v", err)
	}
	if err := s.MarkRead("r"); err != nil {
		t.Fatalf("MarkRead (in the gap): %v", err)
	}

	now := clock.NowMillis()
	late := entryItem(mine, lowID, "peer", "the late reply", "", now, rcpt("agent", "r"))
	if err := s.appendTransact(ctx, late, mine, now, lowID, 0, "peer", true); err != nil {
		t.Fatalf("appendTransact (the late reply): %v", err)
	}

	n, err := s.UnreadCount("r")
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if n != 0 {
		t.Fatalf("UnreadCount = %d, want 0 — this test records the KNOWN same-partition overshoot; "+
			"if it is now 1 the window closed and the package comment must be updated", n)
	}
	// The entry itself is never lost, only its unread signal: it is in the
	// thread, which Inbox still lists.
	_, entries, err := s.GetThread(mine)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(entries) != 2 || entries[1].Body != "the late reply" {
		t.Fatalf("thread entries = %+v, want the opener and the late reply", entries)
	}
}
