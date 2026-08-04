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
// consistency is not just tolerable but the whole premise. An entry the index
// has not yet caught up on arrives on the next tick, under the same watermark,
// because the watermark only advances over entries this poll actually saw.
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
	// re-examined on every tick for ever.
	touched := make(map[int64]bool)
	for _, item := range items {
		e := itemToEntry(item)
		if e.ID > out.MaxEntryID {
			out.MaxEntryID = e.ID
		}
		touched[e.ThreadID] = true
	}
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

// localSessionAgents returns the agents on deviceID that could carry a tmux
// badge — live, with a non-empty tuple — with their per-item state re-read
// from the base table.
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
