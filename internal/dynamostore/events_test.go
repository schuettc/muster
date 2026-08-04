package dynamostore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/schuettc/muster/internal/clock"
	"github.com/schuettc/muster/internal/store"
)

// --- endpoint-free tests ----------------------------------------------------
//
// The pieces of the events implementation that are pure logic are tested
// without an endpoint on purpose: they then run in `just verify` on every
// machine, and the endpoint-backed tests below are left to cover only what
// genuinely needs DynamoDB.

func TestClampEventLimit(t *testing.T) {
	// Mirrors internal/store/events.go: <=0 and anything over the cap both
	// become maxEventLimit; in between is passed through untouched.
	tests := []struct{ in, want int }{
		{0, maxEventLimit},
		{-1, maxEventLimit},
		{1, 1},
		{999, 999},
		{maxEventLimit, maxEventLimit},
		{maxEventLimit + 1, maxEventLimit},
	}
	for _, tc := range tests {
		if got := clampEventLimit(tc.in); got != tc.want {
			t.Errorf("clampEventLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestEventConcernsMatchesEveryArm pins the Go translation of the SQLite
// agent-filter predicate. All four arms matter, and the two that are easiest
// to lose are the bare-alias target (a nudge) and the thread concern (a reply
// row carries an empty target, so only the thread can match the originator).
func TestEventConcernsMatchesEveryArm(t *testing.T) {
	concerning := map[int64]bool{7: true}
	tests := []struct {
		name string
		e    store.Event
		want bool
	}{
		{"actor", store.Event{Agent: "web"}, true},
		{"addressed target", store.Event{Target: "agent:web"}, true},
		{"bare target (nudge)", store.Event{Target: "web"}, true},
		{"thread concern", store.Event{Agent: "api", ThreadID: 7}, true},
		{"unrelated", store.Event{Agent: "x", Target: "agent:zzz", ThreadID: 999}, false},
		{"role target is not a bare alias", store.Event{Target: "role:web"}, false},
		{"thread-less and unrelated", store.Event{Agent: "x"}, false},
		// thread_id 0 must never consult the concerning set, or every
		// thread-less event would match once id 0 crept in.
		{"thread id zero", store.Event{Agent: "x", ThreadID: 0}, false},
	}
	for _, tc := range tests {
		if got := eventConcerns(tc.e, "web", concerning); got != tc.want {
			t.Errorf("%s: eventConcerns = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestEventRetentionKnob(t *testing.T) {
	t.Setenv(EventRetentionEnv, "")
	d, err := eventRetention()
	if err != nil || d != defaultEventRetention {
		t.Fatalf("unset: got %v (%v), want the %v default", d, err, defaultEventRetention)
	}
	t.Setenv(EventRetentionEnv, "48h")
	if d, err := eventRetention(); err != nil || d != 48*time.Hour {
		t.Fatalf("48h: got %v (%v)", d, err)
	}
	t.Setenv(EventRetentionEnv, "banana")
	if _, err := eventRetention(); err == nil {
		t.Fatal("an unparseable retention must fail loudly, not silently default")
	}
	t.Setenv(EventRetentionEnv, "0s")
	if _, err := eventRetention(); err == nil {
		t.Fatal("a non-positive retention must be rejected: it would expire every event on write")
	}
}

// TestPruneEventsIsANoOpOnThisBackend pins the documented contract: native TTL
// supersedes it here, so it deletes nothing and reports nothing. It needs no
// endpoint precisely because it touches no storage.
func TestPruneEventsIsANoOpOnThisBackend(t *testing.T) {
	s := &Store{table: "muster-test-unused"}
	n, err := s.PruneEvents(clock.NowMillis())
	if err != nil || n != 0 {
		t.Fatalf("PruneEvents = %d (%v), want 0, nil", n, err)
	}
}

// --- endpoint-backed tests --------------------------------------------------

func TestEventsBacklogAndFollowModes(t *testing.T) {
	s := newTestStore(t)
	for i, k := range []string{"send", "reply", "notify"} {
		if err := s.AppendEvent(store.Event{Kind: k, Agent: "web", ThreadID: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	back, err := s.Events(store.EventQuery{Backlog: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[0].Kind != "notify" || back[1].Kind != "reply" {
		t.Fatalf("backlog newest-first limit 2: %+v", back)
	}
	follow, err := s.Events(store.EventQuery{AfterID: back[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(follow) != 1 || follow[0].Kind != "notify" {
		t.Fatalf("follow after id %d: %+v", back[1].ID, follow)
	}
	if none, _ := s.Events(store.EventQuery{Backlog: true, Limit: 0}); len(none) != 0 {
		t.Fatalf("backlog limit 0 must return no rows, got %d", len(none))
	}
	if _, err := s.Events(store.EventQuery{AfterID: -1}); err == nil {
		t.Fatal("negative AfterID must error")
	}
	if _, err := s.Events(store.EventQuery{ThreadID: -1}); err == nil {
		t.Fatal("negative ThreadID must error")
	}
	maxID, err := s.MaxEventID()
	if err != nil || maxID != back[0].ID {
		t.Fatalf("MaxEventID = %d (%v), want %d", maxID, err, back[0].ID)
	}
}

func TestMaxEventIDOnEmptyJournal(t *testing.T) {
	s := newTestStore(t)
	n, err := s.MaxEventID()
	if err != nil || n != 0 {
		t.Fatalf("MaxEventID on an empty journal = %d (%v), want 0", n, err)
	}
}

func TestAppendEventRoundTripsEveryField(t *testing.T) {
	s := newTestStore(t)
	want := store.Event{Kind: "send", Agent: "web", Target: "agent:api", ThreadID: 3, Count: 4, Detail: "subj"}
	if err := s.AppendEvent(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Events(store.EventQuery{Backlog: true, Limit: 10})
	if err != nil || len(got) != 1 {
		t.Fatalf("Events: %d rows (%v)", len(got), err)
	}
	e := got[0]
	if e.Kind != want.Kind || e.Agent != want.Agent || e.Target != want.Target ||
		e.ThreadID != want.ThreadID || e.Count != want.Count || e.Detail != want.Detail {
		t.Fatalf("round trip = %+v, want %+v", e, want)
	}
	if e.ID == 0 || e.TS == 0 {
		t.Fatalf("id and ts must be stamped, got %+v", e)
	}
}

// TestEventsAgentFilterMatchesThreadConcern is the SQLite backend's finding-1
// regression, run here: a reply row has an empty target, so only the
// thread-concern arm can match the originator.
func TestEventsAgentFilterMatchesThreadConcern(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(store.Thread{Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api"}, "req")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []store.Event{
		{Kind: "send", Agent: "web", Target: "agent:api", ThreadID: id, Detail: "req"},
		{Kind: "reply", Agent: "api", ThreadID: id},
		{Kind: "nudge", Target: "web"},
		{Kind: "send", Agent: "x", Target: "agent:zzz", ThreadID: 999},
	} {
		if err := s.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Events(store.EventQuery{Agent: "web", Backlog: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 { // its send (actor), api's reply (thread concern), the nudge (bare target)
		t.Fatalf("agent=web should match 3 events, got %d: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Agent == "x" {
			t.Fatalf("unrelated event leaked through agent filter: %+v", e)
		}
	}
}

// TestEventsAgentFilterUsesTheAliasesRole covers the arm the actor/target
// arms cannot: a role-addressed thread concerns whoever currently holds that
// role, so the filter has to read the alias's role exactly like Inbox does.
func TestEventsAgentFilterUsesTheAliasesRole(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "api", Role: "worker"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateThread(store.Thread{Kind: "task", FromAgent: "web", ToKind: "role", ToTarget: "worker"}, "do it")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(store.Event{Kind: "task", Agent: "web", Target: "role:worker", ThreadID: id}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Events(store.EventQuery{Agent: "api", Backlog: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("api holds role worker, so the role-addressed thread's event concerns it: got %d: %+v", len(got), got)
	}
}

func TestEventsKindAndThreadFilters(t *testing.T) {
	s := newTestStore(t)
	for _, e := range []store.Event{
		{Kind: "send", Agent: "web", ThreadID: 1},
		{Kind: "reply", Agent: "api", ThreadID: 1},
		{Kind: "reply", Agent: "api", ThreadID: 2},
	} {
		if err := s.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	byKind, err := s.Events(store.EventQuery{Kind: "reply", Backlog: true, Limit: 10})
	if err != nil || len(byKind) != 2 {
		t.Fatalf("kind=reply: %d rows (%v)", len(byKind), err)
	}
	byThread, err := s.Events(store.EventQuery{ThreadID: 1, Backlog: true, Limit: 10})
	if err != nil || len(byThread) != 2 {
		t.Fatalf("thread_id=1: %d rows (%v)", len(byThread), err)
	}
	both, err := s.Events(store.EventQuery{Kind: "reply", ThreadID: 2, Backlog: true, Limit: 10})
	if err != nil || len(both) != 1 {
		t.Fatalf("kind=reply thread_id=2: %d rows (%v)", len(both), err)
	}
}

// TestEventsJoinsThreadSubjectAndEffectiveIntent mirrors the SQLite join. The
// intent half matters twice over: a task stored with intent ” must read as
// action-requested here exactly as it does everywhere else, and a thread-less
// event must carry neither field.
func TestEventsJoinsThreadSubjectAndEffectiveIntent(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(store.Thread{
		Kind: "task", FromAgent: "web", ToKind: "agent", ToTarget: "api", Subject: "hello subj",
	}, "b")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(store.Event{Kind: "notify", Agent: "api", ThreadID: id, Count: 1, Detail: "lit"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(store.Event{Kind: "read", Agent: "api"}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Events(store.EventQuery{Backlog: true, Limit: 10})
	if err != nil || len(evs) != 2 {
		t.Fatalf("Events: %d rows (%v)", len(evs), err)
	}
	if evs[1].Subject != "hello subj" || evs[0].Subject != "" {
		t.Fatalf("subject join: notify=%q (want hello subj), read=%q (want empty)", evs[1].Subject, evs[0].Subject)
	}
	if evs[1].Intent != store.IntentAction {
		t.Fatalf("a task stored with intent '' must read as action-requested, got %q", evs[1].Intent)
	}
	if evs[0].Intent != "" {
		t.Fatalf("a thread-less event carries no intent, got %q", evs[0].Intent)
	}
}

// TestEventsJoinToleratesAMissingThread pins the LEFT JOIN semantics: an event
// naming a thread that does not exist still comes back, with empty annotations
// rather than being dropped or erroring.
func TestEventsJoinToleratesAMissingThread(t *testing.T) {
	s := newTestStore(t)
	if err := s.AppendEvent(store.Event{Kind: "send", Agent: "web", ThreadID: 4242}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Events(store.EventQuery{Backlog: true, Limit: 10})
	if err != nil || len(evs) != 1 {
		t.Fatalf("Events: %d rows (%v)", len(evs), err)
	}
	if evs[0].Subject != "" || evs[0].Intent != "" {
		t.Fatalf("missing thread should annotate nothing, got %+v", evs[0])
	}
}

// TestAppendEventStampsTTL is the whole reason PruneEvents is a no-op here:
// every event item carries a `ttl` attribute in epoch SECONDS (DynamoDB's
// required unit — milliseconds would put the expiry ~50000 years out and
// nothing would ever be reaped) at now + the retention knob.
func TestAppendEventStampsTTL(t *testing.T) {
	s := newTestStore(t)
	if err := s.AppendEvent(store.Event{Kind: "read", Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Events(store.EventQuery{Backlog: true, Limit: 1})
	if err != nil || len(evs) != 1 {
		t.Fatalf("Events: %d rows (%v)", len(evs), err)
	}
	out, err := s.c.GetItem(context.Background(), &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            eventKey(evs[0].ID),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	ttl := numAttr(out.Item, "ttl")
	want := clock.NowMillis()/1000 + int64(defaultEventRetention/time.Second)
	if ttl < want-60 || ttl > want+60 {
		t.Fatalf("ttl = %d, want ~%d (now + %v, in seconds)", ttl, want, defaultEventRetention)
	}
}

// TestOpenRejectsAnUnparseableRetention keeps a typo in the knob from
// silently falling back to 30 days at deploy time.
func TestOpenRejectsAnUnparseableRetention(t *testing.T) {
	if os.Getenv(EndpointEnv) == "" {
		t.Skipf("%s unset; run `just verify-dynamo`", EndpointEnv)
	}
	t.Setenv(EventRetentionEnv, "not-a-duration")
	if _, err := Open(context.Background(), testTableName(t)); err == nil {
		t.Fatal("Open must refuse an unparseable retention knob")
	}
}
