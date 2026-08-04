package dynamostore

import (
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
		total, action, err := (&Store{}).SessionUnread(tc.sock, tc.sess)
		if err != nil {
			t.Fatalf("SessionUnread(%q,%q): %v", tc.sock, tc.sess, err)
		}
		if total != 0 || action != 0 {
			t.Fatalf("SessionUnread(%q,%q) = %d,%d, want 0,0", tc.sock, tc.sess, total, action)
		}
	}
}

// --- endpoint-backed tests (SKIP without MUSTER_DDB_ENDPOINT) ---------------

func TestCreateThreadAndGetThread(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
		Subject: "hi", Ref: "repo=x", OriginProject: "muster",
	}, "first body")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	th, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.ID != id || th.Kind != "message" || th.FromAgent != "a1" || th.ToKind != "agent" ||
		th.ToTarget != "a2" || th.Subject != "hi" || th.Ref != "repo=x" || th.OriginProject != "muster" {
		t.Fatalf("thread round trip wrong: %+v", th)
	}
	if th.CreatedAt == 0 || th.UpdatedAt == 0 {
		t.Fatalf("timestamps not stamped: %+v", th)
	}
	if len(entries) != 1 || entries[0].Body != "first body" || entries[0].FromAgent != "a1" ||
		entries[0].ThreadID != id {
		t.Fatalf("entries = %+v, want one with 'first body'", entries)
	}
	// LastFrom/LastAt/EntryCount/Unread are query-time only: GetThread and
	// CreateThread leave them zero.
	if th.LastFrom != "" || th.LastAt != 0 || th.EntryCount != 0 || th.Unread != 0 {
		t.Fatalf("GetThread must leave query-time-only fields zero: %+v", th)
	}
}

// TestCreateThreadAcceptsEveryValidIntent is the endpoint-backed half of the
// intent contract: each accepted value actually round-trips a write.
func TestCreateThreadAcceptsEveryValidIntent(t *testing.T) {
	s := newTestStore(t)
	for _, ok := range []string{"", store.IntentFYI, store.IntentReply, store.IntentAction} {
		if _, err := s.CreateThread(store.Thread{
			Kind: "message", FromAgent: "a", ToKind: "broadcast", Intent: ok,
		}, "body"); err != nil {
			t.Fatalf("intent %q should be valid: %v", ok, err)
		}
	}
}

func TestEntriesReturnInIDOrder(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
	}, "one")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	for _, body := range []string{"two", "three"} {
		if _, err := s.AppendEntry(id, "a2", body, ""); err != nil {
			t.Fatalf("AppendEntry %s: %v", body, err)
		}
	}
	_, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	var bodies []string
	for _, e := range entries {
		bodies = append(bodies, e.Body)
	}
	if want := []string{"one", "two", "three"}; !slices.Equal(bodies, want) {
		t.Fatalf("bodies = %v, want %v", bodies, want)
	}
}

func TestAppendEntryAdvancesUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(store.Thread{
		Kind: "task", FromAgent: "backend", ToKind: "role", ToTarget: "reviewer", Status: "open",
	}, "please review")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	before, _, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if _, err := s.AppendEntry(id, "reviewer", "looks good", "claimed"); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	after, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if after.UpdatedAt < before.UpdatedAt {
		t.Fatalf("updated_at went backwards: %d -> %d", before.UpdatedAt, after.UpdatedAt)
	}
	if len(entries) != 2 || entries[1].StatusChange != "claimed" {
		t.Fatalf("entries = %+v, want the second carrying status_change=claimed", entries)
	}
	if after.Status != "open" {
		t.Fatalf("AppendEntry must not touch status, got %q", after.Status)
	}
}

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

func TestUnreadCountRespectsWatermark(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "a2"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
	}, "unread one"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	n, err := s.UnreadCount("a2")
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("UnreadCount = %d, want 1", n)
	}
	if err := s.MarkRead("a2"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	n, err = s.UnreadCount("a2")
	if err != nil {
		t.Fatalf("UnreadCount after MarkRead: %v", err)
	}
	if n != 0 {
		t.Fatalf("UnreadCount after MarkRead = %d, want 0", n)
	}
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
	}, "unread two"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if n, _ := s.UnreadCount("a2"); n != 1 {
		t.Fatalf("UnreadCount after a new message = %d, want 1", n)
	}
}

func TestBroadcastCountsAsUnread(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "a2"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "broadcast",
	}, "all hands"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	n, err := s.UnreadCount("a2")
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("broadcast unread = %d, want 1", n)
	}
}

func TestMarkReadUnknownAliasIsNoOp(t *testing.T) {
	s := newTestStore(t)
	if err := s.MarkRead("nobody"); err != nil {
		t.Fatalf("MarkRead unknown alias: %v", err)
	}
	agents, err := s.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("MarkRead created %d phantom row(s): %+v", len(agents), agents)
	}
}

// TestInboxMatchesAgentRoleBroadcastAndOriginated is threadConcerns in full —
// all four arms. Inbox and UnreadCount must agree on it, so both are asserted
// here against the same fixture.
func TestInboxMatchesAgentRoleBroadcastAndOriginated(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "rev1", Role: "reviewer"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	mk := func(from, toKind, toTarget string) {
		t.Helper()
		if _, err := s.CreateThread(store.Thread{
			Kind: "message", FromAgent: from, ToKind: toKind, ToTarget: toTarget,
		}, "hi"); err != nil {
			t.Fatalf("CreateThread: %v", err)
		}
	}
	mk("backend", "agent", "rev1")        // direct
	mk("backend", "role", "reviewer")     // by role
	mk("backend", "broadcast", "")        // to everyone
	mk("rev1", "agent", "someone-else")   // originated by rev1
	mk("backend", "agent", "someoneelse") // not for rev1
	mk("backend", "role", "producer")     // another role

	in, err := s.Inbox("rev1")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(in) != 4 {
		t.Fatalf("Inbox returned %d threads, want 4 (direct, role, broadcast, originated): %+v", len(in), in)
	}
	// Same predicate: the three peer-written threads are unread; rev1's own
	// send is not.
	n, err := s.UnreadCount("rev1")
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if n != 3 {
		t.Fatalf("UnreadCount = %d, want 3 — Inbox and UnreadCount must use the same predicate", n)
	}
}

// TestInboxIncludesOriginatedThreadsForUnregisteredAlias: the originator arm
// of threadConcerns does not depend on the alias being registered.
func TestInboxIncludesOriginatedThreadsForUnregisteredAlias(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "req"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	in, err := s.Inbox("web")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(in) != 1 {
		t.Fatalf("originator's inbox should include the thread it started, got %d", len(in))
	}
}

// TestUnreadCountOriginatorSeesPeerReply is the originator-blindness
// regression: a reply on a thread you started counts as unread for you.
func TestUnreadCountOriginatorSeesPeerReply(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "web"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	id, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "req")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if n, err := s.UnreadCount("web"); err != nil || n != 0 {
		t.Fatalf("own send must not count as unread, got %d (%v)", n, err)
	}
	if _, err := s.AppendEntry(id, "api", "done", ""); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if n, err := s.UnreadCount("web"); err != nil || n != 1 {
		t.Fatalf("peer reply on originated thread = %d unread (%v), want 1", n, err)
	}
	if err := s.MarkRead("web"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if n, _ := s.UnreadCount("web"); n != 0 {
		t.Fatalf("unread after MarkRead = %d, want 0", n)
	}
}

// TestUnreadCountIgnoresOwnReply: replying on a thread addressed to you must
// not re-flag your own inbox.
func TestUnreadCountIgnoresOwnReply(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "api"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	id, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "req")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := s.MarkRead("api"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if _, err := s.AppendEntry(id, "api", "done", ""); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if n, err := s.UnreadCount("api"); err != nil || n != 0 {
		t.Fatalf("own reply re-flagged own inbox: %d unread (%v), want 0", n, err)
	}
}

// TestInboxAnnotatesLastFromAndUnread is the production defect reproduction:
// an agent must be able to tell "a peer replied on my thread" from "my own
// last send" without a get_thread drill-in.
func TestInboxAnnotatesLastFromAndUnread(t *testing.T) {
	s := newTestStore(t)
	for _, alias := range []string{"web", "api"} {
		if err := s.RegisterAgent(store.Agent{Alias: alias}); err != nil {
			t.Fatalf("register %s: %v", alias, err)
		}
	}
	repliedID, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "req")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := s.AppendEntry(repliedID, "api", "done", ""); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	ownLastID, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "another req")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	in, err := s.Inbox("web")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	byID := map[int64]store.Thread{}
	for _, th := range in {
		byID[th.ID] = th
	}
	replied := byID[repliedID]
	if replied.LastFrom != "api" || replied.Unread != 1 || replied.EntryCount != 2 {
		t.Fatalf("replied thread = {LastFrom:%q Unread:%d EntryCount:%d}, want {api 1 2}",
			replied.LastFrom, replied.Unread, replied.EntryCount)
	}
	if replied.LastAt == 0 {
		t.Fatal("replied thread LastAt not populated")
	}
	ownLast := byID[ownLastID]
	if ownLast.LastFrom != "web" || ownLast.Unread != 0 || ownLast.EntryCount != 1 {
		t.Fatalf("own-last thread = {LastFrom:%q Unread:%d EntryCount:%d}, want {web 0 1}",
			ownLast.LastFrom, ownLast.Unread, ownLast.EntryCount)
	}
}

func TestInboxUnreadDropsAfterMarkRead(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "web"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	id, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "req")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := s.AppendEntry(id, "api", "done", ""); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	before, err := s.Inbox("web")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(before) != 1 || before[0].Unread != 1 {
		t.Fatalf("before MarkRead: %+v, want one thread with unread 1", before)
	}
	if err := s.MarkRead("web"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	after, err := s.Inbox("web")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(after) != 1 || after[0].Unread != 0 {
		t.Fatalf("after MarkRead: %+v, want one thread with unread 0", after)
	}
}

// TestThreadsLastEntrySameMillisecond: with the clock frozen, the last entry
// must be identified by the higher ID (append order), never by created_at.
func TestThreadsLastEntrySameMillisecond(t *testing.T) {
	clock.SetForTesting(func() int64 { return 5000 })
	t.Cleanup(clock.ResetForTesting)

	s := newTestStore(t)
	id, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a", ToKind: "broadcast",
	}, "first")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := s.AppendEntry(id, "b", "second", ""); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	threads, err := s.Threads(10)
	if err != nil {
		t.Fatalf("Threads: %v", err)
	}
	if len(threads) != 1 {
		t.Fatalf("Threads returned %d, want 1", len(threads))
	}
	if threads[0].LastFrom != "b" {
		t.Fatalf("last entry from = %q, want %q (the higher-id entry, despite identical created_at)",
			threads[0].LastFrom, "b")
	}
	if threads[0].EntryCount != 2 {
		t.Fatalf("entry count = %d, want 2", threads[0].EntryCount)
	}
}

// TestThreadsOrderingTiesByID: threads sharing updated_at order newest-id
// first.
func TestThreadsOrderingTiesByID(t *testing.T) {
	clock.SetForTesting(func() int64 { return 9000 })
	t.Cleanup(clock.ResetForTesting)

	s := newTestStore(t)
	firstID, err := s.CreateThread(store.Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast"}, "one")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	secondID, err := s.CreateThread(store.Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast"}, "two")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	threads, err := s.Threads(10)
	if err != nil {
		t.Fatalf("Threads: %v", err)
	}
	if len(threads) != 2 || threads[0].ID != secondID || threads[1].ID != firstID {
		t.Fatalf("tie-break order = %+v, want [%d, %d]", threads, secondID, firstID)
	}
}

func TestThreadsRespectsLimit(t *testing.T) {
	s := newTestStore(t)
	oldID, err := s.CreateThread(store.Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast"}, "old-1")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := s.AppendEntry(oldID, "a", "old-2", ""); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	newID, err := s.CreateThread(store.Thread{Kind: "message", FromAgent: "b", ToKind: "broadcast"}, "new-1")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	threads, err := s.Threads(1)
	if err != nil {
		t.Fatalf("Threads: %v", err)
	}
	if len(threads) != 1 || threads[0].ID != newID || threads[0].EntryCount != 1 {
		t.Fatalf("limit=1 result = %+v, want just thread %d with 1 entry", threads, newID)
	}
}

// TestEffectiveIntentAcrossReadSurfaces: a task stored with intent "" must
// read as action-requested from Threads, GetThread AND Inbox — one vocabulary
// everywhere, no surface left on the raw value.
func TestEffectiveIntentAcrossReadSurfaces(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "worker"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	taskID, err := s.CreateThread(store.Thread{
		Kind: "task", FromAgent: "backend", ToKind: "agent", ToTarget: "worker", Status: "open",
	}, "please do X")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	msgID, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "backend", ToKind: "agent", ToTarget: "worker",
	}, "fyi-ish")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	th, _, err := s.GetThread(taskID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.Intent != store.IntentAction {
		t.Errorf("GetThread task intent = %q, want %q", th.Intent, store.IntentAction)
	}
	msg, _, err := s.GetThread(msgID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if msg.Intent != "" {
		t.Errorf("GetThread message intent = %q, want \"\"", msg.Intent)
	}

	for name, load := range map[string]func() ([]store.Thread, error){
		"Threads": func() ([]store.Thread, error) { return s.Threads(10) },
		"Inbox":   func() ([]store.Thread, error) { return s.Inbox("worker") },
	} {
		got, err := load()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		byID := map[int64]store.Thread{}
		for _, x := range got {
			byID[x.ID] = x
		}
		if byID[taskID].Intent != store.IntentAction {
			t.Errorf("%s task intent = %q, want %q", name, byID[taskID].Intent, store.IntentAction)
		}
		if byID[msgID].Intent != "" {
			t.Errorf("%s message intent = %q, want \"\"", name, byID[msgID].Intent)
		}
	}
}

// TestSessionUnreadCountsDistinctThreads: a broadcast concerning both aliases
// of one session counts once, never twice.
func TestSessionUnreadCountsDistinctThreads(t *testing.T) {
	s := newTestStore(t)
	for _, alias := range []string{"session-name", "chosen-alias"} {
		if err := s.RegisterAgent(store.Agent{Alias: alias, SocketPath: "/s", SessionID: "$1"}); err != nil {
			t.Fatalf("register %s: %v", alias, err)
		}
	}
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "broadcast",
	}, "hi all"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	total, action, err := s.SessionUnread("/s", "$1")
	if err != nil {
		t.Fatalf("SessionUnread: %v", err)
	}
	if total != 1 {
		t.Fatalf("broadcast concerning both sibling aliases counted total=%d, want 1", total)
	}
	if action != 0 {
		t.Fatalf("plain message counted action=%d, want 0", action)
	}
}

// TestSessionUnreadExcludesSiblingAuthors: actor exclusion is session-based,
// not per-alias.
func TestSessionUnreadExcludesSiblingAuthors(t *testing.T) {
	s := newTestStore(t)
	for _, alias := range []string{"a1", "a2"} {
		if err := s.RegisterAgent(store.Agent{Alias: alias, SocketPath: "/s", SessionID: "$1"}); err != nil {
			t.Fatalf("register %s: %v", alias, err)
		}
	}
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "outsider",
	}, "hello"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	total, action, err := s.SessionUnread("/s", "$1")
	if err != nil {
		t.Fatalf("SessionUnread: %v", err)
	}
	if total != 0 || action != 0 {
		t.Fatalf("a session's own write must not flag its own thread unread, got total=%d action=%d", total, action)
	}
}

// TestSessionUnreadActionCount: an unread task addressed to a session alias
// counts in action via effectiveIntent.
func TestSessionUnreadActionCount(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "worker", SocketPath: "/s", SessionID: "$1"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.CreateThread(store.Thread{
		Kind: "task", FromAgent: "backend", ToKind: "agent", ToTarget: "worker", Status: "open",
	}, "please do X"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	total, action, err := s.SessionUnread("/s", "$1")
	if err != nil {
		t.Fatalf("SessionUnread: %v", err)
	}
	if total != 1 || action != 1 {
		t.Fatalf("task addressed to session alias: total=%d action=%d, want 1,1", total, action)
	}
}

// TestSessionUnreadUsesPerAliasWatermark: each alias of a session is judged
// against its OWN read watermark, matching the SQLite join.
func TestSessionUnreadUsesPerAliasWatermark(t *testing.T) {
	s := newTestStore(t)
	for _, alias := range []string{"a1", "a2"} {
		if err := s.RegisterAgent(store.Agent{Alias: alias, SocketPath: "/s", SessionID: "$1"}); err != nil {
			t.Fatalf("register %s: %v", alias, err)
		}
	}
	// Addressed to a1 only, so only a1's watermark can clear it.
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "a1",
	}, "for a1"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if total, _, err := s.SessionUnread("/s", "$1"); err != nil || total != 1 {
		t.Fatalf("before any read: total=%d (%v), want 1", total, err)
	}
	// a2 reading its own inbox must NOT clear the thread: the SQL evaluates
	// the EXISTS against the watermark of the alias the thread actually
	// concerns, and this thread concerns only a1.
	if err := s.MarkRead("a2"); err != nil {
		t.Fatalf("MarkRead a2: %v", err)
	}
	if total, _, err := s.SessionUnread("/s", "$1"); err != nil || total != 1 {
		t.Fatalf("after sibling MarkRead: total=%d (%v), want 1 — each alias is judged against its OWN watermark", total, err)
	}
	// a1 reading its inbox does clear it.
	if err := s.MarkRead("a1"); err != nil {
		t.Fatalf("MarkRead a1: %v", err)
	}
	if total, _, err := s.SessionUnread("/s", "$1"); err != nil || total != 0 {
		t.Fatalf("after concerned alias MarkRead: total=%d (%v), want 0", total, err)
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
	if err := s.appendTransact(ctx, high, id, now, 7, "hi", true); err != nil {
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
	err = s.appendTransact(ctx, low, id, now+1, 5, "lo", true)
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
	if err := s.appendTransact(ctx, late, mine, now, lowID, "peer", true); err != nil {
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
	if err := s.appendTransact(ctx, late, mine, now, lowID, "peer", true); err != nil {
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
