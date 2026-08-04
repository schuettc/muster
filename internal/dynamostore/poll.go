package dynamostore

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/schuettc/muster/internal/store"
)

// DevicePoll answers which of one device's sessions have new mail, plus the
// watermark to resume from. See store.DevicePoll for the contract; this is the
// same behaviour over gsi2.
//
// The shape is chosen for the QUIET case, which is nearly every tick: one
// bounded Query on the ENTRIES partition (`gsi2sk > since`) that usually comes
// back empty, and then nothing else — no roster read, no per-alias fan-out.
// Work is only done once mail actually exists.
//
// Consistency: this is the one read whose decision the caller did NOT cause —
// the entries it looks for were written by another device — so eventual
// consistency is not just tolerable but the whole premise. What eventual
// consistency does NOT buy is that a missed entry simply shows up next tick:
// entries can become visible out of id order, so the watermark cannot be the
// highest id this poll happened to see. pollWatermark is where that is dealt
// with, and it is the whole reason this poll is not a one-liner.
//
// The membership-vs-state rule in the package comment is honoured the same
// way SessionUnread honours it: the roster identifies WHICH aliases are on the
// device, and each one's role — the per-item state the concern predicate turns
// on — is re-read from the base table.
func (s *Store) DevicePoll(deviceID string, sinceEntryID int64) (store.DevicePollResult, error) {
	ctx := backgroundCtx()
	out := store.DevicePollResult{MaxEntryID: sinceEntryID, Sessions: []store.SessionRef{}}

	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		IndexName:              aws.String(gsi2Name),
		KeyConditionExpression: aws.String("gsi2pk = :pk AND gsi2sk > :since"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": attrS(entriesPartition), ":since": attrN(sinceEntryID),
		},
	})
	if err != nil {
		return store.DevicePollResult{MaxEntryID: sinceEntryID}, fmt.Errorf("dynamostore: device poll entries after %d: %w", sinceEntryID, err)
	}

	// The watermark moves over every entry SEEN, not only the concerning ones
	// (see store.DevicePollResult): mail for somebody else must not be
	// re-examined on every tick for ever. How far it moves is pollWatermark's
	// decision, not "the highest id in this page".
	ids := make([]int64, 0, len(items))
	touched := make(map[int64]bool)
	for _, item := range items {
		e := itemToEntry(item)
		ids = append(ids, e.ID)
		touched[e.ThreadID] = true
	}
	out.MaxEntryID = pollWatermark(sinceEntryID, ids)
	if len(touched) == 0 {
		return out, nil
	}

	local, err := s.localSessionAgents(ctx, deviceID)
	if err != nil {
		return store.DevicePollResult{MaxEntryID: sinceEntryID}, err
	}

	seen := make(map[store.SessionRef]bool)
	for _, a := range local {
		ref := store.SessionRef{SocketPath: a.SocketPath, SessionID: a.SessionID}
		if seen[ref] {
			continue // a sibling alias already put this session on the list
		}
		// concerns is the four-arm predicate itself — the SAME one Inbox and
		// UnreadCount reach through unreadFor. Calling it rather than
		// re-deriving "addressed to me" here is what stops the poller and the
		// inbox from disagreeing about what a wake means.
		threads, _, err := s.concerns(ctx, a.Alias, a.Role)
		if err != nil {
			return store.DevicePollResult{MaxEntryID: sinceEntryID}, err
		}
		for id := range threads {
			if touched[id] {
				seen[ref] = true
				out.Sessions = append(out.Sessions, ref)
				break
			}
		}
	}
	sort.Slice(out.Sessions, func(i, j int) bool {
		if out.Sessions[i].SocketPath != out.Sessions[j].SocketPath {
			return out.Sessions[i].SocketPath < out.Sessions[j].SocketPath
		}
		return out.Sessions[i].SessionID < out.Sessions[j].SessionID
	})
	return out, nil
}

// pollOverlap bounds how far DevicePoll's watermark may lag the highest entry
// id it saw — the width of the re-scan that catches an entry which becomes
// visible out of id order.
//
// It exists because the contiguity rule below, on its own, can be stalled for
// ever by an id that will NEVER commit: nextID allocates before the write, so a
// writer that dies (or exhausts appendMaxRetries, or fails its condition) burns
// its id permanently, and a watermark that refuses to pass any hole would then
// re-scan the whole entry log on every tick, for ever. The floor is what makes
// that self-healing: the watermark steps over a hole once the HIGHEST id seen
// is more than pollOverlap above it, and contiguity resumes from there.
//
// Say that precisely, because the looser phrasing ("once pollOverlap later ids
// are visible") states a stronger property than the code implements and this
// file exists because of a comment that did exactly that. The floor keys off
// max(seen), not off how many ids between the hole and the max were actually
// returned — so a poll that saw ONLY id 1000 with since=0 would floor at 936
// and skip 1..936 having never reported them. What rules that out is not the
// rule below but a fact about the id space: nextID keeps a PER-NAME counter
// (agents.go), the "entry" counter is drawn only by CreateThread's opener,
// AppendEntry and ClaimTask, threads and events use their own, and no path
// ever deletes an entry item. Entry ids are therefore dense, so a run that
// wide being invisible at once cannot happen. If the entry id space ever stops
// being dense — a delete path, a shared counter, a reserved block — this floor
// stops being safe and the contiguity rule has to carry the whole weight.
//
// 64 is chosen from what has to fit inside it: every entry allocated-and-
// committed while one earlier allocation is still in flight. An in-flight
// AppendEntry holds its id for at most ~155ms of conflict backoff
// (appendBackoff over appendMaxRetries) plus its transact round trip and gsi2
// replication — call it a second at the pessimistic end — and muster's write
// rate is single-digit per MINUTE (see rosterPartition). So 64 is roughly four
// orders of magnitude of headroom over the expected case, and still tolerates a
// second-long stall inside a 60-writes-per-second burst this bus will never
// see. The cost of it being generous is one Query page of at most 64 small
// items on the ticks that follow a hole, and nothing at all otherwise: with no
// hole the watermark is the raw max and the poll goes quiet exactly as it did
// before (which is what testDevicePollFindsNewMail pins).
const pollOverlap = 64

// pollWatermark is the id DevicePoll answers with: the floor the NEXT poll
// resumes from, given the ids this one saw above since.
//
// It is deliberately not max(ids). Entry ids are allocated by nextID BEFORE the
// write they belong to commits (see the package comment's "MarkRead watermark
// can overshoot" section, and threads.go's advanceLast guard, which exists for
// exactly this), and gsi2 replication reorders on top of that. So a poll can
// see id 11 while id 10 has not landed. Resuming from 11 would mean entry 10 is
// never examined by any later poll, and since this poller is the ONLY wake path
// for cross-device mail on this backend, the badge that entry should have lit
// stays dark permanently — not late, never. That is the failure this function
// exists to prevent; it is also why the older "it arrives on the next tick,
// under the same watermark" reasoning was wrong, and worth remembering as a
// wrong belief that produced a bug: eventual consistency delays what a read
// SEES, and says nothing about the order ids become visible in.
//
// So the watermark is the end of the CONTIGUOUS run above since — an id is only
// passed once every id below it has been seen — with pollOverlap as a floor so
// a never-arriving id cannot stall it for ever. It never returns less than
// since: a backwards watermark would make a device re-read history (the poller
// refuses one anyway, see pollLoop).
//
// Re-scanning is free where it matters: an entry reported twice costs one
// reconcile, and reconcile recomputes the badge from stored state and writes
// the same value (ReconcileLocalSessions). Reporting one zero times costs the
// operator a message they are never told about.
func pollWatermark(since int64, ids []int64) int64 {
	above := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > since {
			above = append(above, id)
		}
	}
	if len(above) == 0 {
		return since
	}
	sort.Slice(above, func(i, j int) bool { return above[i] < above[j] })

	contiguous := since
	for _, id := range above {
		if id == contiguous+1 {
			contiguous = id
		}
	}
	// above is sorted, so the last element is the highest id seen.
	if floor := above[len(above)-1] - pollOverlap; floor > contiguous {
		return floor
	}
	return contiguous
}

// localSessionAgents returns the agents on deviceID that could carry a tmux
// badge — live, with a non-empty tuple — with their per-item state re-read
// from the base table.
//
// Departed agents are skipped here while SessionUnread deliberately keeps
// counting theirs (store.SessionUnread) — an intentional divergence, not an
// oversight in one of the two. A tombstoned agent's mail still waits for it, so
// its unread count must stay honest; what is pointless is PUSHING a badge into
// a session nobody is watching anymore. That is the same line local mode draws,
// where notifyForThread journals a departed recipient as "skipped: departed"
// (daemon.notifyForThread) rather than dropping it from unread math.
//
// The roster is used for MEMBERSHIP only (it is a GSI, so eventually
// consistent), and each member is then re-read strongly consistently through
// agentByAlias: the concern predicate turns on the agent's ROLE, and a role
// the index copy has not caught up on would silently change which threads
// count as mail. Same rule, same reason, as SessionUnread's per-member re-read.
func (s *Store) localSessionAgents(ctx context.Context, deviceID string) ([]store.Agent, error) {
	items, err := s.roster(ctx)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for _, item := range items {
		a := itemToAgent(item)
		if a.DeviceID != deviceID || a.Departed || a.SocketPath == "" || a.SessionID == "" {
			continue
		}
		candidates = append(candidates, a.Alias)
	}
	sort.Strings(candidates)

	var out []store.Agent
	for _, alias := range candidates {
		a, found, err := s.agentByAlias(ctx, alias)
		if err != nil {
			return nil, err
		}
		if !found || a.DeviceID != deviceID || a.Departed || a.SocketPath == "" || a.SessionID == "" {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}
