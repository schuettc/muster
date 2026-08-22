// Package storetest holds the backend conformance suite: one set of
// behavioural tests run against every store.API implementation, so the SQLite
// and DynamoDB backends cannot drift.
//
// The SQLite implementation is the specification. Every case here was written
// against the store.API surface only — nothing reaches into a backend's
// storage — so a case that passes on one backend and fails on the other is a
// real disagreement about what the bus does, not a test artefact.
//
// Three behaviours are deliberately NOT asserted here, because the two
// backends legitimately differ and the difference is invisible to every
// caller:
//
//   - After IdemComplete(key, nil), SQLite hands back a zero-length non-nil
//     []byte and DynamoDB hands back nil. Both are honest reads of their own
//     storage, so the cases below assert len(resp) == 0 and never resp == nil.
//   - GetThread on an unknown id returns store.ErrThreadNotFound on DynamoDB
//     and sql.ErrNoRows on SQLite. The daemon only stringifies it, so no
//     caller can observe the identity; testGetThreadUnknownID asserts only
//     that some error comes back.
//   - The events FOLLOW path (EventQuery.AfterID). DynamoDB reads an
//     eventually-consistent GSI with no cross-item ordering guarantee, so a
//     follow poll can skip an event permanently; SQLite's serialized writers
//     and AUTOINCREMENT make that impossible. See the Events doc comment in
//     internal/dynamostore/events.go. Each backend pins its own follow
//     semantics in its own package; the shared suite covers only backlog
//     reads, which both backends order the same way.
//
// PruneEvents is likewise absent: it genuinely prunes on SQLite and returns
// (0, nil) on DynamoDB, where native TTL supersedes it.
package storetest

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/schuettc/muster/internal/clock"
	"github.com/schuettc/muster/internal/store"
)

// RunConformance runs the full behavioural suite against newStore. Each
// subtest gets a fresh store, so no case can be made to pass (or fail) by
// state another case left behind.
func RunConformance(t *testing.T, newStore func(t *testing.T) store.API) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, newStore(t))
		})
	}
}

type conformanceCase struct {
	name string
	fn   func(t *testing.T, s store.API)
}

var cases = []conformanceCase{
	// Roster.
	{"RegisterAgentUpsertsAndRevivesDeparted", testRegisterAgentUpsert},
	{"RegisterAgentRoundTripsEveryField", testRegisterAgentRoundTrip},
	{"ListAgentsOrdersByAlias", testListAgentsOrder},
	{"GetAgentUnknownAliasIsNotAnError", testGetAgentUnknown},
	{"DeleteAgentRemovesTheRow", testDeleteAgent},
	{"DepartAgentTombstonesPreservingFields", testDepartAgentTombstone},
	{"SetSessionLabelMovesSiblingsTogether", testSetSessionLabel},
	{"SetSessionLabelNoOpsOnEmptyTuple", testSetSessionLabelEmptyTuple},
	{"SetSessionLabelSparesACollidingDevice", testSetSessionLabelDeviceCollision},
	{"SetSessionLabelScopesToTheProvenIncarnation", testSetSessionLabelIncarnation},
	{"SetSessionLabelRefusesAnUnprovenIncarnation", testSetSessionLabelZeroIncarnation},
	{"DepartStaleSiblingsGhostGuard", testDepartStaleSiblings},
	{"DepartStaleSiblingsNoOpsOnEmptyTuple", testDepartStaleSiblingsEmptyTuple},
	{"DepartStaleSiblingsSparesACollidingDevice", testDepartStaleSiblingsDeviceCollision},

	// Threads and entries.
	{"CreateThreadAndGetThread", testCreateThread},
	{"CreateThreadValidatesIntent", testIntentValidation},
	{"CreateThreadPreservesAnExplicitIntentOnATask", testExplicitTaskIntent},
	{"EntriesReturnInIDOrder", testEntryOrder},
	{"AppendEntryReturnsTheNewEntryID", testAppendReturnsID},
	{"AppendEntryAdvancesUpdatedAt", testAppendAdvancesUpdatedAt},
	{"AppendEntryOnMissingThreadIsNotFound", testAppendMissingThread},
	{"GetThreadUnknownIDIsAnError", testGetThreadUnknownID},
	{"EffectiveIntentAcrossReadSurfaces", testEffectiveIntent},
	{"ThreadsLastEntryIsTheHighestID", testThreadsLastEntry},
	{"ThreadsOrderingTiesByID", testThreadsTieByID},
	{"ThreadsRespectsLimit", testThreadsLimit},
	{"ThreadsDefaultsAnUnsetLimit", testThreadsUnsetLimit},

	// Inbox and unread.
	{"UnreadCountRespectsWatermark", testUnreadWatermark},
	{"BroadcastCountsAsUnread", testBroadcastUnread},
	{"MarkReadUnknownAliasIsNoOp", testMarkReadUnknown},
	{"InboxMatchesAgentRoleBroadcastAndOriginated", testInboxArms},
	{"InboxIncludesOriginatedThreadsForUnregisteredAlias", testInboxOriginatedUnregistered},
	{"UnreadCountOriginatorSeesPeerReply", testUnreadOriginatorSeesReply},
	{"UnreadCountIgnoresOwnReply", testUnreadIgnoresOwnReply},
	{"InboxAnnotatesLastFromAndUnread", testInboxAnnotations},
	{"InboxUnreadDropsAfterMarkRead", testInboxUnreadDrops},
	{"InboxOrdersMostRecentlyUpdatedFirst", testInboxOrder},
	{"ScopedBroadcastReachesItsProjectOnly", testScopedBroadcastProjectOnly},
	{"ScopedBroadcastConcernsADepartedAgentsProject", testScopedBroadcastDeparted},

	// Session-scoped unread.
	{"SessionUnreadCountsDistinctThreads", testSessionUnreadDistinct},
	{"SessionUnreadExcludesSiblingAuthors", testSessionUnreadSiblingAuthors},
	{"SessionUnreadActionCount", testSessionUnreadAction},
	{"SessionUnreadUsesPerAliasWatermark", testSessionUnreadPerAliasWatermark},
	{"SessionUnreadEmptyTupleNeverGroups", testSessionUnreadEmptyTuple},
	{"SessionUnreadSeparatesCollidingDevices", testSessionUnreadDeviceCollision},
	{"SessionUnreadGroupsThePanelessTuple", testSessionUnreadPaneless},
	{"SessionUnreadSeparatesTmuxIncarnations", testSessionUnreadIncarnation},
	{"SessionUnreadRefusesAnUnprovenIncarnation", testSessionUnreadZeroIncarnation},
	{"SessionUnreadExemptsThePanelessTupleFromIncarnation", testSessionUnreadPanelessIgnoresIncarnation},

	// Identity: harness link, become, and supersession lineage.
	{"StampHarnessStampsBothFields", testStampHarness},
	{"StampHarnessEmptyArgLeavesFieldAlone", testStampHarnessPartial},
	{"StampHarnessUnknownAliasIsNoOp", testStampHarnessUnknown},
	{"RegisterPersistsTranscriptPath", testRegisterTranscriptPath},
	{"RegisterKeepsSupersededBy", testRegisterKeepsSupersededBy},
	{"LineageExcludesTombstonesOfOtherConversations", testLineageExcludesForeignTombstones},
	{"BecomeClonesIdentityAndRetiresSeed", testBecomeClonesAndRetires},
	{"BecomeCarriesTheReadWatermark", testBecomeCarriesWatermark},
	{"BecomeStampsSupersededBy", testBecomeStampsSupersededBy},
	{"BecomeRefusesAnExistingTarget", testBecomeRefusesExistingTarget},
	{"BecomeRefusesAMissingSource", testBecomeRefusesMissingSource},
	{"BecomeIsAtomicOnRefusal", testBecomeAtomicOnRefusal},
	{"SessionUnreadFollowsSupersessionLineage", testSessionUnreadLineage},
	{"SessionUnreadFollowsChainedLineage", testSessionUnreadChainedLineage},
	{"SessionAliasLineageWalksTheChain", testSessionAliasLineage},
	{"SessionAliasLineageEmptySessionIsEmpty", testSessionAliasLineageEmpty},
	{"SessionAliasLineageSeparatesCollidingDevices", testSessionAliasLineageDevices},
	{"SessionAliasLineageSeparatesTmuxIncarnations", testSessionAliasLineageIncarnation},
	{"SessionAliasLineageRefusesAnUnprovenIncarnation", testSessionAliasLineageZeroIncarnation},
	{"SessionLineageCrossesIncarnationsOnTheRecursiveStep", testLineageCrossesIncarnations},

	// Device poll.
	{"DevicePollFindsNewMail", testDevicePollFindsNewMail},
	{"DevicePollIgnoresOtherDevices", testDevicePollIgnoresOtherDevices},
	{"DevicePollWakesAnOriginator", testDevicePollWakesOriginator},
	{"DevicePollWatermarkAdvancesPastUnrelatedMail", testDevicePollWatermarkAdvances},
	{"DevicePollSkipsDepartedAndTuplelessAgents", testDevicePollSkipsUnbadgeable},
	{"DevicePollWakesOnlyTheScopedBroadcastsProject", testDevicePollScopedBroadcast},

	// Tasks.
	{"ClaimTaskSucceedsOnceThenFails", testClaimOnce},
	{"ClaimTaskIsAtomicUnderConcurrency", testClaimAtomic},
	{"ClaimTaskRecordsStatusChangeEntry", testClaimEntry},
	{"ClaimTaskOnMissingThreadIsNotClaimable", testClaimMissingThread},
	{"ClaimTaskOnATerminalTaskIsNotClaimable", testClaimTerminal},
	{"TransitionTaskValidatesAndRecords", testTransitionRecords},
	{"TransitionTaskOnMissingThreadIsNotFound", testTransitionMissingThread},
	{"TransitionTaskBackToOpenIsClaimableAgain", testTransitionReopen},

	// Idempotency records.
	{"IdemBeginClaimsThenReportsDone", testIdemLifecycle},
	{"IdemBeginIsAtomicUnderConcurrency", testIdemAtomic},
	{"IdemKeysAreIndependent", testIdemKeysIndependent},
	{"IdemCompleteUnknownKeyIsNoOp", testIdemCompleteUnknown},
	{"IdemCompleteEmptyResponse", testIdemCompleteEmpty},

	// Blackboard.
	{"KVSetIsLastWriteWins", testKVLastWriteWins},
	{"KVGetIsReadYourWrites", testKVReadYourWrites},

	// Journal.
	{"AppendEventRoundTripsEveryField", testEventRoundTrip},
	{"MaxEventIDOnAnEmptyJournal", testMaxEventIDEmpty},
	{"EventsBacklogIsNewestFirst", testEventsBacklog},
	{"EventsFilterByAgent", testEventsByAgent},
	{"EventsFilterByAgentUsesTheAliasesRole", testEventsByAgentRole},
	{"EventsFilterByKindAndThread", testEventsByKindAndThread},
	{"EventsJoinsThreadSubjectAndIntent", testEventsJoin},
	{"EventsJoinToleratesAMissingThread", testEventsJoinMissingThread},
}

// --- helpers ---------------------------------------------------------------

// liveCreated is the canonical "this session is running now" incarnation
// stamp. Session-tuple methods take a sessionCreated and a ZERO matches
// nothing (store.API: zero is the absence of proof, not a value), so a case
// that left it unset would ask for an incarnation no row has and get a
// perfectly plausible empty answer — passing while proving nothing. Every
// tmux-tuple registration therefore carries this, and every call site passes
// it; the incarnation cases below override it on purpose.
const liveCreated int64 = 1700000000

// ghostCreated is a DIFFERENT incarnation of the same session id — what tmux
// leaves behind when its server restarts and renumbers from $1 again.
const ghostCreated int64 = 1600000000

// mustRegister registers a, defaulting SessionCreated to liveCreated for a
// tmux-tuple registration that did not name one. The default is narrow: a
// paneless row (empty socket) is exempt from the incarnation dimension
// entirely and is left alone, and a case that names its own SessionCreated
// keeps it. To register a row with a genuinely absent incarnation — a legacy
// pre-v0.8.0 row on a real socket — use mustRegisterLegacy, which says so.
func mustRegister(t *testing.T, s store.API, a store.Agent) {
	t.Helper()
	if a.SocketPath != "" && a.SessionCreated == 0 {
		a.SessionCreated = liveCreated
	}
	if err := s.RegisterAgent(a); err != nil {
		t.Fatalf("RegisterAgent(%q): %v", a.Alias, err)
	}
}

// mustRegisterLegacy registers a row whose session_created is 0 on a real tmux
// tuple — a registration written before muster captured creation times. Such a
// row can never PROVE which incarnation it belongs to, so it is
// indistinguishable from a ghost, and the session-tuple methods must refuse to
// attribute anything to it. Separate from mustRegister so that "created 0" is
// always a decision a case made out loud, never a field someone forgot.
func mustRegisterLegacy(t *testing.T, s store.API, a store.Agent) {
	t.Helper()
	a.SessionCreated = 0
	if err := s.RegisterAgent(a); err != nil {
		t.Fatalf("RegisterAgent(%q): %v", a.Alias, err)
	}
}

func mustThread(t *testing.T, s store.API, th store.Thread, body string) int64 {
	t.Helper()
	id, err := s.CreateThread(th, body)
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return id
}

func mustAppend(t *testing.T, s store.API, id int64, from, body, statusChange string) {
	t.Helper()
	if _, err := s.AppendEntry(id, from, body, statusChange); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
}

// newTask creates the open task every claim/transition case starts from.
func newTask(t *testing.T, s store.API) int64 {
	t.Helper()
	return mustThread(t, s, store.Thread{
		Kind: "task", FromAgent: "backend", ToKind: "role", ToTarget: "reviewer", Status: "open",
	}, "review please")
}

// freezeClock pins the clock so same-millisecond ordering cases are
// deterministic. Both backends read time through internal/clock, so this is
// backend-agnostic.
func freezeClock(t *testing.T, at int64) {
	t.Helper()
	clock.SetForTesting(func() int64 { return at })
	t.Cleanup(clock.ResetForTesting)
}

func threadsByID(ts []store.Thread) map[int64]store.Thread {
	byID := make(map[int64]store.Thread, len(ts))
	for _, th := range ts {
		byID[th.ID] = th
	}
	return byID
}

// --- roster ----------------------------------------------------------------

// testRegisterAgentUpsert: re-registering an alias refreshes the mutable tuple
// in place (never a duplicate row), leaves the insert-only RegisteredAt alone,
// and clears the departed tombstone so a returning session revives its row.
func testRegisterAgentUpsert(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{
		Alias: "backend", Role: "producer", ModelType: "claude", SocketPath: "/s", PaneID: "%1",
	})
	first, ok, err := s.GetAgent("backend")
	if err != nil || !ok {
		t.Fatalf("GetAgent: ok=%v err=%v", ok, err)
	}
	if first.RegisteredAt == 0 || first.LastSeen == 0 {
		t.Fatalf("timestamps not stamped: %+v", first)
	}
	if err := s.DepartAgent("backend"); err != nil {
		t.Fatalf("DepartAgent: %v", err)
	}

	mustRegister(t, s, store.Agent{
		Alias: "backend", Role: "reviewer", ModelType: "claude", SocketPath: "/s2", PaneID: "%9",
	})
	agents, err := s.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("re-register produced %d rows, want 1 (upsert, not insert)", len(agents))
	}
	got := agents[0]
	if got.PaneID != "%9" || got.SocketPath != "/s2" || got.Role != "reviewer" {
		t.Errorf("upsert did not refresh the tuple: %+v", got)
	}
	if got.Departed {
		t.Error("re-registering must clear Departed — a returning session revives its row")
	}
	if got.RegisteredAt != first.RegisteredAt {
		t.Errorf("RegisteredAt changed on re-register: %d -> %d; it is insert-only",
			first.RegisteredAt, got.RegisteredAt)
	}
	if got.LastSeen < first.LastSeen {
		t.Errorf("LastSeen went backwards: %d -> %d", first.LastSeen, got.LastSeen)
	}
}

// testRegisterAgentRoundTrip pins every column through the write and both read
// paths — a field silently dropped by one backend is a wake-routing or
// liveness bug that nothing else notices.
func testRegisterAgentRoundTrip(t *testing.T, s store.API) {
	want := store.Agent{
		Alias: "a1", Role: "worker", ModelType: "claude",
		SocketPath: "/tmp/tmux-501/default", PaneID: "%1",
		SessionName: "muster-1", SessionID: "$1", SessionCreated: 1700000000,
		DeviceID: "dev-1", DeviceName: "work-laptop",
		Project: "muster", Label: "backend", LabelManual: true,
	}
	mustRegister(t, s, want)
	got, ok, err := s.GetAgent("a1")
	if err != nil || !ok {
		t.Fatalf("GetAgent: ok=%v err=%v", ok, err)
	}
	if got.Alias != want.Alias || got.Role != want.Role || got.ModelType != want.ModelType ||
		got.SocketPath != want.SocketPath || got.PaneID != want.PaneID ||
		got.SessionName != want.SessionName || got.SessionID != want.SessionID ||
		got.SessionCreated != want.SessionCreated || got.DeviceID != want.DeviceID ||
		got.DeviceName != want.DeviceName ||
		got.Project != want.Project || got.Label != want.Label || got.LabelManual != want.LabelManual {
		t.Fatalf("round trip lost fields:\n got %+v\nwant %+v", got, want)
	}
	list, err := s.ListAgents()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListAgents: %d rows (%v)", len(list), err)
	}
	if list[0].SessionCreated != want.SessionCreated || list[0].DeviceID != want.DeviceID ||
		list[0].DeviceName != want.DeviceName {
		t.Fatalf("ListAgents dropped identity fields: %+v", list[0])
	}
}

// testListAgentsOrder: aliases come back sorted, and a departed row is history
// rather than gone — it stays in the roster.
func testListAgentsOrder(t *testing.T, s store.API) {
	for _, alias := range []string{"c", "a", "b"} {
		mustRegister(t, s, store.Agent{Alias: alias})
	}
	if err := s.DepartAgent("b"); err != nil {
		t.Fatalf("DepartAgent: %v", err)
	}
	got, err := s.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	var aliases []string
	for _, a := range got {
		aliases = append(aliases, a.Alias)
	}
	if want := []string{"a", "b", "c"}; !slices.Equal(aliases, want) {
		t.Fatalf("aliases = %v, want %v (sorted, departed included)", aliases, want)
	}
}

func testGetAgentUnknown(t *testing.T, s store.API) {
	_, ok, err := s.GetAgent("nobody")
	if err != nil {
		t.Fatalf("GetAgent on an unknown alias must not error: %v", err)
	}
	if ok {
		t.Fatal("unknown alias must report ok=false")
	}
	// DepartAgent on an unknown alias is a no-op, not a phantom row: on
	// DynamoDB an unguarded UpdateItem would upsert one containing nothing
	// but a departed flag.
	if err := s.DepartAgent("nobody"); err != nil {
		t.Fatalf("DepartAgent unknown: %v", err)
	}
	agents, err := s.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("no-op calls created %d phantom row(s): %+v", len(agents), agents)
	}
}

func testDeleteAgent(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "a1"})
	if err := s.DeleteAgent("a1"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	if _, ok, err := s.GetAgent("a1"); err != nil || ok {
		t.Fatalf("agent should be gone after DeleteAgent: ok=%v err=%v", ok, err)
	}
	if err := s.DeleteAgent("nonexistent"); err != nil {
		t.Fatalf("DeleteAgent of an unknown alias must be a no-op, got %v", err)
	}
}

// testDepartAgentTombstone: deregistration is a tombstone, not a delete. The
// row survives with Departed set and every other field — project, label,
// label_manual, the read watermark — untouched, so departed history stays
// drillable.
func testDepartAgentTombstone(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{
		Alias: "muster-2", Role: "peer", ModelType: "codex", SocketPath: "/s", SessionID: "$1",
		Project: "muster", Label: "frontend", LabelManual: true,
	})
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "x", ToKind: "agent", ToTarget: "muster-2",
	}, "hi")
	if err := s.MarkRead("muster-2"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	before, ok, err := s.GetAgent("muster-2")
	if err != nil || !ok {
		t.Fatalf("GetAgent: ok=%v err=%v", ok, err)
	}
	if before.LastReadEntryID == 0 {
		t.Fatalf("MarkRead did not advance the watermark: %+v", before)
	}

	if err := s.DepartAgent("muster-2"); err != nil {
		t.Fatalf("DepartAgent: %v", err)
	}
	got, ok, err := s.GetAgent("muster-2")
	if err != nil || !ok {
		t.Fatalf("departed agent must still be readable: ok=%v err=%v", ok, err)
	}
	if !got.Departed {
		t.Error("Departed not set")
	}
	if got.Project != before.Project || got.Label != before.Label ||
		got.LabelManual != before.LabelManual || got.Role != before.Role ||
		got.LastReadEntryID != before.LastReadEntryID {
		t.Fatalf("tombstone lost fields:\n got %+v\nwant %+v", got, before)
	}
}

// testSetSessionLabel: the set_label op moves every LIVE alias on the tuple
// together — labels are addresses, so a sibling left behind is an alias that
// no longer answers to the name its session goes by. A departed sibling and
// another session are both spared.
func testSetSessionLabel(t *testing.T, s store.API) {
	const sock, sess = "/tmp/tmux-501/default", "$1"
	for _, alias := range []string{"a1", "a2"} {
		mustRegister(t, s, store.Agent{Alias: alias, SocketPath: sock, SessionID: sess})
	}
	mustRegister(t, s, store.Agent{Alias: "other", SocketPath: sock, SessionID: "$2"})
	mustRegister(t, s, store.Agent{Alias: "gone", SocketPath: sock, SessionID: sess})
	if err := s.DepartAgent("gone"); err != nil {
		t.Fatalf("DepartAgent: %v", err)
	}

	n, err := s.SetSessionLabel("", sock, sess, liveCreated, "backend", true)
	if err != nil {
		t.Fatalf("SetSessionLabel: %v", err)
	}
	if n != 2 {
		t.Fatalf("labelled %d rows, want 2 (a1 and a2 only)", n)
	}
	for _, tc := range []struct{ alias, want string }{
		{"a1", "backend"}, {"a2", "backend"}, {"other", ""}, {"gone", ""},
	} {
		got, _, err := s.GetAgent(tc.alias)
		if err != nil {
			t.Fatalf("GetAgent %s: %v", tc.alias, err)
		}
		if got.Label != tc.want {
			t.Errorf("%s label = %q, want %q", tc.alias, got.Label, tc.want)
		}
	}
}

func testSetSessionLabelEmptyTuple(t *testing.T, s store.API) {
	for _, tc := range []struct{ sock, sess string }{{"", "$1"}, {"/tmp/s", ""}, {"", ""}} {
		n, err := s.SetSessionLabel("", tc.sock, tc.sess, liveCreated, "x", true)
		if err != nil {
			t.Fatalf("SetSessionLabel(%q,%q): %v", tc.sock, tc.sess, err)
		}
		if n != 0 {
			t.Errorf("SetSessionLabel(%q,%q) = %d, want 0 — an empty tuple is never a group",
				tc.sock, tc.sess, n)
		}
	}
}

// testDepartStaleSiblings covers the ghost reaper: tmux recycles session IDs
// across server restarts, so a registration left behind by a dead incarnation
// can share a (socket, session id) tuple with a live session. Only a
// DIFFERING, NON-ZERO session_created proves a row is a ghost — a row with 0
// carries no incarnation evidence and must be spared.
func testDepartStaleSiblings(t *testing.T, s store.API) {
	const sock, sess = "/tmp/tmux-501/default", "$1"
	const nowCreated = int64(1700000200)
	reg := func(alias string, created int64) {
		t.Helper()
		mustRegister(t, s, store.Agent{
			Alias: alias, SocketPath: sock, SessionID: sess, SessionCreated: created,
		})
	}
	reg("ghost", 1700000000)
	reg("keeper", nowCreated)
	reg("sibling", nowCreated)
	// A row that predates creation-time capture. It must be SPARED even though
	// its created differs from nowCreated: 0 is not a different incarnation,
	// it is no evidence of one, and tombstoning on absent evidence destroys a
	// live registration. Registered through mustRegisterLegacy so the 0 stays
	// 0 — mustRegister would fill it in.
	mustRegisterLegacy(t, s, store.Agent{Alias: "preupgrade", SocketPath: sock, SessionID: sess})
	mustRegister(t, s, store.Agent{
		Alias: "elsewhere", SocketPath: sock, SessionID: "$2", SessionCreated: 1700000000,
	})

	stale, err := s.DepartStaleSiblings("", sock, sess, nowCreated, "keeper")
	if err != nil {
		t.Fatalf("DepartStaleSiblings: %v", err)
	}
	if want := []string{"ghost"}; !slices.Equal(stale, want) {
		t.Fatalf("departed %v, want %v — only a differing NON-ZERO session_created is a ghost", stale, want)
	}
	for _, alias := range []string{"keeper", "sibling", "preupgrade", "elsewhere"} {
		got, _, err := s.GetAgent(alias)
		if err != nil {
			t.Fatalf("GetAgent %s: %v", alias, err)
		}
		if got.Departed {
			t.Errorf("%s was departed but should have been spared", alias)
		}
	}
}

func testDepartStaleSiblingsEmptyTuple(t *testing.T, s store.API) {
	for _, tc := range []struct {
		sock, sess string
		created    int64
	}{{"", "$1", 1}, {"/tmp/s", "", 1}, {"/tmp/s", "$1", 0}} {
		got, err := s.DepartStaleSiblings("", tc.sock, tc.sess, tc.created, "keeper")
		if err != nil {
			t.Fatalf("DepartStaleSiblings(%q,%q,%d): %v", tc.sock, tc.sess, tc.created, err)
		}
		if len(got) != 0 {
			t.Errorf("DepartStaleSiblings(%q,%q,%d) = %v, want none",
				tc.sock, tc.sess, tc.created, got)
		}
	}
}

// testSetSessionLabelDeviceCollision: a label is a session-level property of a
// session on ONE machine. Two devices sharing the identical tuple (the default
// tmux socket plus $1, which every first-user macOS laptop produces) must not
// relabel each other — a label IS an address, so a stray relabel readdresses a
// peer's agent.
func testSetSessionLabelDeviceCollision(t *testing.T, s store.API) {
	const sock, sess = "/private/tmp/tmux-501/default", "$1"
	mustRegister(t, s, store.Agent{Alias: "mine", DeviceID: "dev-a", SocketPath: sock, SessionID: sess})
	mustRegister(t, s, store.Agent{Alias: "theirs", DeviceID: "dev-b", SocketPath: sock, SessionID: sess})

	n, err := s.SetSessionLabel("dev-a", sock, sess, liveCreated, "backend", true)
	if err != nil {
		t.Fatalf("SetSessionLabel: %v", err)
	}
	if n != 1 {
		t.Fatalf("labelled %d rows, want 1 — the colliding tuple on dev-b is another machine's session", n)
	}
	got, _, err := s.GetAgent("theirs")
	if err != nil {
		t.Fatalf("GetAgent theirs: %v", err)
	}
	if got.Label != "" {
		t.Errorf("dev-b's agent was relabelled %q by dev-a's set_label", got.Label)
	}
}

// testDepartStaleSiblingsDeviceCollision: the ghost reaper's evidence —
// "another row claims my session id under a different creation time" — is only
// evidence on MY machine. Two laptops' sessions have unrelated creation times,
// so without a device dimension registering on one tombstones the other's LIVE
// agent, which is a destroyed registration rather than a stale badge.
func testDepartStaleSiblingsDeviceCollision(t *testing.T, s store.API) {
	const sock, sess = "/private/tmp/tmux-501/default", "$1"
	mustRegister(t, s, store.Agent{
		Alias: "mine", DeviceID: "dev-a", SocketPath: sock, SessionID: sess, SessionCreated: 1700000200,
	})
	mustRegister(t, s, store.Agent{
		Alias: "theirs", DeviceID: "dev-b", SocketPath: sock, SessionID: sess, SessionCreated: 1700000000,
	})

	stale, err := s.DepartStaleSiblings("dev-a", sock, sess, 1700000200, "mine")
	if err != nil {
		t.Fatalf("DepartStaleSiblings: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("departed %v — a live agent on ANOTHER device is not a ghost of this one", stale)
	}
	got, _, err := s.GetAgent("theirs")
	if err != nil {
		t.Fatalf("GetAgent theirs: %v", err)
	}
	if got.Departed {
		t.Error("dev-b's live agent was tombstoned by dev-a's registration")
	}
}

// --- threads and entries ---------------------------------------------------

func testCreateThread(t *testing.T, s store.API) {
	id := mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
		Subject: "hi", Ref: "repo=x", OriginProject: "muster",
	}, "first body")
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
		t.Fatalf("entries = %+v, want one carrying 'first body'", entries)
	}
	// LastFrom/LastAt/EntryCount/Unread are query-time only: CreateThread and
	// GetThread leave them zero.
	if th.LastFrom != "" || th.LastAt != 0 || th.EntryCount != 0 || th.Unread != 0 {
		t.Fatalf("GetThread must leave query-time-only fields zero: %+v", th)
	}
}

// testIntentValidation: CreateThread is the validation boundary, so MCP, CLI
// and station cannot diverge on the vocabulary.
//
// Each accepted value is also read back. Accepting an intent and then dropping
// it is indistinguishable from rejecting it at every surface that matters, and
// on a message kind effectiveIntent is the identity, so the stored value must
// come back verbatim.
func testIntentValidation(t *testing.T, s store.API) {
	for _, ok := range []string{"", store.IntentFYI, store.IntentReply, store.IntentAction} {
		id, err := s.CreateThread(store.Thread{
			Kind: "message", FromAgent: "a", ToKind: "broadcast", Intent: ok,
		}, "body")
		if err != nil {
			t.Fatalf("intent %q should be valid: %v", ok, err)
		}
		th, _, err := s.GetThread(id)
		if err != nil {
			t.Fatalf("GetThread for intent %q: %v", ok, err)
		}
		if th.Intent != ok {
			t.Fatalf("message stored with intent %q read back as %q", ok, th.Intent)
		}
	}
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a", ToKind: "broadcast", Intent: "urgent",
	}, "body"); err == nil {
		t.Fatal("an unknown intent must be rejected")
	}
}

// testExplicitTaskIntent: effectiveIntent supplies action-requested only when
// a task stored NO intent. An explicit one is the operator's word and must
// survive every read surface unchanged.
func testExplicitTaskIntent(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "worker", SocketPath: "/s", SessionID: "$1"})
	id := mustThread(t, s, store.Thread{
		Kind: "task", FromAgent: "backend", ToKind: "agent", ToTarget: "worker",
		Status: "open", Intent: store.IntentFYI,
	}, "heads up, no action needed")

	th, _, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.Intent != store.IntentFYI {
		t.Errorf("GetThread intent = %q, want %q — an explicit intent must not be overridden",
			th.Intent, store.IntentFYI)
	}
	for name, load := range map[string]func() ([]store.Thread, error){
		"Threads": func() ([]store.Thread, error) { return s.Threads(10) },
		"Inbox":   func() ([]store.Thread, error) { return s.Inbox("worker") },
	} {
		got, err := load()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if in := threadsByID(got)[id].Intent; in != store.IntentFYI {
			t.Errorf("%s intent = %q, want %q", name, in, store.IntentFYI)
		}
	}
	// The action count keys off the EFFECTIVE intent, not off kind: a task
	// explicitly marked fyi is unread but is not asking for anything.
	total, action, err := s.SessionUnread("", "/s", "$1", liveCreated)
	if err != nil {
		t.Fatalf("SessionUnread: %v", err)
	}
	if total != 1 || action != 0 {
		t.Fatalf("an fyi-intent task: total=%d action=%d, want 1,0 — the action count follows "+
			"the effective intent, not the thread kind", total, action)
	}
}

func testEntryOrder(t *testing.T, s store.API) {
	id := mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
	}, "one")
	for _, body := range []string{"two", "three"} {
		mustAppend(t, s, id, "a2", body, "")
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

// testAppendReturnsID: the id AppendEntry returns is the id the entry
// actually got, and ids advance. The daemon hands this straight back to the
// caller as the reply's entry id, so a backend returning a counter value it
// then failed to use would misreport a write that never landed.
func testAppendReturnsID(t *testing.T, s store.API) {
	id := mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "a", ToKind: "agent", ToTarget: "b",
	}, "first")
	firstReply, err := s.AppendEntry(id, "b", "second", "")
	if err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	secondReply, err := s.AppendEntry(id, "b", "third", "")
	if err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if firstReply <= 0 || secondReply <= firstReply {
		t.Fatalf("returned entry ids = %d then %d, want increasing positives", firstReply, secondReply)
	}
	_, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("thread has %d entries, want 3", len(entries))
	}
	if entries[1].ID != firstReply || entries[2].ID != secondReply {
		t.Fatalf("stored ids = %d,%d but AppendEntry returned %d,%d",
			entries[1].ID, entries[2].ID, firstReply, secondReply)
	}
}

func testAppendAdvancesUpdatedAt(t *testing.T, s store.API) {
	id := mustThread(t, s, store.Thread{
		Kind: "task", FromAgent: "backend", ToKind: "role", ToTarget: "reviewer", Status: "open",
	}, "please review")
	before, _, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	mustAppend(t, s, id, "reviewer", "looks good", "claimed")
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

func testAppendMissingThread(t *testing.T, s store.API) {
	if _, err := s.AppendEntry(999999, "backend", "hello", ""); !errors.Is(err, store.ErrThreadNotFound) {
		t.Fatalf("AppendEntry on a missing thread = %v, want ErrThreadNotFound", err)
	}
	// The write must not have half-landed: nothing addressed to anyone.
	threads, err := s.Threads(10)
	if err != nil {
		t.Fatalf("Threads: %v", err)
	}
	if len(threads) != 0 {
		t.Fatalf("a failed append created %d thread(s): %+v", len(threads), threads)
	}
}

// testGetThreadUnknownID asserts only that an unknown id is an error. The two
// backends report DIFFERENT errors — store.ErrThreadNotFound on DynamoDB,
// sql.ErrNoRows on SQLite — and the daemon only stringifies it, so no caller
// can observe the difference. Asserting identity here would pin an accepted
// divergence rather than the contract.
func testGetThreadUnknownID(t *testing.T, s store.API) {
	if _, _, err := s.GetThread(999999); err == nil {
		t.Fatal("GetThread on an unknown id must return an error")
	}
}

// testEffectiveIntent: a task stored with intent "" must read as
// action-requested from Threads, GetThread AND Inbox — one vocabulary
// everywhere a Thread is read, with no surface left on the raw stored value.
func testEffectiveIntent(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "worker"})
	taskID := mustThread(t, s, store.Thread{
		Kind: "task", FromAgent: "backend", ToKind: "agent", ToTarget: "worker", Status: "open",
	}, "please do X")
	msgID := mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "backend", ToKind: "agent", ToTarget: "worker",
	}, "fyi-ish")

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
		byID := threadsByID(got)
		if byID[taskID].Intent != store.IntentAction {
			t.Errorf("%s task intent = %q, want %q", name, byID[taskID].Intent, store.IntentAction)
		}
		if byID[msgID].Intent != "" {
			t.Errorf("%s message intent = %q, want \"\"", name, byID[msgID].Intent)
		}
	}
}

// testThreadsLastEntry: with the clock frozen the two entries share a
// created_at, so the last entry must be identified by the higher ID (append
// order) rather than by an ambiguous MAX(created_at).
func testThreadsLastEntry(t *testing.T, s store.API) {
	freezeClock(t, 5000)
	id := mustThread(t, s, store.Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast"}, "first")
	mustAppend(t, s, id, "b", "second", "")
	threads, err := s.Threads(10)
	if err != nil {
		t.Fatalf("Threads: %v", err)
	}
	if len(threads) != 1 {
		t.Fatalf("Threads returned %d, want 1", len(threads))
	}
	if threads[0].LastFrom != "b" {
		t.Fatalf("last entry from = %q, want \"b\" (the higher-id entry, despite identical created_at)",
			threads[0].LastFrom)
	}
	if threads[0].EntryCount != 2 {
		t.Fatalf("entry count = %d, want 2", threads[0].EntryCount)
	}
	if threads[0].LastAt == 0 {
		t.Fatal("LastAt not populated")
	}
}

func testThreadsTieByID(t *testing.T, s store.API) {
	freezeClock(t, 9000)
	firstID := mustThread(t, s, store.Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast"}, "one")
	secondID := mustThread(t, s, store.Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast"}, "two")
	threads, err := s.Threads(10)
	if err != nil {
		t.Fatalf("Threads: %v", err)
	}
	if len(threads) != 2 || threads[0].ID != secondID || threads[1].ID != firstID {
		t.Fatalf("tie-break order = %+v, want [%d, %d] (newest id first)", threads, secondID, firstID)
	}
}

// testThreadsLimit: a thread outside the limited window contributes nothing —
// the threads that ARE returned still carry correct aggregates.
func testThreadsLimit(t *testing.T, s store.API) {
	oldID := mustThread(t, s, store.Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast"}, "old-1")
	mustAppend(t, s, oldID, "a", "old-2", "")
	newID := mustThread(t, s, store.Thread{Kind: "message", FromAgent: "b", ToKind: "broadcast"}, "new-1")
	threads, err := s.Threads(1)
	if err != nil {
		t.Fatalf("Threads: %v", err)
	}
	if len(threads) != 1 || threads[0].ID != newID || threads[0].EntryCount != 1 {
		t.Fatalf("limit=1 result = %+v, want just thread %d with 1 entry", threads, newID)
	}
}

// testThreadsUnsetLimit: limit <= 0 is "unspecified", not "none". Every human
// CLI and MCP caller that omits a limit relies on the store's default rather
// than on getting an empty list back.
func testThreadsUnsetLimit(t *testing.T, s store.API) {
	for _, body := range []string{"one", "two", "three"} {
		mustThread(t, s, store.Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast"}, body)
	}
	for _, limit := range []int{0, -1} {
		got, err := s.Threads(limit)
		if err != nil {
			t.Fatalf("Threads(%d): %v", limit, err)
		}
		if len(got) != 3 {
			t.Fatalf("Threads(%d) returned %d threads, want 3 — a non-positive limit means "+
				"'use the default', not 'return nothing'", limit, len(got))
		}
	}
}

// --- inbox and unread ------------------------------------------------------

func testUnreadWatermark(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "a2"})
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
	}, "unread one")
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
	if n, err := s.UnreadCount("a2"); err != nil || n != 0 {
		t.Fatalf("UnreadCount after MarkRead = %d (%v), want 0", n, err)
	}
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
	}, "unread two")
	if n, err := s.UnreadCount("a2"); err != nil || n != 1 {
		t.Fatalf("UnreadCount after a new message = %d (%v), want 1", n, err)
	}
}

func testBroadcastUnread(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "a2"})
	mustThread(t, s, store.Thread{Kind: "message", FromAgent: "a1", ToKind: "broadcast"}, "all hands")
	n, err := s.UnreadCount("a2")
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("broadcast unread = %d, want 1", n)
	}
}

func testMarkReadUnknown(t *testing.T, s store.API) {
	if err := s.MarkRead("nobody"); err != nil {
		t.Fatalf("MarkRead on an unknown alias: %v", err)
	}
	agents, err := s.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("MarkRead created %d phantom row(s): %+v", len(agents), agents)
	}
}

// testInboxArms is the thread-concern predicate in full — direct, by role, by
// broadcast, and originated. Inbox and UnreadCount must agree on it, so both
// are asserted against the same fixture.
func testInboxArms(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "rev1", Role: "reviewer"})
	mk := func(from, toKind, toTarget string) {
		t.Helper()
		mustThread(t, s, store.Thread{
			Kind: "message", FromAgent: from, ToKind: toKind, ToTarget: toTarget,
		}, "hi")
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
	n, err := s.UnreadCount("rev1")
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if n != 3 {
		t.Fatalf("UnreadCount = %d, want 3 — Inbox and UnreadCount must use the same predicate", n)
	}
}

func testInboxOriginatedUnregistered(t *testing.T, s store.API) {
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "req")
	in, err := s.Inbox("web")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(in) != 1 {
		t.Fatalf("the originator arm must not require registration, got %d threads", len(in))
	}
}

// testUnreadOriginatorSeesReply is the originator-blindness regression: a
// reply on a thread you started counts as unread for you, so the notify
// fan-out lights your mailbox instead of clearing it.
func testUnreadOriginatorSeesReply(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "web"})
	id := mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "req")
	if n, err := s.UnreadCount("web"); err != nil || n != 0 {
		t.Fatalf("own send must not count as unread, got %d (%v)", n, err)
	}
	mustAppend(t, s, id, "api", "done", "")
	if n, err := s.UnreadCount("web"); err != nil || n != 1 {
		t.Fatalf("peer reply on an originated thread = %d unread (%v), want 1", n, err)
	}
	if err := s.MarkRead("web"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if n, err := s.UnreadCount("web"); err != nil || n != 0 {
		t.Fatalf("unread after MarkRead = %d (%v), want 0", n, err)
	}
}

func testUnreadIgnoresOwnReply(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "api"})
	id := mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "req")
	if err := s.MarkRead("api"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	mustAppend(t, s, id, "api", "done", "")
	if n, err := s.UnreadCount("api"); err != nil || n != 0 {
		t.Fatalf("own reply re-flagged own inbox: %d unread (%v), want 0", n, err)
	}
}

// testInboxAnnotations is the production defect reproduction: an agent must be
// able to tell "a peer replied on my thread" from "my own last send" without
// drilling into GetThread.
func testInboxAnnotations(t *testing.T, s store.API) {
	for _, alias := range []string{"web", "api"} {
		mustRegister(t, s, store.Agent{Alias: alias})
	}
	repliedID := mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "req")
	mustAppend(t, s, repliedID, "api", "done", "")
	ownLastID := mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "another req")

	in, err := s.Inbox("web")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	byID := threadsByID(in)
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

func testInboxUnreadDrops(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "web"})
	id := mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "req")
	mustAppend(t, s, id, "api", "done", "")
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
		t.Fatalf("after MarkRead: %+v, want one thread with unread 0 — the annotation is watermark-relative", after)
	}
}

// testInboxOrder: an inbox is a work queue, so the thread touched most
// recently comes first — and a reply on an OLDER thread pulls it back to the
// top, which is the whole reason the sort key is updated_at rather than
// created_at.
func testInboxOrder(t *testing.T, s store.API) {
	var tick int64 = 1000
	clock.SetForTesting(func() int64 { tick++; return tick })
	t.Cleanup(clock.ResetForTesting)

	mustRegister(t, s, store.Agent{Alias: "api"})
	first := mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "oldest")
	second := mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "newer")

	in, err := s.Inbox("api")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(in) != 2 || in[0].ID != second || in[1].ID != first {
		t.Fatalf("inbox order = %+v, want [%d, %d] (most recent first)", in, second, first)
	}

	mustAppend(t, s, first, "web", "bumped", "")
	in, err = s.Inbox("api")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(in) != 2 || in[0].ID != first {
		t.Fatalf("after a reply on the older thread the order = %+v, want thread %d first",
			in, first)
	}
}

// testScopedBroadcastProjectOnly pins the scoped-broadcast arm of
// threadConcerns: a broadcast with a non-empty to_target concerns ONLY agents
// whose registered project matches it exactly, while a global one (empty
// target) still reaches everybody.
//
// Promoted here from the SQLite package's own tests, which is where the
// semantics landed and where they stayed pinned to one backend. Every other
// fixture in this suite creates broadcasts with ToTarget "", so the suite had
// zero scoped coverage — and the DynamoDB backend collapsed every broadcast
// into one recipient partition regardless of target, delivering
// `muster send --broadcast --project web` to every agent on the bus. That is
// silent cross-project delivery in exactly the shared-roster deployment the
// hosted backend exists for, and it survived precisely because this suite
// could not see it. A divergence this suite does not cover is a divergence
// that ships.
func testScopedBroadcastProjectOnly(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "in-proj", Project: "web"})
	mustRegister(t, s, store.Agent{Alias: "other-proj", Project: "api"})
	mustRegister(t, s, store.Agent{Alias: "no-proj"})

	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "sender", ToKind: "broadcast", ToTarget: "web",
	}, "web only")
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "sender", ToKind: "broadcast",
	}, "everyone")

	for alias, want := range map[string]int{"in-proj": 2, "other-proj": 1, "no-proj": 1} {
		in, err := s.Inbox(alias)
		if err != nil {
			t.Fatalf("Inbox(%s): %v", alias, err)
		}
		if len(in) != want {
			t.Errorf("Inbox(%s) = %d threads, want %d — a scoped broadcast must reach "+
				"only its own project", alias, len(in), want)
		}
		// UnreadCount must agree: same canonical predicate, and a badge that
		// disagrees with the inbox is the divergence that predicate exists to
		// prevent.
		n, err := s.UnreadCount(alias)
		if err != nil {
			t.Fatalf("UnreadCount(%s): %v", alias, err)
		}
		if n != want {
			t.Errorf("UnreadCount(%s) = %d, want %d (Inbox says %d)", alias, n, want, len(in))
		}
	}
}

// testScopedBroadcastDeparted pins the READ-TIME half of the same rule: the
// project comes from the agent's row when the query runs, not from anything
// captured when the thread was written. A tombstoned row preserves its
// project, so a departed agent still matches, and one that re-registers into
// the same alias sees the scoped broadcast waiting.
func testScopedBroadcastDeparted(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "ghost", Project: "web"})
	if err := s.DepartAgent("ghost"); err != nil {
		t.Fatalf("DepartAgent: %v", err)
	}
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "sender", ToKind: "broadcast", ToTarget: "web",
	}, "web only")

	in, err := s.Inbox("ghost")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(in) != 1 {
		t.Fatalf("departed ghost's inbox = %d threads, want 1", len(in))
	}
}

// --- session-scoped unread -------------------------------------------------

func testSessionUnreadDistinct(t *testing.T, s store.API) {
	for _, alias := range []string{"session-name", "chosen-alias"} {
		mustRegister(t, s, store.Agent{Alias: alias, SocketPath: "/s", SessionID: "$1"})
	}
	mustThread(t, s, store.Thread{Kind: "message", FromAgent: "peer", ToKind: "broadcast"}, "hi all")
	total, action, err := s.SessionUnread("", "/s", "$1", liveCreated)
	if err != nil {
		t.Fatalf("SessionUnread: %v", err)
	}
	if total != 1 {
		t.Fatalf("a broadcast concerning both sibling aliases counted total=%d, want 1", total)
	}
	if action != 0 {
		t.Fatalf("a plain message counted action=%d, want 0", action)
	}
}

func testSessionUnreadSiblingAuthors(t *testing.T, s store.API) {
	for _, alias := range []string{"a1", "a2"} {
		mustRegister(t, s, store.Agent{Alias: alias, SocketPath: "/s", SessionID: "$1"})
	}
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "outsider",
	}, "hello")
	total, action, err := s.SessionUnread("", "/s", "$1", liveCreated)
	if err != nil {
		t.Fatalf("SessionUnread: %v", err)
	}
	if total != 0 || action != 0 {
		t.Fatalf("a session's own write must not flag its own thread unread, got total=%d action=%d", total, action)
	}
}

func testSessionUnreadAction(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "worker", SocketPath: "/s", SessionID: "$1"})
	mustThread(t, s, store.Thread{
		Kind: "task", FromAgent: "backend", ToKind: "agent", ToTarget: "worker", Status: "open",
	}, "please do X")
	total, action, err := s.SessionUnread("", "/s", "$1", liveCreated)
	if err != nil {
		t.Fatalf("SessionUnread: %v", err)
	}
	if total != 1 || action != 1 {
		t.Fatalf("task addressed to a session alias: total=%d action=%d, want 1,1", total, action)
	}
}

// testSessionUnreadPerAliasWatermark: each alias of a session is judged
// against its OWN read watermark, so a sibling reading its inbox cannot clear
// a thread that concerns only its neighbour.
func testSessionUnreadPerAliasWatermark(t *testing.T, s store.API) {
	for _, alias := range []string{"a1", "a2"} {
		mustRegister(t, s, store.Agent{Alias: alias, SocketPath: "/s", SessionID: "$1"})
	}
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "a1",
	}, "for a1")
	if total, _, err := s.SessionUnread("", "/s", "$1", liveCreated); err != nil || total != 1 {
		t.Fatalf("before any read: total=%d (%v), want 1", total, err)
	}
	if err := s.MarkRead("a2"); err != nil {
		t.Fatalf("MarkRead a2: %v", err)
	}
	if total, _, err := s.SessionUnread("", "/s", "$1", liveCreated); err != nil || total != 1 {
		t.Fatalf("after a sibling's MarkRead: total=%d (%v), want 1 — each alias is judged against its OWN watermark",
			total, err)
	}
	if err := s.MarkRead("a1"); err != nil {
		t.Fatalf("MarkRead a1: %v", err)
	}
	if total, _, err := s.SessionUnread("", "/s", "$1", liveCreated); err != nil || total != 0 {
		t.Fatalf("after the concerned alias's MarkRead: total=%d (%v), want 0", total, err)
	}
}

func testSessionUnreadEmptyTuple(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "a1", SocketPath: "", SessionID: ""})
	mustThread(t, s, store.Thread{Kind: "message", FromAgent: "peer", ToKind: "broadcast"}, "hi")
	for _, tc := range []struct{ sock, sess string }{{"", ""}, {"", "$1"}, {"/s", ""}} {
		total, action, err := s.SessionUnread("", tc.sock, tc.sess, liveCreated)
		if err != nil {
			t.Fatalf("SessionUnread(%q,%q): %v", tc.sock, tc.sess, err)
		}
		if total != 0 || action != 0 {
			t.Fatalf("SessionUnread(%q,%q) = %d,%d, want 0,0 — an empty tuple is never a group",
				tc.sock, tc.sess, total, action)
		}
	}
}

// testSessionUnreadDeviceCollision is the two-device milestone in one case.
// (socket_path, session_id) is NOT device-unique in a shared store: two macOS
// laptops both run tmux on /private/tmp/tmux-501/default (501 is the default
// first-user uid) and each numbers its own sessions from $1. Without a device
// dimension the session's self-exclusion — "entries written by any alias of
// this session are my own writes" — swallows the REMOTE device's alias, and
// the receiving device's badge never lights.
func testSessionUnreadDeviceCollision(t *testing.T, s store.API) {
	const sock, sess = "/private/tmp/tmux-501/default", "$1"
	mustRegister(t, s, store.Agent{Alias: "backend", DeviceID: "dev-a", SocketPath: sock, SessionID: sess})
	mustRegister(t, s, store.Agent{Alias: "frontend", DeviceID: "dev-b", SocketPath: sock, SessionID: sess})

	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "frontend", ToKind: "agent", ToTarget: "backend",
	}, "ping from the other laptop")

	total, _, err := s.SessionUnread("dev-a", sock, sess, liveCreated)
	if err != nil {
		t.Fatalf("SessionUnread(dev-a): %v", err)
	}
	if total != 1 {
		t.Fatalf("dev-a unread = %d, want 1 — the sender is on ANOTHER device, so its write is not dev-a's own", total)
	}

	// The mirror image, which is what makes the exclusion still an exclusion:
	// on the sending device the same entry IS its own write.
	total, _, err = s.SessionUnread("dev-b", sock, sess, liveCreated)
	if err != nil {
		t.Fatalf("SessionUnread(dev-b): %v", err)
	}
	if total != 0 {
		t.Fatalf("dev-b unread = %d, want 0 — a session's own write never flags its own badge", total)
	}
}

// --- device poll -----------------------------------------------------------

func testDevicePollFindsNewMail(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{
		Alias: "a2", DeviceID: "dev-1", SocketPath: "/tmp/tmux-501/default", SessionID: "$1",
	})
	before, err := s.DevicePoll("dev-1", 0)
	if err != nil {
		t.Fatalf("DevicePoll: %v", err)
	}
	if len(before.Sessions) != 0 {
		t.Fatalf("no mail yet, got sessions %+v", before.Sessions)
	}
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
	}, "wake up")

	after, err := s.DevicePoll("dev-1", before.MaxEntryID)
	if err != nil {
		t.Fatalf("DevicePoll: %v", err)
	}
	if len(after.Sessions) != 1 || after.Sessions[0].SessionID != "$1" {
		t.Fatalf("sessions = %+v, want one $1", after.Sessions)
	}
	if after.MaxEntryID <= before.MaxEntryID {
		t.Fatalf("watermark did not advance: %d -> %d", before.MaxEntryID, after.MaxEntryID)
	}
	// Resuming from the new watermark must be quiet: the same entry is not
	// mail twice, or every tick would re-reconcile for ever.
	again, err := s.DevicePoll("dev-1", after.MaxEntryID)
	if err != nil {
		t.Fatalf("DevicePoll: %v", err)
	}
	if len(again.Sessions) != 0 {
		t.Fatalf("polling from the new watermark re-reported %+v", again.Sessions)
	}
}

func testDevicePollIgnoresOtherDevices(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{
		Alias: "a2", DeviceID: "dev-1", SocketPath: "/tmp/tmux-501/default", SessionID: "$1",
	})
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
	}, "for dev-1 only")

	got, err := s.DevicePoll("dev-2", 0)
	if err != nil {
		t.Fatalf("DevicePoll: %v", err)
	}
	if len(got.Sessions) != 0 {
		t.Fatalf("dev-2 was told to wake for dev-1's mail: %+v", got.Sessions)
	}
}

// testDevicePollWakesOriginator pins the four-arm concern predicate: a reply
// on a thread the local agent ORIGINATED lands in its inbox, so it must light
// its pane too. A poller that matched only the recipient arm would leave a
// peer's answer sitting unread in `muster inbox` with the badge dark — and
// only on the hosted backend, which is the worst kind of divergence.
func testDevicePollWakesOriginator(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{
		Alias: "asker", DeviceID: "dev-1", SocketPath: "/tmp/tmux-501/default", SessionID: "$1",
	})
	id := mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "asker", ToKind: "agent", ToTarget: "answerer",
	}, "question")
	mark, err := s.DevicePoll("dev-1", 0)
	if err != nil {
		t.Fatalf("DevicePoll: %v", err)
	}
	mustAppend(t, s, id, "answerer", "here is the answer", "")

	got, err := s.DevicePoll("dev-1", mark.MaxEntryID)
	if err != nil {
		t.Fatalf("DevicePoll: %v", err)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].SessionID != "$1" {
		t.Fatalf("sessions = %+v, want one $1 — a reply on an originated thread concerns its originator", got.Sessions)
	}
}

// testDevicePollWatermarkAdvances: mail for somebody else still moves the
// watermark. If it did not, every tick would re-read the same entries for ever
// and the poll would never go quiet on a busy bus.
func testDevicePollWatermarkAdvances(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{
		Alias: "mine", DeviceID: "dev-1", SocketPath: "/tmp/tmux-501/default", SessionID: "$1",
	})
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "x", ToKind: "agent", ToTarget: "y",
	}, "not for this device")

	got, err := s.DevicePoll("dev-1", 0)
	if err != nil {
		t.Fatalf("DevicePoll: %v", err)
	}
	if len(got.Sessions) != 0 {
		t.Fatalf("sessions = %+v, want none", got.Sessions)
	}
	if got.MaxEntryID == 0 {
		t.Fatal("watermark stayed at 0 over an entry that did not concern this device")
	}
}

// testDevicePollSkipsUnbadgeable: a departed agent and an agent with no tmux
// tuple have no badge anyone is watching, so neither can produce a session to
// reconcile — the same rule ReconcileLocalSessions applies on the device.
func testDevicePollSkipsUnbadgeable(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{
		Alias: "gone", DeviceID: "dev-1", SocketPath: "/tmp/tmux-501/default", SessionID: "$1",
	})
	if err := s.DepartAgent("gone"); err != nil {
		t.Fatalf("DepartAgent: %v", err)
	}
	mustRegister(t, s, store.Agent{Alias: "headless", DeviceID: "dev-1"})
	mustThread(t, s, store.Thread{Kind: "message", FromAgent: "peer", ToKind: "broadcast"}, "hi all")

	got, err := s.DevicePoll("dev-1", 0)
	if err != nil {
		t.Fatalf("DevicePoll: %v", err)
	}
	if len(got.Sessions) != 0 {
		t.Fatalf("sessions = %+v, want none — neither agent has a badge to light", got.Sessions)
	}
}

// testDevicePollScopedBroadcast carries the scoped-broadcast rule all the way
// to the wake path, which is where getting it wrong is loudest: DevicePoll is
// how cross-device mail lights a pane, so a broadcast that concerns only
// project "web" must wake only the devices with a "web" agent on them. Under
// the collapsed-partition bug this woke every device on the bus, lighting
// every operator's badge for a message their inbox would not even list.
func testDevicePollScopedBroadcast(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{
		Alias: "web-agent", Project: "web", DeviceID: "dev-web",
		SocketPath: "/tmp/tmux-501/default", SessionID: "$1",
	})
	mustRegister(t, s, store.Agent{
		Alias: "api-agent", Project: "api", DeviceID: "dev-api",
		SocketPath: "/tmp/tmux-501/default", SessionID: "$2",
	})
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "sender", ToKind: "broadcast", ToTarget: "web",
	}, "web only")

	web, err := s.DevicePoll("dev-web", 0)
	if err != nil {
		t.Fatalf("DevicePoll(dev-web): %v", err)
	}
	if len(web.Sessions) != 1 || web.Sessions[0].SessionID != "$1" {
		t.Errorf("dev-web sessions = %+v, want one $1 — the broadcast is scoped to its project",
			web.Sessions)
	}

	api, err := s.DevicePoll("dev-api", 0)
	if err != nil {
		t.Fatalf("DevicePoll(dev-api): %v", err)
	}
	if len(api.Sessions) != 0 {
		t.Errorf("dev-api sessions = %+v, want none — project api was never addressed",
			api.Sessions)
	}
	// The watermark still moves on the device that was not concerned: mail for
	// somebody else must not be re-examined for ever.
	if api.MaxEntryID == 0 {
		t.Error("dev-api's watermark stayed at 0 over an entry that did not concern it")
	}
}

// --- tasks -----------------------------------------------------------------

func testClaimOnce(t *testing.T, s store.API) {
	id := newTask(t, s)
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

// testClaimAtomic is the reason ClaimTask is a compare-and-swap rather than a
// read-then-write: it is the only thing stopping two agents from picking up
// the same work.
func testClaimAtomic(t *testing.T, s store.API) {
	id := newTask(t, s)
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
	// Every loser must lose the DOCUMENTED way — an infrastructure error that
	// happened to fail would satisfy the count while meaning something else
	// entirely to the daemon.
	for i, err := range errs {
		if err != nil && !errors.Is(err, store.ErrNotClaimable) {
			t.Fatalf("claim %d failed with %v, want ErrNotClaimable", i, err)
		}
	}
	// A losing claim writes NOTHING: the opener plus exactly one status-change
	// entry.
	_, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("thread has %d entries, want 2 — a losing claim must write no entry", len(entries))
	}
}

func testClaimEntry(t *testing.T, s store.API) {
	id := newTask(t, s)
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

// testClaimMissingThread pins the contract the obvious implementation gets
// wrong: an unknown id is NOT claimable, and must not surface as
// ErrThreadNotFound — a metadata read is the natural (and wrong) way to
// implement it.
func testClaimMissingThread(t *testing.T, s store.API) {
	err := s.ClaimTask(999999, "rev1")
	if !errors.Is(err, store.ErrNotClaimable) {
		t.Fatalf("ClaimTask on a missing thread = %v, want ErrNotClaimable", err)
	}
	if errors.Is(err, store.ErrThreadNotFound) {
		t.Fatalf("ClaimTask on a missing thread leaked ErrThreadNotFound: %v", err)
	}
}

// testClaimTerminal covers the states a real bus spends most of its life in:
// the open guard is what stops finished work being picked back up. A guard
// written as "status != claimed" would pass every other claim case here.
func testClaimTerminal(t *testing.T, s store.API) {
	for _, terminal := range []string{"completed", "cancelled", "declined"} {
		id := newTask(t, s)
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
			t.Fatalf("GetThread: %v", err)
		}
		if th.Status != terminal {
			t.Fatalf("a refused claim changed status to %q, want %q", th.Status, terminal)
		}
		if len(after) != len(before) {
			t.Fatalf("a refused claim wrote %d entries", len(after)-len(before))
		}
	}
}

func testTransitionRecords(t *testing.T, s store.API) {
	id := newTask(t, s)
	if err := s.ClaimTask(id, "rev1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.TransitionTask(id, "rev1", "bogus", ""); err == nil {
		t.Fatal("an invalid status must be rejected")
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

// testTransitionMissingThread — note the deliberate contrast with ClaimTask,
// which returns ErrNotClaimable for the same input.
func testTransitionMissingThread(t *testing.T, s store.API) {
	if err := s.TransitionTask(999999, "rev1", "completed", "LGTM"); !errors.Is(err, store.ErrThreadNotFound) {
		t.Fatalf("TransitionTask on a missing thread = %v, want ErrThreadNotFound", err)
	}
}

// testTransitionReopen closes the loop between the two methods: TransitionTask
// carries no status predicate, and the claim guard reads the LIVE status, so
// re-opening a task makes it claimable again rather than permanently burnt.
func testTransitionReopen(t *testing.T, s store.API) {
	id := newTask(t, s)
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

// --- idempotency records ---------------------------------------------------

func testIdemLifecycle(t *testing.T, s store.API) {
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

// testIdemAtomic is the test the whole design exists for: N callers race for
// one key and exactly one may execute the op.
//
// Counting winners is only half the contract. What every LOSER reports is the
// signal the dispatch wrapper reads to tell a replay from an in-flight
// duplicate, so each one is asserted individually: no error, found, not done.
// A backend whose losers returned an error instead would satisfy a
// winners-only count while turning "already in flight" into a 500.
func testIdemAtomic(t *testing.T, s store.API) {
	const n = 8
	type outcome struct {
		done  bool
		found bool
		err   error
	}
	outcomes := make([]outcome, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, done, found, err := s.IdemBegin("race")
			outcomes[i] = outcome{done: done, found: found, err: err}
		}()
	}
	wg.Wait()

	winners := 0
	for i, o := range outcomes {
		if o.err == nil && !o.found {
			winners++
			continue
		}
		// Every non-winner must lose the DOCUMENTED way.
		if o.err != nil {
			t.Errorf("caller %d: IdemBegin returned %v, want a clean in-flight report", i, o.err)
			continue
		}
		if o.done {
			t.Errorf("caller %d: done=true, want false — the op has not run yet", i)
		}
	}
	if winners != 1 {
		t.Fatalf("%d of %d callers claimed the key, want exactly 1 (outcomes: %+v)", winners, n, outcomes)
	}
}

// testIdemKeysIndependent guards the obvious way to get the claim wrong: a
// single-row table, or a key that is not actually part of the primary key.
func testIdemKeysIndependent(t *testing.T, s store.API) {
	if _, _, found, err := s.IdemBegin("a"); err != nil || found {
		t.Fatalf("claim a: found=%v err=%v", found, err)
	}
	if _, _, found, err := s.IdemBegin("b"); err != nil || found {
		t.Fatalf("claim b must be independent of a: found=%v err=%v", found, err)
	}
	if err := s.IdemComplete("a", []byte("ra")); err != nil {
		t.Fatalf("IdemComplete a: %v", err)
	}
	resp, done, found, err := s.IdemBegin("b")
	if err != nil || !found || done || len(resp) != 0 {
		t.Fatalf("b after completing a: found=%v done=%v resp=%q err=%v", found, done, resp, err)
	}
}

// testIdemCompleteUnknown pins the DynamoDB hazard against the SQLite
// specification: UpdateItem is an upsert, so an unguarded IdemComplete would
// CREATE a done record for a key nobody claimed and the next caller would be
// told its op had already run. SQLite's UPDATE matches no rows.
func testIdemCompleteUnknown(t *testing.T, s store.API) {
	if err := s.IdemComplete("never-claimed", []byte("x")); err != nil {
		t.Fatalf("IdemComplete on an unknown key: %v", err)
	}
	if _, _, found, err := s.IdemBegin("never-claimed"); err != nil || found {
		t.Fatalf("after a no-op complete the key must still be claimable: found=%v err=%v", found, err)
	}
}

// testIdemCompleteEmpty: an op recording no body still reads back as done.
// The length check is deliberate — SQLite returns a zero-length non-nil slice
// here and DynamoDB returns nil, an accepted divergence no caller can observe.
func testIdemCompleteEmpty(t *testing.T, s store.API) {
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

// --- blackboard ------------------------------------------------------------

func testKVLastWriteWins(t *testing.T, s store.API) {
	if _, ok, err := s.KVGet("api.base"); err != nil || ok {
		t.Fatalf("a missing key must read ok=false, got ok=%v err=%v", ok, err)
	}
	if err := s.KVSet("api.base", "http://localhost:4000", "backend"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.KVSet("api.base", "http://localhost:4001", "backend"); err != nil {
		t.Fatalf("overwrite must be last-write-wins, not an error: %v", err)
	}
	p, ok, err := s.KVGet("api.base")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if p.Key != "api.base" || p.Value != "http://localhost:4001" || p.UpdatedBy != "backend" {
		t.Fatalf("unexpected pair: %+v", p)
	}
	if p.UpdatedAt == 0 {
		t.Fatalf("UpdatedAt must be stamped, got %+v", p)
	}
}

// testKVReadYourWrites: the blackboard is a coordination primitive, so an
// agent that writes a fact and reads it back must never be handed the
// superseded value.
func testKVReadYourWrites(t *testing.T, s store.API) {
	for i := range 20 {
		if err := s.KVSet("k", fmt.Sprintf("v%d", i), "writer"); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
		p, ok, err := s.KVGet("k")
		if err != nil || !ok || p.Value != fmt.Sprintf("v%d", i) {
			t.Fatalf("read %d: ok=%v value=%q err=%v", i, ok, p.Value, err)
		}
	}
}

// --- journal ---------------------------------------------------------------

func testEventRoundTrip(t *testing.T, s store.API) {
	want := store.Event{Kind: "send", Agent: "web", Target: "agent:api", ThreadID: 3, Count: 4, Detail: "subj"}
	if err := s.AppendEvent(want); err != nil {
		t.Fatalf("AppendEvent: %v", err)
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

func testMaxEventIDEmpty(t *testing.T, s store.API) {
	n, err := s.MaxEventID()
	if err != nil || n != 0 {
		t.Fatalf("MaxEventID on an empty journal = %d (%v), want 0 — the follow poller's "+
			"starting watermark must not be an error", n, err)
	}
}

// testEventsBacklog covers the mode both backends order identically: a backlog
// read is newest-first and honours its limit, MaxEventID names the newest row,
// and a negative AfterID is rejected rather than silently treated as zero.
//
// The FOLLOW mode (AfterID > 0) is deliberately absent — see the package doc:
// DynamoDB's follow path reads an eventually-consistent index with no
// cross-item ordering guarantee, so parity there is not a contract either
// backend can be held to.
func testEventsBacklog(t *testing.T, s store.API) {
	for i, k := range []string{"send", "reply", "notify"} {
		if err := s.AppendEvent(store.Event{Kind: k, Agent: "web", ThreadID: int64(i + 1)}); err != nil {
			t.Fatalf("AppendEvent %s: %v", k, err)
		}
	}
	back, err := s.Events(store.EventQuery{Backlog: true, Limit: 2})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(back) != 2 || back[0].Kind != "notify" || back[1].Kind != "reply" {
		t.Fatalf("backlog newest-first limit 2: %+v", back)
	}
	if none, err := s.Events(store.EventQuery{Backlog: true, Limit: 0}); err != nil || len(none) != 0 {
		t.Fatalf("backlog limit 0 must return no rows, got %d (%v)", len(none), err)
	}
	if _, err := s.Events(store.EventQuery{AfterID: -1}); err == nil {
		t.Fatal("a negative AfterID must error")
	}
	maxID, err := s.MaxEventID()
	if err != nil || maxID != back[0].ID {
		t.Fatalf("MaxEventID = %d (%v), want %d", maxID, err, back[0].ID)
	}
}

// testEventsByAgent is the finding-1 regression: a reply row carries an empty
// target, so only the thread-concern arm can match the originator.
func testEventsByAgent(t *testing.T, s store.API) {
	id := mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "web", ToKind: "agent", ToTarget: "api",
	}, "req")
	for _, e := range []store.Event{
		{Kind: "send", Agent: "web", Target: "agent:api", ThreadID: id, Detail: "req"},
		{Kind: "reply", Agent: "api", ThreadID: id},
		{Kind: "nudge", Target: "web"},
		{Kind: "send", Agent: "x", Target: "agent:zzz", ThreadID: 999},
	} {
		if err := s.AppendEvent(e); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	got, err := s.Events(store.EventQuery{Agent: "web", Backlog: true, Limit: 10})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	// Its own send (actor), api's reply (thread concern), the nudge (bare
	// target).
	if len(got) != 3 {
		t.Fatalf("agent=web should match 3 events, got %d: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Agent == "x" {
			t.Fatalf("an unrelated event leaked through the agent filter: %+v", e)
		}
	}
}

// testEventsByAgentRole covers the arm the actor and target arms cannot: a
// role-addressed thread concerns whoever currently HOLDS that role, so the
// filter has to read the alias's role exactly like Inbox does.
func testEventsByAgentRole(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "api", Role: "worker"})
	id := mustThread(t, s, store.Thread{
		Kind: "task", FromAgent: "web", ToKind: "role", ToTarget: "worker", Status: "open",
	}, "do it")
	// The second event is the negative control: without it a filter that
	// matched EVERYTHING would satisfy the count below.
	for _, e := range []store.Event{
		{Kind: "task", Agent: "web", Target: "role:worker", ThreadID: id},
		{Kind: "send", Agent: "x", Target: "agent:zzz", ThreadID: 999},
	} {
		if err := s.AppendEvent(e); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	got, err := s.Events(store.EventQuery{Agent: "api", Backlog: true, Limit: 10})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("api holds role worker, so the role-addressed thread's event concerns it: got %d: %+v",
			len(got), got)
	}
	if got[0].Agent == "x" {
		t.Fatalf("an unrelated event leaked through the role-aware agent filter: %+v", got[0])
	}
}

func testEventsByKindAndThread(t *testing.T, s store.API) {
	for _, e := range []store.Event{
		{Kind: "send", Agent: "web", ThreadID: 1},
		{Kind: "reply", Agent: "api", ThreadID: 1},
		{Kind: "reply", Agent: "api", ThreadID: 2},
	} {
		if err := s.AppendEvent(e); err != nil {
			t.Fatalf("AppendEvent: %v", err)
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

// testEventsJoin: subject and effective intent are joined at query time, and a
// thread-less event carries neither.
func testEventsJoin(t *testing.T, s store.API) {
	id := mustThread(t, s, store.Thread{
		Kind: "task", FromAgent: "web", ToKind: "agent", ToTarget: "api", Subject: "hello subj",
	}, "b")
	if err := s.AppendEvent(store.Event{
		Kind: "notify", Agent: "api", ThreadID: id, Count: 1, Detail: "lit",
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := s.AppendEvent(store.Event{Kind: "read", Agent: "api"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	evs, err := s.Events(store.EventQuery{Backlog: true, Limit: 10})
	if err != nil || len(evs) != 2 {
		t.Fatalf("Events: %d rows (%v)", len(evs), err)
	}
	if evs[1].Subject != "hello subj" || evs[0].Subject != "" {
		t.Fatalf("subject join: notify=%q (want hello subj), read=%q (want empty)",
			evs[1].Subject, evs[0].Subject)
	}
	if evs[1].Intent != store.IntentAction {
		t.Fatalf("a task stored with intent \"\" must read as action-requested, got %q", evs[1].Intent)
	}
	if evs[0].Intent != "" {
		t.Fatalf("a thread-less event carries no intent, got %q", evs[0].Intent)
	}
}

// testEventsJoinMissingThread pins the LEFT JOIN semantics: an event naming a
// thread that does not exist still comes back, annotated with nothing, rather
// than being dropped or erroring.
func testEventsJoinMissingThread(t *testing.T, s store.API) {
	if err := s.AppendEvent(store.Event{Kind: "send", Agent: "web", ThreadID: 4242}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	evs, err := s.Events(store.EventQuery{Backlog: true, Limit: 10})
	if err != nil || len(evs) != 1 {
		t.Fatalf("Events: %d rows (%v)", len(evs), err)
	}
	if evs[0].Subject != "" || evs[0].Intent != "" {
		t.Fatalf("a missing thread should annotate nothing, got %+v", evs[0])
	}
}

// --- identity: harness link, become, supersession lineage -------------------

// testSessionUnreadPaneless: ("", harness session UUID) is the PANELESS tuple —
// a session with no tmux pane still has a real identity, and its sibling
// aliases must group exactly like a tmux session's. This is the case an
// "empty socket path never groups" guard silently swallows: the backend returns
// 0 unread forever and the session never learns it has mail.
func testSessionUnreadPaneless(t *testing.T, s store.API) {
	const uuid = "9f1c2f6e-0000-4000-8000-000000000001"
	for _, alias := range []string{"paneless-a", "paneless-b"} {
		mustRegister(t, s, store.Agent{Alias: alias, SocketPath: "", SessionID: uuid})
	}
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "paneless-a",
	}, "mail for a paneless session")

	total, _, err := s.SessionUnread("", "", uuid, 0)
	if err != nil {
		t.Fatalf("SessionUnread: %v", err)
	}
	if total != 1 {
		t.Fatalf("paneless session unread = %d, want 1 — ('' , uuid) is a real tuple", total)
	}
}

func testStampHarness(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "backend", SocketPath: "/s", SessionID: "$1"})
	if err := s.StampHarness("backend", "uuid-1", "/t/a.jsonl"); err != nil {
		t.Fatalf("StampHarness: %v", err)
	}
	a, ok, err := s.GetAgent("backend")
	if err != nil || !ok {
		t.Fatalf("GetAgent: %v (ok=%v)", err, ok)
	}
	if a.HarnessSessionID != "uuid-1" || a.TranscriptPath != "/t/a.jsonl" {
		t.Fatalf("got harness=%q transcript=%q", a.HarnessSessionID, a.TranscriptPath)
	}
	// Identity, tuple and read state are untouched by the stamp.
	if a.SocketPath != "/s" || a.SessionID != "$1" {
		t.Fatalf("stamp disturbed the tuple: %+v", a)
	}
}

func testStampHarnessPartial(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "backend", SocketPath: "/s", SessionID: "$1", HarnessSessionID: "uuid-0", TranscriptPath: "/t/a.jsonl"})
	if err := s.StampHarness("backend", "uuid-1", ""); err != nil {
		t.Fatalf("StampHarness: %v", err)
	}
	a, _, _ := s.GetAgent("backend")
	if a.HarnessSessionID != "uuid-1" || a.TranscriptPath != "/t/a.jsonl" {
		t.Fatalf("empty transcript arg must not clear it: harness=%q transcript=%q", a.HarnessSessionID, a.TranscriptPath)
	}
	if err := s.StampHarness("backend", "", "/t/b.jsonl"); err != nil {
		t.Fatalf("StampHarness: %v", err)
	}
	a, _, _ = s.GetAgent("backend")
	if a.HarnessSessionID != "uuid-1" || a.TranscriptPath != "/t/b.jsonl" {
		t.Fatalf("empty harness arg must not clear it: harness=%q transcript=%q", a.HarnessSessionID, a.TranscriptPath)
	}
}

// testStampHarnessUnknown is the phantom-row guard. A backend whose
// update is an upsert (DynamoDB's UpdateItem is) will CREATE a row for an
// alias that was never registered, where the SQLite UPDATE matches nothing.
// The phantom is a roster member with an empty tuple and no identity.
func testStampHarnessUnknown(t *testing.T, s store.API) {
	if err := s.StampHarness("ghost", "uuid-2", "/t/x.jsonl"); err != nil {
		t.Fatalf("StampHarness on an unknown alias must be a no-op, got %v", err)
	}
	if _, ok, err := s.GetAgent("ghost"); err != nil || ok {
		t.Fatalf("stamping an unknown alias created a phantom row (ok=%v, err=%v)", ok, err)
	}
	agents, err := s.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("roster gained %d phantom row(s): %+v", len(agents), agents)
	}
}

func testRegisterTranscriptPath(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "a", SocketPath: "/s", SessionID: "$1", TranscriptPath: "/t/a.jsonl"})
	a, _, _ := s.GetAgent("a")
	if a.TranscriptPath != "/t/a.jsonl" {
		t.Fatalf("transcript_path not persisted: %q", a.TranscriptPath)
	}
	all, _ := s.ListAgents()
	if all[0].TranscriptPath != "/t/a.jsonl" {
		t.Fatal("ListAgents must carry transcript_path")
	}
}

// testRegisterKeepsSupersededBy: re-registering a claimed-away alias must NOT
// forget its supersession pointer — a returning session on the old tuple is
// exactly the case become-lineage exists to route mail through, not a signal
// that the claim never happened.
func testRegisterKeepsSupersededBy(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "seed", SocketPath: "/s", SessionID: "$1"})
	if err := s.Become("seed", "claimed"); err != nil {
		t.Fatalf("Become: %v", err)
	}
	mustRegister(t, s, store.Agent{Alias: "seed", SocketPath: "/s", SessionID: "$1"})
	a, _, _ := s.GetAgent("seed")
	if a.SupersededBy != "claimed" {
		t.Fatalf("re-register must not forget the successor: got %q", a.SupersededBy)
	}
	if a.Departed {
		t.Fatal("re-registering still revives the tombstone")
	}
}

// testLineageExcludesForeignTombstones: a previous conversation's tombstone
// sitting on the same tuple with no successor must never be pulled into a
// live conversation's lineage walk — only rows that are either live or
// chained through a successor belong to somebody's identity.
func testLineageExcludesForeignTombstones(t *testing.T, s store.API) {
	// A previous conversation's tombstone on the same tuple (no successor)…
	mustRegister(t, s, store.Agent{Alias: "old-conv", SocketPath: "/s", SessionID: "$1", SessionCreated: 5, PaneID: "%1"})
	if err := s.DepartAgent("old-conv"); err != nil {
		t.Fatal(err)
	}
	// …and the live conversation with a become-chain seed.
	mustRegister(t, s, store.Agent{Alias: "seed", SocketPath: "/s", SessionID: "$1", SessionCreated: 5, PaneID: "%1"})
	if err := s.Become("seed", "me"); err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionAliasLineage("", "/s", "$1", 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"me", "seed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage = %v, want %v (old-conv is another conversation's tombstone)", got, want)
	}
}

// testBecomeClonesAndRetires is the core claim: the successor inherits the
// whole identity — tuple, DEVICE, harness link, project, label, role, model —
// and the seed becomes a tombstone. The device matters as much as the rest: the
// session tuple that addresses the successor is (device, socket, session), so a
// clone that dropped the device would land on a tuple nothing queries.
func testBecomeClonesAndRetires(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{
		Alias: "seed", Role: "backend", ModelType: "claude",
		DeviceID: "dev-a", SocketPath: "/s", SessionID: "$1", SessionCreated: 4242,
		HarnessSessionID: "uuid-1", Project: "muster", Label: "alias-routing", LabelManual: true,
	})
	if err := s.Become("seed", "claimed"); err != nil {
		t.Fatalf("Become: %v", err)
	}

	got, ok, err := s.GetAgent("claimed")
	if err != nil || !ok {
		t.Fatalf("GetAgent(claimed): %v (ok=%v)", err, ok)
	}
	for _, f := range []struct{ name, got, want string }{
		{"role", got.Role, "backend"},
		{"model_type", got.ModelType, "claude"},
		{"device_id", got.DeviceID, "dev-a"},
		{"socket_path", got.SocketPath, "/s"},
		{"session_id", got.SessionID, "$1"},
		{"harness_session_id", got.HarnessSessionID, "uuid-1"},
		{"project", got.Project, "muster"},
		{"label", got.Label, "alias-routing"},
	} {
		if f.got != f.want {
			t.Errorf("clone %s = %q, want %q", f.name, f.got, f.want)
		}
	}
	if got.SessionCreated != 4242 {
		t.Errorf("clone session_created = %d, want 4242", got.SessionCreated)
	}
	if !got.LabelManual {
		t.Error("clone lost label_manual")
	}
	if got.Departed {
		t.Error("the successor must be live, not a tombstone")
	}
	if got.SupersededBy != "" {
		t.Errorf("the successor must start unsuperseded, got %q", got.SupersededBy)
	}

	seed, ok, err := s.GetAgent("seed")
	if err != nil || !ok {
		t.Fatalf("the seed row must survive as history: %v (ok=%v)", err, ok)
	}
	if !seed.Departed {
		t.Error("the seed must be retired")
	}
}

// testBecomeCarriesWatermark: without the read watermark the claimed identity
// would open on all of history as unread — the loudest possible regression.
func testBecomeCarriesWatermark(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "seed", SocketPath: "/s", SessionID: "$1"})
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "seed",
	}, "old news")
	if err := s.MarkRead("seed"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	seed, _, err := s.GetAgent("seed")
	if err != nil {
		t.Fatalf("GetAgent(seed): %v", err)
	}
	if seed.LastReadEntryID == 0 {
		t.Fatal("test setup: MarkRead did not advance the watermark")
	}
	if err := s.Become("seed", "claimed"); err != nil {
		t.Fatalf("Become: %v", err)
	}
	got, _, err := s.GetAgent("claimed")
	if err != nil {
		t.Fatalf("GetAgent(claimed): %v", err)
	}
	if got.LastReadEntryID != seed.LastReadEntryID {
		t.Fatalf("clone watermark = %d, want %d — the claim must not re-open read history",
			got.LastReadEntryID, seed.LastReadEntryID)
	}
}

func testBecomeStampsSupersededBy(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "seed", SocketPath: "/s", SessionID: "$1"})
	if err := s.Become("seed", "claimed"); err != nil {
		t.Fatalf("Become: %v", err)
	}
	seed, _, err := s.GetAgent("seed")
	if err != nil {
		t.Fatalf("GetAgent(seed): %v", err)
	}
	if seed.SupersededBy != "claimed" {
		t.Fatalf("seed superseded_by = %q, want %q", seed.SupersededBy, "claimed")
	}

	// Chained: B→C must not write C's pointer backward onto the successor.
	if err := s.Become("claimed", "final"); err != nil {
		t.Fatalf("Become(chained): %v", err)
	}
	mid, _, err := s.GetAgent("claimed")
	if err != nil {
		t.Fatalf("GetAgent(claimed): %v", err)
	}
	if mid.SupersededBy != "final" {
		t.Fatalf("mid superseded_by = %q, want %q", mid.SupersededBy, "final")
	}
	last, _, err := s.GetAgent("final")
	if err != nil {
		t.Fatalf("GetAgent(final): %v", err)
	}
	if last.SupersededBy != "" {
		t.Fatalf("the head of a chain must be unsuperseded, got %q", last.SupersededBy)
	}
}

// testBecomeRefusesExistingTarget: `to` must not exist AT ALL. A live row is
// someone else's identity; a tombstone is another conversation's history.
// Merging identities is the confusion the feature exists to kill, so this is a
// compare-and-swap, not a read-then-write.
func testBecomeRefusesExistingTarget(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "seed", SocketPath: "/s", SessionID: "$1"})
	mustRegister(t, s, store.Agent{Alias: "taken", SocketPath: "/other", SessionID: "$2"})

	if err := s.Become("seed", "taken"); !errors.Is(err, store.ErrBecomeToExists) {
		t.Fatalf("Become onto a live alias = %v, want ErrBecomeToExists", err)
	}
	// A tombstone is just as much of a refusal as a live row.
	if err := s.DepartAgent("taken"); err != nil {
		t.Fatalf("DepartAgent: %v", err)
	}
	if err := s.Become("seed", "taken"); !errors.Is(err, store.ErrBecomeToExists) {
		t.Fatalf("Become onto a tombstone = %v, want ErrBecomeToExists", err)
	}
}

func testBecomeRefusesMissingSource(t *testing.T, s store.API) {
	if err := s.Become("ghost", "claimed"); !errors.Is(err, store.ErrBecomeFromMissing) {
		t.Fatalf("Become from an unknown alias = %v, want ErrBecomeFromMissing", err)
	}
	if _, ok, err := s.GetAgent("claimed"); err != nil || ok {
		t.Fatalf("a refused claim must write nothing, got ok=%v (%v)", ok, err)
	}
}

// testBecomeAtomicOnRefusal: a refused claim leaves BOTH rows exactly as they
// were. On a backend without a transaction, a clone that succeeded before the
// retire failed would leave the seed live alongside its own successor.
func testBecomeAtomicOnRefusal(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "seed", SocketPath: "/s", SessionID: "$1", Project: "muster"})
	mustRegister(t, s, store.Agent{Alias: "taken", SocketPath: "/other", SessionID: "$2", Project: "other"})

	if err := s.Become("seed", "taken"); !errors.Is(err, store.ErrBecomeToExists) {
		t.Fatalf("Become = %v, want ErrBecomeToExists", err)
	}
	seed, ok, err := s.GetAgent("seed")
	if err != nil || !ok {
		t.Fatalf("GetAgent(seed): %v (ok=%v)", err, ok)
	}
	if seed.Departed || seed.SupersededBy != "" {
		t.Fatalf("a refused claim retired the seed anyway: departed=%v superseded_by=%q",
			seed.Departed, seed.SupersededBy)
	}
	taken, _, err := s.GetAgent("taken")
	if err != nil {
		t.Fatalf("GetAgent(taken): %v", err)
	}
	if taken.Project != "other" || taken.SocketPath != "/other" {
		t.Fatalf("a refused claim overwrote the existing alias: %+v", taken)
	}
}

// testSessionUnreadLineage is the "mail follows the name" rule. become + resume
// leaves the retired seed's row on its OLD, now-dead tuple while the identity
// moved to a NEW one. A straggler addressed to the seed alias must still count
// against the session its identity moved to — the lineage walk, not a flat
// tuple match.
func testSessionUnreadLineage(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "seed", SocketPath: "/old", SessionID: "$old"})
	if err := s.Become("seed", "claimed"); err != nil {
		t.Fatalf("Become: %v", err)
	}
	// The claimed identity resumes on a new tuple; the seed's row stays put.
	mustRegister(t, s, store.Agent{Alias: "claimed", SocketPath: "/new", SessionID: "$new"})
	// A straggler still addressed to the retired seed alias.
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "seed",
	}, "addressed to the old name")

	total, _, err := s.SessionUnread("", "/new", "$new", liveCreated)
	if err != nil {
		t.Fatalf("SessionUnread: %v", err)
	}
	if total != 1 {
		t.Fatalf("unread on the new tuple = %d, want 1 — mail must follow the claimed name", total)
	}
}

func testSessionUnreadChainedLineage(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "a", SocketPath: "/old", SessionID: "$old"})
	if err := s.Become("a", "b"); err != nil {
		t.Fatalf("Become(a,b): %v", err)
	}
	if err := s.Become("b", "c"); err != nil {
		t.Fatalf("Become(b,c): %v", err)
	}
	mustRegister(t, s, store.Agent{Alias: "c", SocketPath: "/new", SessionID: "$new"})
	// Addressed to the ORIGINAL name, two claims back.
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "a",
	}, "addressed to the original name")

	total, _, err := s.SessionUnread("", "/new", "$new", liveCreated)
	if err != nil {
		t.Fatalf("SessionUnread: %v", err)
	}
	if total != 1 {
		t.Fatalf("unread across a two-step chain = %d, want 1", total)
	}
}

// testSessionAliasLineage: the alias list the hook drains must include retired
// seeds sitting on long-dead tuples, which is precisely what a flat tuple match
// misses. Departed aliases are included ON PURPOSE — their mail still needs
// draining.
func testSessionAliasLineage(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "a", SocketPath: "/old", SessionID: "$old"})
	if err := s.Become("a", "b"); err != nil {
		t.Fatalf("Become(a,b): %v", err)
	}
	if err := s.Become("b", "c"); err != nil {
		t.Fatalf("Become(b,c): %v", err)
	}
	mustRegister(t, s, store.Agent{Alias: "c", SocketPath: "/new", SessionID: "$new"})
	// A sibling on the same live tuple, plus an unrelated agent elsewhere.
	mustRegister(t, s, store.Agent{Alias: "sibling", SocketPath: "/new", SessionID: "$new"})
	mustRegister(t, s, store.Agent{Alias: "stranger", SocketPath: "/elsewhere", SessionID: "$x"})

	got, err := s.SessionAliasLineage("", "/new", "$new", liveCreated)
	if err != nil {
		t.Fatalf("SessionAliasLineage: %v", err)
	}
	want := []string{"a", "b", "c", "sibling"}
	if len(got) != len(want) {
		t.Fatalf("lineage = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lineage = %v, want %v (sorted, deduplicated)", got, want)
		}
	}
}

func testSessionAliasLineageEmpty(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{Alias: "a1", SocketPath: "", SessionID: ""})
	got, err := s.SessionAliasLineage("", "", "", liveCreated)
	if err != nil {
		t.Fatalf("SessionAliasLineage: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an empty session id must match nothing, got %v", got)
	}
}

// testSessionAliasLineageDevices: this op's answer is what the hook drains and
// the nudge path addresses, so a colliding tuple on ANOTHER machine must never
// appear in it.
func testSessionAliasLineageDevices(t *testing.T, s store.API) {
	const sock, sess = "/private/tmp/tmux-501/default", "$1"
	mustRegister(t, s, store.Agent{Alias: "mine", DeviceID: "dev-a", SocketPath: sock, SessionID: sess})
	mustRegister(t, s, store.Agent{Alias: "theirs", DeviceID: "dev-b", SocketPath: sock, SessionID: sess})

	got, err := s.SessionAliasLineage("dev-a", sock, sess, liveCreated)
	if err != nil {
		t.Fatalf("SessionAliasLineage: %v", err)
	}
	if len(got) != 1 || got[0] != "mine" {
		t.Fatalf("dev-a lineage = %v, want [mine] — the tuple is not device-unique", got)
	}
}

// --- the incarnation dimension ---------------------------------------------
//
// tmux recycles session IDs across server restarts, so (device, socket,
// session) names a SEQUENCE of unrelated sessions over a machine's lifetime.
// session_created picks the one running now. These cases are built like the
// device-collision ones above: the ghost and the live session share a
// GENUINELY IDENTICAL tuple, differing only in creation time, so a backend
// that dropped the dimension cannot pass them by accident.
//
// Every call below passes sessionCreated EXPLICITLY. It has to: a zero matches
// nothing and the methods answer empty rather than erroring (store.API), so a
// case that omitted it would assert against a plausible zero and prove
// nothing at all.

// testSetSessionLabelIncarnation: a label write lands on the session running
// now and never on a ghost from a dead tmux server that happened to be handed
// the same session id. Labels are addresses; relabelling a ghost is silent,
// relabelling the wrong live thing is not.
func testSetSessionLabelIncarnation(t *testing.T, s store.API) {
	const sock, sess = "/private/tmp/tmux-501/default", "$1"
	mustRegister(t, s, store.Agent{Alias: "current", SocketPath: sock, SessionID: sess, SessionCreated: liveCreated})
	mustRegister(t, s, store.Agent{Alias: "ghost", SocketPath: sock, SessionID: sess, SessionCreated: ghostCreated})

	n, err := s.SetSessionLabel("", sock, sess, liveCreated, "nfl-3", true)
	if err != nil {
		t.Fatalf("SetSessionLabel: %v", err)
	}
	if n != 1 {
		t.Fatalf("labelled %d rows, want 1 — only the proven incarnation is eligible", n)
	}
	ghost, _, err := s.GetAgent("ghost")
	if err != nil {
		t.Fatalf("GetAgent ghost: %v", err)
	}
	if ghost.Label != "" || ghost.LabelManual {
		t.Errorf("a dead incarnation's row was relabelled (label=%q manual=%v)", ghost.Label, ghost.LabelManual)
	}
}

// testSetSessionLabelZeroIncarnation: zero is the ABSENCE of proof, not a
// value to match on. A caller that cannot name its incarnation writes nothing
// — including onto legacy rows that also carry 0, which would otherwise make
// "no proof" match "no proof" and relabel exactly the rows that cannot be
// attributed.
func testSetSessionLabelZeroIncarnation(t *testing.T, s store.API) {
	const sock, sess = "/private/tmp/tmux-501/default", "$1"
	mustRegisterLegacy(t, s, store.Agent{Alias: "legacy", SocketPath: sock, SessionID: sess})
	mustRegister(t, s, store.Agent{Alias: "current", SocketPath: sock, SessionID: sess, SessionCreated: liveCreated})

	n, err := s.SetSessionLabel("", sock, sess, 0, "nfl-3", true)
	if err != nil {
		t.Fatalf("SetSessionLabel: %v", err)
	}
	if n != 0 {
		t.Fatalf("labelled %d rows with an unproven incarnation, want 0", n)
	}
	for _, alias := range []string{"legacy", "current"} {
		got, _, err := s.GetAgent(alias)
		if err != nil {
			t.Fatalf("GetAgent %s: %v", alias, err)
		}
		if got.Label != "" {
			t.Errorf("%s was relabelled %q by a caller that proved no incarnation", alias, got.Label)
		}
	}
}

// testSessionUnreadIncarnation: two incarnations of one session id each count
// their OWN mail. Without the dimension the ghost is a member of the live
// session, so its straggler mail inflates the live badge — and, worse, the
// ghost's alias counts as a sibling author, which is how a real message gets
// silently dropped from the count.
func testSessionUnreadIncarnation(t *testing.T, s store.API) {
	const sock, sess = "/private/tmp/tmux-501/default", "$1"
	mustRegister(t, s, store.Agent{Alias: "current", SocketPath: sock, SessionID: sess, SessionCreated: liveCreated})
	mustRegister(t, s, store.Agent{Alias: "ghost", SocketPath: sock, SessionID: sess, SessionCreated: ghostCreated})
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "ghost",
	}, "mail for a session that is gone")

	total, _, err := s.SessionUnread("", sock, sess, liveCreated)
	if err != nil {
		t.Fatalf("SessionUnread(live): %v", err)
	}
	if total != 0 {
		t.Fatalf("live incarnation unread = %d, want 0 — the dead incarnation's mail is not this session's", total)
	}

	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "current",
	}, "mail for the session running now")

	total, _, err = s.SessionUnread("", sock, sess, liveCreated)
	if err != nil {
		t.Fatalf("SessionUnread(live, again): %v", err)
	}
	if total != 1 {
		t.Fatalf("live incarnation unread = %d, want exactly its own 1", total)
	}
	// And symmetric: the ghost tuple still answers for itself, so this is a
	// partition of the mail rather than a filter that hides some of it.
	total, _, err = s.SessionUnread("", sock, sess, ghostCreated)
	if err != nil {
		t.Fatalf("SessionUnread(ghost): %v", err)
	}
	if total != 1 {
		t.Fatalf("dead incarnation unread = %d, want its own 1", total)
	}
}

// testSessionUnreadZeroIncarnation: an unprovable row seeds nothing. A legacy
// registration on a real tmux tuple cannot show which incarnation it belongs
// to, so it is indistinguishable from a ghost and must not be attributed —
// not even to a caller that also proves nothing.
func testSessionUnreadZeroIncarnation(t *testing.T, s store.API) {
	const sock, sess = "/private/tmp/tmux-501/default", "$1"
	mustRegisterLegacy(t, s, store.Agent{Alias: "legacy", SocketPath: sock, SessionID: sess})
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "legacy",
	}, "mail for an unprovable row")

	total, _, err := s.SessionUnread("", sock, sess, 0)
	if err != nil {
		t.Fatalf("SessionUnread: %v", err)
	}
	if total != 0 {
		t.Fatalf("unread = %d with created 0 on a real tmux tuple, want 0 — zero seeds nothing", total)
	}
}

// testSessionUnreadPanelessIgnoresIncarnation: the paneless tuple ("", harness
// UUID) is EXEMPT. A harness UUID is never recycled, so there is no second
// incarnation to tell apart and demanding proof would only break the one
// identity that needs none. Asserted for both a zero and a mismatched
// non-zero, so the exemption cannot be implemented as "0 means any".
func testSessionUnreadPanelessIgnoresIncarnation(t *testing.T, s store.API) {
	const uuid = "9f1c2f6e-0000-4000-8000-000000000042"
	mustRegister(t, s, store.Agent{Alias: "harness", SocketPath: "", SessionID: uuid})
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "harness",
	}, "mail for a paneless session")

	for _, created := range []int64{0, liveCreated, ghostCreated} {
		total, _, err := s.SessionUnread("", "", uuid, created)
		if err != nil {
			t.Fatalf("SessionUnread(created=%d): %v", created, err)
		}
		if total != 1 {
			t.Errorf("paneless unread with created=%d is %d, want 1 — the paneless tuple is exempt",
				created, total)
		}
	}
}

// testSessionAliasLineageIncarnation: the op the SessionStart hook drains and
// the nudge path addresses. Handing it a dead incarnation's aliases means
// telling a live session to drain, and address, somebody else's names.
func testSessionAliasLineageIncarnation(t *testing.T, s store.API) {
	const sock, sess = "/private/tmp/tmux-501/default", "$1"
	mustRegister(t, s, store.Agent{Alias: "current", SocketPath: sock, SessionID: sess, SessionCreated: liveCreated})
	mustRegister(t, s, store.Agent{Alias: "ghost", SocketPath: sock, SessionID: sess, SessionCreated: ghostCreated})

	got, err := s.SessionAliasLineage("", sock, sess, liveCreated)
	if err != nil {
		t.Fatalf("SessionAliasLineage: %v", err)
	}
	if len(got) != 1 || got[0] != "current" {
		t.Fatalf("lineage = %v, want [current] — a recycled session id is not one session", got)
	}
}

// testSessionAliasLineageZeroIncarnation: same rule as SessionUnread's, pinned
// separately because the two run different SQL on the SQLite backend, so a
// re-tightening regression in one would pass the other's test.
func testSessionAliasLineageZeroIncarnation(t *testing.T, s store.API) {
	const sock, sess = "/private/tmp/tmux-501/default", "$1"
	mustRegisterLegacy(t, s, store.Agent{Alias: "legacy", SocketPath: sock, SessionID: sess})

	got, err := s.SessionAliasLineage("", sock, sess, 0)
	if err != nil {
		t.Fatalf("SessionAliasLineage: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("lineage = %v with created 0 on a real tmux tuple, want empty", got)
	}
}

// testLineageCrossesIncarnations pins the OTHER half of the rule — the half
// nothing pinned before, so a future reviewer "fixing the inconsistency" by
// scoping the recursive step would have got a green suite and silently broken
// resume.
//
// Both scoping dimensions stop at the base case. Here a name is claimed on one
// machine under one tmux incarnation, and resumed on ANOTHER machine under
// ANOTHER incarnation: the walk must still reach back through superseded_by to
// the retired seed and count its mail. Lineage is identity, which crosses both
// machines and restarts; the tuple is location, which does not.
func testLineageCrossesIncarnations(t *testing.T, s store.API) {
	mustRegister(t, s, store.Agent{
		Alias: "seed", DeviceID: "laptop-a", SocketPath: "/private/tmp/tmux-501/default",
		SessionID: "$1", SessionCreated: ghostCreated,
	})
	if err := s.Become("seed", "claimed"); err != nil {
		t.Fatalf("Become: %v", err)
	}
	// Resumed elsewhere: different machine, different tmux server, and the
	// identical session id $1 that every fresh tmux hands out first.
	mustRegister(t, s, store.Agent{
		Alias: "claimed", DeviceID: "laptop-b", SocketPath: "/private/tmp/tmux-501/default",
		SessionID: "$1", SessionCreated: liveCreated,
	})
	mustThread(t, s, store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "seed",
	}, "still addressed to the old name")

	got, err := s.SessionAliasLineage("laptop-b", "/private/tmp/tmux-501/default", "$1", liveCreated)
	if err != nil {
		t.Fatalf("SessionAliasLineage: %v", err)
	}
	if len(got) != 2 || got[0] != "claimed" || got[1] != "seed" {
		t.Fatalf("lineage = %v, want [claimed seed] — the recursive step is scoped by NEITHER dimension", got)
	}

	total, _, err := s.SessionUnread("laptop-b", "/private/tmp/tmux-501/default", "$1", liveCreated)
	if err != nil {
		t.Fatalf("SessionUnread: %v", err)
	}
	if total != 1 {
		t.Fatalf("unread = %d, want 1 — mail must follow the name across both a machine and a restart", total)
	}
}
