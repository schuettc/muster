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
// device, and each one's role and project — the per-item state the concern
// predicate turns on — are re-read from the base table.
//
// # Attribution must not cross indexes
//
// The watermark moves over every id seen in gsi2, and it only moves forward
// (pollLoop refuses a backwards one), so an entry this poll fails to attribute
// to a session is an entry NO LATER POLL WILL EXAMINE. Attribution therefore
// has to be at least as fresh as the read that advanced the watermark, and
// gsi1 is not: CreateThread writes the metadata item and its first entry as
// two items in one transaction, and DynamoDB gives no mutual ordering between
// the two indexes those items land in. So a poll can hold a thread's first
// entry from gsi2 while gsi1 has not yet been told the thread is addressed to
// anybody — and "which threads concern me", read from gsi1, would answer
// nothing for it. That is a cross-device task handoff, whose sender is by then
// blocking on a reply, silently dropped: not late, never.
//
// The fix is that the entry item ALREADY CARRIES its address. entryItem
// denormalizes the thread's rcpt() into gsi1pk (this is the same
// denormalization unread math is built on), and gsi2 projects ALL — so the
// three address arms are decided by matching that attribute against
// directParts(a), which is computed from the agent's own row. Same read as the
// watermark, no second index, nothing to be stale.
//
// The originator arm still reads gsi1 through concerns, and is safe for a
// reason that does not generalise to the others: on a thread the local alias
// ORIGINATED, the local alias is the actor on the first entry, so there is
// nothing to wake it for during the window when only the first entry exists.
// By the time a peer replies, the metadata item is long since indexed.
//
// # What is NOT closed by this, and is NOT self-healing
//
// DevicePoll only NAMES the session. The daemon then recomputes that session's
// badge through SessionUnread (daemon.reconcileSessions -> setSessionBadge),
// which reads gsi1 unavoidably — both the thread's metadata item and the entry
// have to have replicated there for the count to be right. So a reconcile that
// lands while gsi1 is still behind counts zero, and setSessionBadge on zero
// does not merely leave the badge dark: it calls Notify's Clear. The watermark
// is already past the entry by then, so this poll will not name that session
// for it again.
//
// Nothing recovers that on its own. daemon.pollLoop reconciles only when a tick
// returns a non-empty Sessions list, and the only other reconcile trigger is a
// badge-moving write made ON THIS DEVICE (daemon.forward's triggerReconcile).
// So the badge relights only if more mail happens to arrive or the operator
// happens to write something — and the case this whole section is about, a
// cross-device handoff into an otherwise idle device whose sender is blocking
// on the reply, is precisely the case where neither happens. Do not describe
// this as "late": for that operator it is permanent.
//
// It is a much narrower window than the one above — it opens when this poll
// reads gsi2 and has to still be open one Lambda response plus one device
// round trip later, rather than any time after the transaction commits — but
// narrow is an empirical claim about replication speed, and DynamoDB bounds
// GSI lag at nothing at all. Treat it as a real hole, not a rounding error.
//
// Closing it is not a change here. The candidates, cheapest first: have the
// poller distrust a zero (DevicePoll asserted this session has mail, so a
// SessionUnread of zero is a contradiction, and the session should be
// re-reconciled on the next tick regardless of the watermark); or have
// DevicePoll answer with the unread counts it can already derive from the
// entries it holds, which removes the second read entirely but has to
// reproduce SessionUnread's per-alias watermarks, sibling exclusion and action
// counts to do it.
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
	touchedParts := make(map[string]bool)
	for _, item := range items {
		e := itemToEntry(item)
		ids = append(ids, e.ID)
		touched[e.ThreadID] = true
		// The entry's own denormalized recipient partition. gsi2 projects ALL
		// (secondaryIndex), so this poll ALREADY HOLDS the address of every
		// entry it saw, from the same read that produced the watermark. That is
		// what makes attribution below skew-free; see the doc comment.
		touchedParts[strAttr(item, "gsi1pk")] = true
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
		// The three ADDRESS arms, matched on the partition the entry itself
		// carries rather than on a set of threads read back out of gsi1. This
		// is the arm that must not depend on a second index — see the doc
		// comment's cross-index skew section — and directParts is derived from
		// a's own base-table row, so nothing here can be stale.
		hit := false
		for part := range directParts(a) {
			if touchedParts[part] {
				hit = true
				break
			}
		}
		if !hit {
			// The ORIGINATOR arm, which has no partition of its own: an entry
			// on a thread this alias originated lands in the RECIPIENT's
			// partition, so it can only be recognised by thread id. concerns is
			// the four-arm predicate itself — the SAME one Inbox and
			// UnreadCount reach through unreadFor — so calling it here is what
			// stops the poller and the inbox from disagreeing about what a wake
			// means. Its gsi1 read is safe for THIS arm specifically: see the
			// doc comment.
			threads, _, err := s.concerns(ctx, a)
			if err != nil {
				return store.DevicePollResult{MaxEntryID: sinceEntryID}, err
			}
			for id := range threads {
				if touched[id] {
					hit = true
					break
				}
			}
		}
		if hit {
			seen[ref] = true
			out.Sessions = append(out.Sessions, ref)
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
// Note what this does and does not buy, because the property it guarantees is
// narrower than "no entry is missed" and stating it as the latter is how the
// cross-index bug got in. What it guarantees is that every id is SEEN by some
// poll before the watermark passes it. Whether seeing it produces a wake is a
// separate question answered by attribution, and if attribution reads any
// source that can be staler than this poll's own gsi2 read, this rule does not
// save it: the id was seen, the watermark moved, and the entry is gone. That
// is precisely why the address arms in DevicePoll match on the entry's own
// projected gsi1pk rather than on a gsi1 lookup. Any new attribution path
// added here owes the same question an answer.
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
