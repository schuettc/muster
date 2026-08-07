package dynamostore

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/schuettc/muster/internal/clock"
	"github.com/schuettc/muster/internal/store"
)

// TestPollWatermarkNeverPassesAnUnseenEntry is the unit-level pin on the fix:
// the watermark stops at the first hole, because an entry below the floor is
// never examined again and its wake would be lost, not delayed. It needs no
// endpoint, so unlike the integration case below it runs under plain
// `just verify`.
func TestPollWatermarkNeverPassesAnUnseenEntry(t *testing.T) {
	cases := []struct {
		name  string
		since int64
		ids   []int64
		want  int64
	}{
		{"nothing seen holds the floor", 7, nil, 7},
		{"a gapless run advances to its max", 7, []int64{8, 9, 10}, 10},
		{"out of order arrival is still gapless", 7, []int64{10, 8, 9}, 10},
		{"a hole stops the watermark below it", 7, []int64{8, 10, 11}, 8},
		{"a hole at the very first id holds the floor", 7, []int64{9, 10, 11}, 7},
		{"two holes stop at the first", 7, []int64{8, 10, 11, 13}, 8},
		{"ids at or below since cannot move it", 7, []int64{5, 6, 7}, 7},
		{"the hole fills and the watermark catches up", 7, []int64{8, 9, 10, 11}, 11},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pollWatermark(tc.since, tc.ids); got != tc.want {
				t.Fatalf("pollWatermark(%d, %v) = %d, want %d", tc.since, tc.ids, got, tc.want)
			}
		})
	}
}

// TestPollWatermarkStallsOnlyUntilTheOverlapIsExceeded pins the two sides of
// the floor around one hole: still held one id short of the overlap, stepped
// over as soon as it is reached. Written as an explicit before/after pair
// because "off by one on the constant" is the mistake that would quietly
// shorten the tolerance to nothing.
func TestPollWatermarkStallsOnlyUntilTheOverlapIsExceeded(t *testing.T) {
	const since = 100
	hole := int64(since + 1) // 101 never commits
	// Visible: 102 .. 101+pollOverlap — the highest is exactly hole+pollOverlap-1,
	// so the floor (max-pollOverlap) is still below the hole and cannot pass it.
	held := pollWatermark(since, seq(hole+1, hole+pollOverlap-1))
	if held != since {
		t.Fatalf("watermark = %d, want %d — the hole at %d must still hold it", held, since, hole)
	}
	// One more entry above and the floor reaches the hole: it is now presumed
	// burnt rather than in flight, and the watermark steps over it.
	stepped := pollWatermark(since, seq(hole+1, hole+pollOverlap))
	if stepped <= since {
		t.Fatalf("watermark = %d, want > %d — a never-arriving id must not stall the poller for ever", stepped, since)
	}
}

// seq returns [from, to] inclusive, or nil when empty.
func seq(from, to int64) []int64 {
	var out []int64
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// TestDevicePollSurfacesAnEntryCommittingBelowTheWatermark is the regression
// test proper, and the reason the watermark is not max(ids): it reproduces the
// out-of-order commit deterministically against DynamoDB Local, exactly as
// TestMarkReadStillOvershootsWithinOnePartition does — an id allocated first,
// overtaken by a later write, committed afterwards.
//
// Before the fix the first poll returned the overtaker's id as the watermark,
// the late entry landed below it, and no later poll ever looked there again: the
// badge for mail this device was sent stayed dark for ever. The poller is the
// only wake path for cross-device mail, so "for ever" is literal.
//
// The overtaking traffic is addressed to another device on purpose. If it
// concerned dev-1 too, the second poll would report the session because of the
// OVERTAKER, and the test would pass with the bug still in place.
func TestDevicePollSurfacesAnEntryCommittingBelowTheWatermark(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if err := s.RegisterAgent(store.Agent{
		Alias: "local", DeviceID: "dev-1", SocketPath: "/tmp/tmux-501/default", SessionID: "$1",
	}); err != nil {
		t.Fatalf("register local: %v", err)
	}
	// Same tuple on another machine, which is the collision the device
	// dimension exists for: this agent must never put dev-1's session in scope.
	if err := s.RegisterAgent(store.Agent{
		Alias: "faraway", DeviceID: "dev-2", SocketPath: "/tmp/tmux-501/default", SessionID: "$1",
	}); err != nil {
		t.Fatalf("register faraway: %v", err)
	}

	mine, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "peer", ToKind: "agent", ToTarget: "local",
	}, "opener")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	drained, err := s.DevicePoll("dev-1", 0)
	if err != nil {
		t.Fatalf("DevicePoll (drain the opener): %v", err)
	}

	// A reply to the local agent allocates its id and stalls mid-write.
	lowID, err := s.nextID(ctx, "entry")
	if err != nil {
		t.Fatalf("nextID: %v", err)
	}
	// Traffic between two OTHER agents takes a higher id and lands first.
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "peer2", ToKind: "agent", ToTarget: "faraway",
	}, "overtakes the stalled reply"); err != nil {
		t.Fatalf("CreateThread (overtaker): %v", err)
	}

	// The poll in the gap must NOT hand back a watermark above the hole.
	gap, err := s.DevicePoll("dev-1", drained.MaxEntryID)
	if err != nil {
		t.Fatalf("DevicePoll (in the gap): %v", err)
	}
	if len(gap.Sessions) != 0 {
		t.Fatalf("sessions = %+v, want none — the overtaker is dev-2's mail", gap.Sessions)
	}
	if gap.MaxEntryID >= lowID {
		t.Fatalf("watermark = %d, which is at or past the un-committed id %d — "+
			"the entry below it can never be examined again", gap.MaxEntryID, lowID)
	}

	// The stalled reply finally commits, below the highest id already seen.
	now := clock.NowMillis()
	late := entryItem(mine, lowID, "peer", "the late reply", "", now, rcpt("agent", "local"))
	if err := s.appendTransact(ctx, late, mine, now, lowID, 0, "peer", true); err != nil {
		t.Fatalf("appendTransact (the late reply): %v", err)
	}

	got, err := s.DevicePoll("dev-1", gap.MaxEntryID)
	if err != nil {
		t.Fatalf("DevicePoll (after the late commit): %v", err)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].SessionID != "$1" {
		t.Fatalf("sessions = %+v, want $1 — the late entry's wake was dropped", got.Sessions)
	}
	// With the hole filled, the watermark is free to move all the way up, so the
	// next tick goes quiet rather than re-reporting for ever.
	quiet, err := s.DevicePoll("dev-1", got.MaxEntryID)
	if err != nil {
		t.Fatalf("DevicePoll (from the healed watermark): %v", err)
	}
	if len(quiet.Sessions) != 0 {
		t.Fatalf("sessions = %+v, want none — a filled gap must let the poll go quiet", quiet.Sessions)
	}
}

// TestDevicePollAttributesEntriesWithoutReadingGSI1 is the pin on Critical 1:
// DevicePoll must decide "whose mail is this" from the read that produced its
// watermark, never from a second index that can be staler.
//
// The interleaving it reproduces is CreateThread's. The metadata item and the
// thread's first entry go in one transaction, but they land in DIFFERENT
// indexes — metadata in gsi1 (the recipient partition), entry in gsi2 (the
// global entry log) — and DynamoDB promises no ordering between the two. So a
// poll can hold the entry while gsi1 still does not know the thread exists.
// The watermark then advances over that id and pollLoop never goes back: the
// wake is not late, it is gone. A cross-device task handoff is exactly this
// shape, with the sender blocking on a reply that will never come.
//
// DynamoDB Local replicates its indexes synchronously, so the skew has to be
// constructed rather than waited for: removing gsi1pk from the metadata item
// takes it out of gsi1 while leaving the base item and the entry untouched —
// precisely the state gsi1 is in during the window, and precisely what
// concerns() would read there. With the fix the poll never asks gsi1 about the
// address arms at all; it matches the entry's OWN denormalized gsi1pk, which
// gsi2 projects, against the partitions derived from the agent's row.
//
// Mutating DevicePoll back to `threads, _, err := s.concerns(...)` +
// `touched[id]` alone makes this fail with zero sessions.
func TestDevicePollAttributesEntriesWithoutReadingGSI1(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RegisterAgent(store.Agent{
		Alias: "local", DeviceID: "dev-1",
		SocketPath: "/tmp/tmux-501/default", SessionID: "$1",
	}); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	drained, err := s.DevicePoll("dev-1", 0)
	if err != nil {
		t.Fatalf("DevicePoll (drain): %v", err)
	}

	id, err := s.CreateThread(store.Thread{
		Kind: "task", FromAgent: "peer", ToKind: "agent", ToTarget: "local",
		Status: "open", Intent: store.IntentAction,
	}, "please take this")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Take the metadata item out of gsi1, leaving the entry and the base item
	// exactly as the transaction wrote them: gsi1 as it looks mid-window.
	if _, err := s.c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(s.table),
		Key:              map[string]types.AttributeValue{"pk": attrS(pkThread(id)), "sk": attrN(metaSK)},
		UpdateExpression: aws.String("REMOVE gsi1pk"),
	}); err != nil {
		t.Fatalf("un-index the metadata item: %v", err)
	}
	// Confirm the fixture actually built the skew — otherwise this test could
	// pass for the wrong reason.
	threads, _, err := s.concerns(ctx, store.Agent{Alias: "local"})
	if err != nil {
		t.Fatalf("concerns: %v", err)
	}
	if threads[id] != nil {
		t.Fatal("fixture did not remove the thread from gsi1; the skew is not being tested")
	}

	got, err := s.DevicePoll("dev-1", drained.MaxEntryID)
	if err != nil {
		t.Fatalf("DevicePoll: %v", err)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].SessionID != "$1" {
		t.Fatalf("sessions = %+v, want one $1 — the first entry of a new thread was seen "+
			"in gsi2 but attributed through gsi1, so its wake was dropped and the "+
			"watermark moved past it for good", got.Sessions)
	}
	if got.MaxEntryID <= drained.MaxEntryID {
		t.Fatalf("watermark did not advance: %d -> %d", drained.MaxEntryID, got.MaxEntryID)
	}
}

// TestDevicePollScopedBroadcastSkipsOtherProjects is the DynamoDB-level pin on
// Critical 2's wake half, complementing the conformance case: the address arms
// now match on the entry's denormalized partition, so a broadcast scoped to
// one project must not even be a candidate for another project's device.
func TestDevicePollScopedBroadcastSkipsOtherProjects(t *testing.T) {
	s := newTestStore(t)

	if err := s.RegisterAgent(store.Agent{
		Alias: "api-agent", Project: "api", DeviceID: "dev-api",
		SocketPath: "/tmp/tmux-501/default", SessionID: "$1",
	}); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "sender", ToKind: "broadcast", ToTarget: "web",
	}, "web only"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	got, err := s.DevicePoll("dev-api", 0)
	if err != nil {
		t.Fatalf("DevicePoll: %v", err)
	}
	if len(got.Sessions) != 0 {
		t.Fatalf("sessions = %+v, want none — a broadcast scoped to project web "+
			"must not wake a device that has only project api on it", got.Sessions)
	}
}
