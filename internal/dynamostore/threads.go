package dynamostore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/schuettc/muster/internal/clock"
	"github.com/schuettc/muster/internal/store"
)

const (
	// threadsPartition is gsi2's partition holding every thread's metadata
	// item, keyed by thread id. It is what makes Threads() one query instead
	// of a table scan, and it is disjoint from entriesPartition because only
	// metadata items carry it.
	threadsPartition = "THREADS"

	// entriesPartition is gsi2's global entry log, in entry-id order — the
	// partition device_poll reads to find mail for one device.
	entriesPartition = "ENTRIES"
)

// validIntent reports whether intent is a value CreateThread accepts: ""
// (unspecified) or one of the three named intents. This mirrors the SQLite
// backend's own validIntent (internal/store/threads.go), which is unexported;
// the vocabulary itself is shared via the store.Intent* constants, so the two
// cannot disagree on the values, only on the shape of the check.
func validIntent(intent string) bool {
	switch intent {
	case "", store.IntentFYI, store.IntentReply, store.IntentAction:
		return true
	default:
		return false
	}
}

// effectiveIntent is the Go translation of the SQLite backend's canonical
// effectiveIntent SQL fragment: a task is a request for action, including
// every pre-existing task row stored with intent "" — so a task with no
// explicit intent counts as action-requested, never unspecified. Every READ
// surface must go through it; itemToThread applies it once so no surface can
// forget.
func effectiveIntent(kind, intent string) string {
	if kind == "task" && intent == "" {
		return store.IntentAction
	}
	return intent
}

// clampThreadsLimit enforces Threads()'s documented range: <=0 defaults to
// 100, anything over 500 clamps to 500. Mirrors the SQLite backend's
// clampThreadsLimit, which is unexported.
func clampThreadsLimit(limit int) int {
	switch {
	case limit <= 0:
		return 100
	case limit > 500:
		return 500
	default:
		return limit
	}
}

func threadKey(id int64) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": attrS(pkThread(id)),
		"sk": attrN(metaSK),
	}
}

// threadMetaItem builds a thread's metadata item.
//
// It carries three index placements, one per access pattern the SQLite
// backend gets from a join:
//
//	pk/sk        THREAD#<id> / 0 — GetThread is one query over this partition,
//	             with metadata sorting before every entry.
//	gsi1         the thread's RECIPIENT partition at sort key 0 — "which
//	             threads are addressed to me" is one query that reads no
//	             entries at all (entries in that partition start at id 1).
//	gsi2         THREADS / <id> — Threads() is one query, never a scan.
//
// last_entry_id / last_from / last_at / entry_count are the denormalized form
// of the SQLite backend's threadLastEntryCTE. They are maintained on write
// (see AppendEntry) rather than aggregated on read because Threads() runs on
// station's once-a-second polling cadence, and a per-thread fan-out query
// there would be up to 500 round trips per poll.
func threadMetaItem(t store.Thread, threadID, firstEntryID, now int64) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk":             attrS(pkThread(threadID)),
		"sk":             attrN(metaSK),
		"id":             attrN(threadID),
		"kind":           attrS(t.Kind),
		"from_agent":     attrS(t.FromAgent),
		"to_kind":        attrS(t.ToKind),
		"to_target":      attrS(t.ToTarget),
		"subject":        attrS(t.Subject),
		"ref":            attrS(t.Ref),
		"status":         attrS(t.Status),
		"intent":         attrS(t.Intent), // RAW; effectiveIntent applies on read
		"created_at":     attrN(now),
		"updated_at":     attrN(now),
		"origin_project": attrS(t.OriginProject),
		"entry_count":    attrN(1),
		"last_entry_id":  attrN(firstEntryID),
		"last_from":      attrS(t.FromAgent),
		"last_at":        attrN(now),
		"gsi1pk":         attrS(rcpt(t.ToKind, t.ToTarget)),
		"gsi1sk":         attrN(metaSK),
		"gsi2pk":         attrS(threadsPartition),
		"gsi2sk":         attrN(threadID),
	}
}

// entryItem builds one entry item. recipient is the thread's rcpt() value,
// denormalized onto the entry: that is what turns "what is unread for me"
// into a sort-key-bounded query with no join, and it is why AppendEntry reads
// the thread's metadata before writing.
//
// It is FROZEN at write time. See "The recipient denormalization is
// write-once" in the package comment: re-addressing a thread without rewriting
// gsi1pk on every existing entry strands them in the old recipient's
// partition, and unread under-counts silently.
func entryItem(threadID, entryID int64, fromAgent, body, statusChange string, now int64, recipient string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk":            attrS(pkThread(threadID)),
		"sk":            attrN(entryID),
		"id":            attrN(entryID),
		"thread_id":     attrN(threadID),
		"from_agent":    attrS(fromAgent),
		"body":          attrS(body),
		"status_change": attrS(statusChange),
		"created_at":    attrN(now),
		"gsi1pk":        attrS(recipient),
		"gsi1sk":        attrN(entryID),
		"gsi2pk":        attrS(entriesPartition),
		"gsi2sk":        attrN(entryID),
	}
}

// itemToThread reads a metadata item. Intent is ALWAYS the effective intent —
// every read surface funnels through here, so the "one vocabulary everywhere"
// rule cannot be dropped on one of them. The query-time-only annotation
// fields (LastFrom/LastAt/EntryCount/Unread) are deliberately left zero;
// Threads() and Inbox() fill them explicitly, GetThread and CreateThread do
// not.
func itemToThread(item map[string]types.AttributeValue) store.Thread {
	kind := strAttr(item, "kind")
	return store.Thread{
		ID:            numAttr(item, "id"),
		Kind:          kind,
		FromAgent:     strAttr(item, "from_agent"),
		ToKind:        strAttr(item, "to_kind"),
		ToTarget:      strAttr(item, "to_target"),
		Subject:       strAttr(item, "subject"),
		Ref:           strAttr(item, "ref"),
		Status:        strAttr(item, "status"),
		Intent:        effectiveIntent(kind, strAttr(item, "intent")),
		CreatedAt:     numAttr(item, "created_at"),
		UpdatedAt:     numAttr(item, "updated_at"),
		OriginProject: strAttr(item, "origin_project"),
	}
}

// annotateLastEntry fills the query-time-only last-entry fields from the
// denormalized attributes — the counterpart of the SQLite backend's
// threadLastEntryCTE/Join. Only Threads() and Inbox() call it.
func annotateLastEntry(t *store.Thread, item map[string]types.AttributeValue) {
	t.LastFrom = strAttr(item, "last_from")
	t.LastAt = numAttr(item, "last_at")
	t.EntryCount = int(numAttr(item, "entry_count"))
}

func itemToEntry(item map[string]types.AttributeValue) store.Entry {
	return store.Entry{
		ID:           numAttr(item, "id"),
		ThreadID:     numAttr(item, "thread_id"),
		FromAgent:    strAttr(item, "from_agent"),
		Body:         strAttr(item, "body"),
		StatusChange: strAttr(item, "status_change"),
		CreatedAt:    numAttr(item, "created_at"),
	}
}

// sortThreadsRecent applies the SQLite ORDER BY: updated_at DESC, ties broken
// by id DESC. DynamoDB cannot order on a non-key attribute, so this happens in
// memory. (Inbox's SQL orders by updated_at alone and leaves ties to SQLite;
// applying the same id tie-break here just makes that arbitrary order
// deterministic.)
func sortThreadsRecent(threads []store.Thread) {
	sort.Slice(threads, func(i, j int) bool {
		if threads[i].UpdatedAt != threads[j].UpdatedAt {
			return threads[i].UpdatedAt > threads[j].UpdatedAt
		}
		return threads[i].ID > threads[j].ID
	})
}

// queryAll runs a Query to exhaustion, following LastEvaluatedKey. DynamoDB
// pages at 1MB regardless of how few items match a FilterExpression, so every
// unbounded read in this package goes through here rather than trusting a
// single page.
func (s *Store) queryAll(ctx context.Context, in *dynamodb.QueryInput) ([]map[string]types.AttributeValue, error) {
	var items []map[string]types.AttributeValue
	var start map[string]types.AttributeValue
	for {
		in.ExclusiveStartKey = start
		out, err := s.c.Query(ctx, in)
		if err != nil {
			return nil, err
		}
		items = append(items, out.Items...)
		if len(out.LastEvaluatedKey) == 0 {
			return items, nil
		}
		start = out.LastEvaluatedKey
	}
}

// CreateThread writes the thread's metadata item and its first entry in one
// TransactWriteItems, so a thread never exists without its opening entry —
// the DynamoDB expression of the SQLite transaction.
func (s *Store) CreateThread(t store.Thread, firstBody string) (int64, error) {
	if !validIntent(t.Intent) {
		return 0, fmt.Errorf("invalid intent %q", t.Intent)
	}
	ctx := backgroundCtx()
	now := clock.NowMillis()

	threadID, err := s.nextID(ctx, "thread")
	if err != nil {
		return 0, err
	}
	entryID, err := s.nextID(ctx, "entry")
	if err != nil {
		return 0, err
	}

	_, err = s.c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{
				TableName: aws.String(s.table),
				Item:      threadMetaItem(t, threadID, entryID, now),
			}},
			{Put: &types.Put{
				TableName: aws.String(s.table),
				Item: entryItem(threadID, entryID, t.FromAgent, firstBody, "", now,
					rcpt(t.ToKind, t.ToTarget)),
			}},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("dynamostore: create thread: %w", err)
	}
	return threadID, nil
}

// AppendEntry adds an entry and advances the thread's updated_at.
//
// The metadata read comes FIRST for two reasons: the entry must carry its
// thread's recipient (the gsi1 denormalization), and an unknown thread must
// return ErrThreadNotFound having written nothing — the SQLite version rolls
// its insert back, so no orphan entry may survive here either. The entry id is
// allocated only after that check, so a missing thread does not burn one.
func (s *Store) AppendEntry(threadID int64, fromAgent, body, statusChange string) (int64, error) {
	ctx := backgroundCtx()
	meta, err := s.threadMeta(ctx, threadID)
	if err != nil {
		return 0, err
	}
	now := clock.NowMillis()
	entryID, err := s.nextID(ctx, "entry")
	if err != nil {
		return 0, err
	}
	entry := entryItem(threadID, entryID, fromAgent, body, statusChange, now,
		rcpt(strAttr(meta, "to_kind"), strAttr(meta, "to_target")))

	// advanceLast starts true: the write claims the last-entry trio, guarded so
	// it may only move FORWARD. That guard is the MAX(entries.id) rule made
	// durable — two entries allocated in order but committed out of order would
	// otherwise leave the LOWER id recorded as the thread's last entry,
	// precisely the mis-pick the SQLite comment forbids.
	advanceLast := true
	for attempt := 0; ; attempt++ {
		err := s.appendTransact(ctx, entry, threadID, now, entryID, fromAgent, advanceLast)
		switch {
		case err == nil:
			return entryID, nil

		case isTransactionConflict(err) && attempt < appendMaxRetries:
			// Another writer held the thread item mid-transaction. Nothing was
			// written, so retrying the whole thing is safe.
			time.Sleep(appendBackoff(attempt))

		case isTransactionConditionFailed(err) && advanceLast:
			// Either the thread vanished under us, or a higher-id entry
			// already claimed "last". Re-read to tell them apart; if the
			// thread is still there, stop competing for the trio and just
			// record the entry.
			if _, mErr := s.threadMeta(ctx, threadID); mErr != nil {
				return 0, mErr
			}
			advanceLast = false

		case isTransactionConditionFailed(err):
			return 0, store.ErrThreadNotFound

		default:
			return 0, fmt.Errorf("dynamostore: append entry to thread %d: %w", threadID, err)
		}
	}
}

// appendMaxRetries bounds AppendEntry's conflict retries. Conflicts resolve in
// milliseconds; anything past this is a signal, not a hiccup.
const appendMaxRetries = 5

// appendBackoff is the wait before retrying a conflicted append: 5ms doubling
// to 80ms. Deliberately short — a conflict means a sibling transaction on the
// same thread just committed, not that the service is overloaded.
func appendBackoff(attempt int) time.Duration {
	return 5 * time.Millisecond << attempt
}

// appendTransact writes the entry and bumps the thread in one transaction.
// When advanceLast is false the last-entry trio is left alone (a higher-id
// entry already won that race) and only updated_at and the entry count move.
func (s *Store) appendTransact(ctx context.Context, entry map[string]types.AttributeValue,
	threadID, now, entryID int64, fromAgent string, advanceLast bool,
) error {
	upd := &types.Update{
		TableName: aws.String(s.table),
		Key:       threadKey(threadID),
		ExpressionAttributeNames: map[string]string{
			"#updated_at":  "updated_at",
			"#entry_count": "entry_count",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now": attrN(now),
			":one": attrN(1),
		},
		ConditionExpression: aws.String("attribute_exists(pk)"),
		UpdateExpression:    aws.String("SET #updated_at = :now ADD #entry_count :one"),
	}
	if advanceLast {
		upd.ExpressionAttributeNames["#last_entry_id"] = "last_entry_id"
		upd.ExpressionAttributeNames["#last_from"] = "last_from"
		upd.ExpressionAttributeNames["#last_at"] = "last_at"
		upd.ExpressionAttributeValues[":eid"] = attrN(entryID)
		upd.ExpressionAttributeValues[":from"] = attrS(fromAgent)
		upd.UpdateExpression = aws.String(
			"SET #updated_at = :now, #last_entry_id = :eid, #last_from = :from, #last_at = :now " +
				"ADD #entry_count :one")
		upd.ConditionExpression = aws.String(
			"attribute_exists(pk) AND (attribute_not_exists(#last_entry_id) OR #last_entry_id < :eid)")
	}
	_, err := s.c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{TableName: aws.String(s.table), Item: entry}},
			{Update: upd},
		},
	})
	return err
}

// isTransactionConditionFailed reports whether a TransactWriteItems was
// cancelled because one of its ConditionExpressions failed. The SDK surfaces
// this as a TransactionCanceledException carrying per-item reasons, NOT as the
// ConditionalCheckFailedException that isConditionFailed matches — so a
// transaction needs its own check.
func isTransactionConditionFailed(err error) bool {
	return hasCancellationCode(err, "ConditionalCheckFailed")
}

// isTransactionConflict reports whether a TransactWriteItems was cancelled
// because ANOTHER transaction was operating on one of its items — what real
// DynamoDB does when two AppendEntry calls touch the same thread at once.
//
// It must be handled here rather than left to the SDK: the standard retryer's
// list carries TransactionInProgressException but NOT TransactionCanceledException,
// so a conflict surfaces to the caller as a hard failure. DynamoDB Local never
// produces one either, so an unhandled conflict looks perfect in every test
// and silently drops entries in production.
func isTransactionConflict(err error) bool {
	return hasCancellationCode(err, "TransactionConflict")
}

func hasCancellationCode(err error, code string) bool {
	var cancelled *types.TransactionCanceledException
	if !errors.As(err, &cancelled) {
		return false
	}
	for _, reason := range cancelled.CancellationReasons {
		if aws.ToString(reason.Code) == code {
			return true
		}
	}
	return false
}

// threadMeta reads one thread's metadata item, strongly consistent (it gates
// AppendEntry's not-found contract, so an eventually-consistent read could
// reject an entry on a thread that demonstrably exists).
func (s *Store) threadMeta(ctx context.Context, id int64) (map[string]types.AttributeValue, error) {
	out, err := s.c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            threadKey(id),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("dynamostore: get thread %d: %w", id, err)
	}
	if len(out.Item) == 0 {
		return nil, store.ErrThreadNotFound
	}
	return out.Item, nil
}

// GetThread returns the thread and its entries, ordered by id. One query over
// the THREAD# partition: the metadata item sits at sort key 0 and the entries
// follow at their own ids, so DynamoDB returns them already ordered.
// LastFrom/LastAt/EntryCount/Unread are left zero, matching the SQLite
// backend.
func (s *Store) GetThread(id int64) (store.Thread, []store.Entry, error) {
	items, err := s.queryAll(backgroundCtx(), &dynamodb.QueryInput{
		TableName:                 aws.String(s.table),
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": attrS(pkThread(id))},
		ConsistentRead:            aws.Bool(true),
	})
	if err != nil {
		return store.Thread{}, nil, fmt.Errorf("dynamostore: get thread %d: %w", id, err)
	}
	var t store.Thread
	var entries []store.Entry
	found := false
	for _, item := range items {
		if numAttr(item, "sk") == metaSK {
			t = itemToThread(item)
			found = true
			continue
		}
		entries = append(entries, itemToEntry(item))
	}
	if !found {
		return store.Thread{}, nil, store.ErrThreadNotFound
	}
	return t, entries, nil
}

// Threads returns the most recently updated threads (updated_at DESC, ties by
// id DESC), limit clamped via clampThreadsLimit, each annotated with its last
// entry and entry count. One query over gsi2's THREADS partition; the ordering
// and the limit are applied in memory because DynamoDB can order only on a
// sort key, and updated_at is not one.
func (s *Store) Threads(limit int) ([]store.Thread, error) {
	limit = clampThreadsLimit(limit)
	items, err := s.queryAll(backgroundCtx(), &dynamodb.QueryInput{
		TableName:                 aws.String(s.table),
		IndexName:                 aws.String(gsi2Name),
		KeyConditionExpression:    aws.String("gsi2pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": attrS(threadsPartition)},
	})
	if err != nil {
		return nil, fmt.Errorf("dynamostore: list threads: %w", err)
	}
	var out []store.Thread
	for _, item := range items {
		t := itemToThread(item)
		annotateLastEntry(&t, item)
		out = append(out, t)
	}
	sortThreadsRecent(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// concerns is the ONE canonical DynamoDB expression of the SQLite backend's
// threadConcerns predicate — "does this thread concern alias": addressed to it
// directly, to its role, broadcast, or ORIGINATED by it. Inbox, UnreadCount
// and SessionUnread all reach it through unreadFor, so they cannot drift
// apart; the surfaces diverging is exactly how replies to originated threads
// once went invisible.
//
// It returns the concerning threads' metadata items keyed by id, plus the set
// of gsi1 recipient partitions that between them hold EVERY entry of those
// threads. That second value is what keeps unread math bounded: an entry on a
// thread this alias originated lives in the RECIPIENT's partition, not the
// alias's, so the originator arm contributes its threads' recipients to the
// set rather than forcing a per-thread query.
func (s *Store) concerns(ctx context.Context, alias, role string) (map[int64]map[string]types.AttributeValue, []string, error) {
	parts := map[string]bool{
		rcpt("agent", alias):  true,
		rcpt("broadcast", ""): true,
	}
	// The SQL guards the role arm with to_target != '', so an agent with no
	// role never matches role-addressed threads.
	if role != "" {
		parts[rcpt("role", role)] = true
	}

	threads := make(map[int64]map[string]types.AttributeValue)
	for part := range parts {
		// gsi1sk = 0 selects metadata items only: "which threads are addressed
		// here", reading none of their entries.
		items, err := s.queryAll(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			IndexName:              aws.String(gsi1Name),
			KeyConditionExpression: aws.String("gsi1pk = :pk AND gsi1sk = :meta"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": attrS(part), ":meta": attrN(metaSK),
			},
		})
		if err != nil {
			return nil, nil, fmt.Errorf("dynamostore: threads for %q: %w", part, err)
		}
		for _, item := range items {
			threads[numAttr(item, "id")] = item
		}
	}

	// The originator arm. There is no index partitioned by from_agent — a
	// metadata item already spends gsi1 on its recipient and gsi2 on the
	// global thread list — so this filters the THREADS partition instead. Same
	// read cost as Threads(), which is the surface this backend already
	// accepts on a polling cadence.
	originated, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:                aws.String(s.table),
		IndexName:                aws.String(gsi2Name),
		KeyConditionExpression:   aws.String("gsi2pk = :pk"),
		FilterExpression:         aws.String("#from_agent = :alias"),
		ExpressionAttributeNames: map[string]string{"#from_agent": "from_agent"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": attrS(threadsPartition), ":alias": attrS(alias),
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("dynamostore: threads originated by %q: %w", alias, err)
	}
	for _, item := range originated {
		threads[numAttr(item, "id")] = item
		parts[rcpt(strAttr(item, "to_kind"), strAttr(item, "to_target"))] = true
	}

	ordered := make([]string, 0, len(parts))
	for part := range parts {
		ordered = append(ordered, part)
	}
	sort.Strings(ordered)
	return threads, ordered, nil
}

// entriesAfter reads every entry newer than the watermark from the given gsi1
// recipient partitions. The sort-key bound is the whole point of
// denormalizing the recipient onto entries: unread is "the tail of a few
// partitions", not a scan. Metadata items sit at sort key 0 and entry ids
// start at 1, so `> after` never returns one even when after is 0.
func (s *Store) entriesAfter(ctx context.Context, parts []string, after int64) ([]store.Entry, error) {
	var out []store.Entry
	for _, part := range parts {
		items, err := s.queryAll(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			IndexName:              aws.String(gsi1Name),
			KeyConditionExpression: aws.String("gsi1pk = :pk AND gsi1sk > :after"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": attrS(part), ":after": attrN(after),
			},
		})
		if err != nil {
			return nil, fmt.Errorf("dynamostore: entries after %d in %q: %w", after, part, err)
		}
		for _, item := range items {
			out = append(out, itemToEntry(item))
		}
	}
	return out, nil
}

// unreadByThread is the unread predicate itself, scoped per thread: entries
// with an id STRICTLY greater than the watermark, on a thread that concerns
// the caller, written by someone the caller is not. Judging entry ids rather
// than the thread's updated_at is what stops an agent's own reply from
// re-flagging its own inbox, and lets a peer's reply on a thread the agent
// originated flag it. exclude holds every alias that counts as "the caller" —
// one alias for Inbox/UnreadCount, all of a session's aliases for
// SessionUnread.
//
// Threads with no qualifying entry are absent from the result, so its length
// is UnreadCount's answer.
func unreadByThread(entries []store.Entry, concerning map[int64]bool, exclude map[string]bool, after int64) map[int64]int {
	out := make(map[int64]int)
	for _, e := range entries {
		if e.ID <= after || !concerning[e.ThreadID] || exclude[e.FromAgent] {
			continue
		}
		out[e.ThreadID]++
	}
	return out
}

// unreadFor gathers everything Inbox and UnreadCount need, so the two cannot
// disagree about which threads reach an alias: the concerning threads' items
// and the per-thread unread counts relative to alias's own watermark.
func (s *Store) unreadFor(ctx context.Context, alias string) (map[int64]map[string]types.AttributeValue, map[int64]int, error) {
	// Strongly consistent by way of agentByAlias: unread_count can be called
	// immediately after get_inbox's MarkRead wrote this very watermark, and a
	// stale one would report mail the caller has already drained.
	agent, _, err := s.agentByAlias(ctx, alias)
	if err != nil {
		return nil, nil, err
	}
	// An unregistered alias has no role and no watermark — the SQL's role
	// subquery yields NULL and its watermark COALESCEs to 0, so everything
	// addressed to it or originated by it is unread.
	threads, parts, err := s.concerns(ctx, alias, agent.Role)
	if err != nil {
		return nil, nil, err
	}
	entries, err := s.entriesAfter(ctx, parts, agent.LastReadEntryID)
	if err != nil {
		return nil, nil, err
	}
	return threads, unreadByThread(entries, idSet(threads), map[string]bool{alias: true}, agent.LastReadEntryID), nil
}

func idSet(threads map[int64]map[string]types.AttributeValue) map[int64]bool {
	out := make(map[int64]bool, len(threads))
	for id := range threads {
		out[id] = true
	}
	return out
}

// Inbox returns every thread that concerns alias (see concerns): addressed to
// it directly, to its role, broadcast, or originated by it — so replies on
// threads the agent started show up here too. Thread.Intent is the EFFECTIVE
// intent. Each row's LastFrom/LastAt/EntryCount come from the thread's last
// entry, so a caller can tell "a peer replied" from "my own last send" without
// a second get_thread round trip, and Unread carries the caller-relative count
// of entries after alias's watermark not written by alias. The daemon MUST
// call Inbox() before MarkRead so callers see a non-zero count before their
// own read clears it.
func (s *Store) Inbox(alias string) ([]store.Thread, error) {
	ctx := backgroundCtx()
	threads, unread, err := s.unreadFor(ctx, alias)
	if err != nil {
		return nil, err
	}
	var out []store.Thread
	for id, item := range threads {
		t := itemToThread(item)
		annotateLastEntry(&t, item)
		t.Unread = unread[id]
		out = append(out, t)
	}
	sortThreadsRecent(out)
	return out, nil
}

// UnreadCount returns how many threads concerning alias contain an entry newer
// than its watermark written by someone else — the same predicate Inbox
// annotates each row with, reached through the same code path.
func (s *Store) UnreadCount(alias string) (int, error) {
	_, unread, err := s.unreadFor(backgroundCtx(), alias)
	if err != nil {
		return 0, err
	}
	return len(unread), nil
}

// MarkRead records that alias has read its inbox up to the highest entry id
// the alias could actually have been SHOWN — the entries Inbox reads, from the
// alias's own gsi1 recipient partitions, filtered to threads that concern it.
// The watermark is an entry id, not a wall-clock timestamp, so two entries
// landing in the same millisecond never race a strict "after last read"
// comparison. last_read_at is stamped for display only; no unread predicate
// consults it.
//
// Three properties this must not lose:
//
//   - The watermark comes from WRITTEN entries, never from the entry counter.
//     A counter value can already be allocated to an entry still in flight,
//     and treating that as read would swallow its unread signal outright.
//     entriesAfter reads index items, which exist only once a write committed.
//   - It reads the SAME index Inbox reads (gsi1). Deriving it from the global
//     entry log (gsi2) instead meant one write's two index replications could
//     disagree, so a get_inbox could mark read an entry Inbox had not shown.
//   - It never moves the watermark DOWN. agentByAlias is strongly consistent,
//     so the floor is the alias's current watermark and an eventually
//     consistent index that lags cannot re-surface already-read entries. (Two
//     MarkReads for the same alias concurrently can still interleave — the
//     write is not conditional on the old value, because condition failure
//     already means "unknown alias" here.)
//
// What remains: writers racing into the alias's own partitions. See "The
// MarkRead watermark can overshoot" in the package comment.
//
// The condition mirrors the SQLite UPDATE-matches-no-rows contract: DynamoDB's
// UpdateItem is an upsert, so without it, marking an unknown alias read would
// CREATE an agent row holding nothing but a watermark. It is kept alongside
// the unknown-alias early return because the agent can be deleted between the
// read and this write.
func (s *Store) MarkRead(alias string) error {
	ctx := backgroundCtx()
	agent, ok, err := s.agentByAlias(ctx, alias)
	if err != nil {
		return err
	}
	if !ok {
		return nil // unknown alias: no-op, matching the SQLite contract
	}
	watermark, err := s.readableThrough(ctx, alias, agent.Role, agent.LastReadEntryID)
	if err != nil {
		return err
	}
	_, err = s.c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key:       agentKey(alias),
		UpdateExpression: aws.String(
			"SET #last_read_entry_id = :id, #last_read_at = :now"),
		ConditionExpression: aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames: map[string]string{
			"#last_read_entry_id": "last_read_entry_id",
			"#last_read_at":       "last_read_at",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":id":  attrN(watermark),
			":now": attrN(clock.NowMillis()),
		},
	})
	if err != nil {
		if isConditionFailed(err) {
			return nil // unknown alias: no-op, matching the SQLite contract
		}
		return fmt.Errorf("dynamostore: mark read %q: %w", alias, err)
	}
	return nil
}

// readableThrough is the watermark MarkRead writes: the highest id among the
// entries visible to alias right now, or its current watermark if there are
// none — never lower than after, so the watermark only moves forward.
//
// It runs the same fan-out unreadFor runs (concerns, then entriesAfter from
// after), which is why get_inbox pays for that fan-out twice. Entries outside
// the concerning set are ignored even though they share a partition: an entry
// on a thread that does not concern alias can never be unread for it
// (unreadByThread filters on exactly this set), so counting it would raise the
// watermark over in-flight ids for no signal at all.
func (s *Store) readableThrough(ctx context.Context, alias, role string, after int64) (int64, error) {
	threads, parts, err := s.concerns(ctx, alias, role)
	if err != nil {
		return 0, err
	}
	entries, err := s.entriesAfter(ctx, parts, after)
	if err != nil {
		return 0, err
	}
	concerning := idSet(threads)
	watermark := after
	for _, e := range entries {
		if e.ID > watermark && concerning[e.ThreadID] {
			watermark = e.ID
		}
	}
	return watermark, nil
}

// SessionUnread is the session-level unread query: all aliases sharing the
// exact (socketPath, sessionID) tuple are ONE actor identity for unread math
// and actor exclusion. total counts DISTINCT threads concerning any alias of
// the session that have an entry newer than THAT alias's own watermark written
// by someone who is not any alias of the session — so a session's own writes
// under either alias never make its own threads unread, and a broadcast
// concerning two sibling aliases counts once, never twice. action is the
// subset whose effective intent is action-requested. An empty socketPath or
// sessionID never groups.
func (s *Store) SessionUnread(socketPath, sessionID string) (total, action int, err error) {
	if socketPath == "" || sessionID == "" {
		return 0, 0, nil
	}
	ctx := backgroundCtx()
	items, err := s.roster(ctx)
	if err != nil {
		return 0, 0, err
	}
	// The roster is a GSI query, which DynamoDB can never serve strongly
	// consistently — so it is used ONLY to identify which aliases belong to the
	// session. The watermark it projects is not trustworthy here: the daemon
	// runs Inbox -> MarkRead -> setSessionBadge -> SessionUnread synchronously
	// on get_inbox, so a stale watermark would return total > 0 and re-light
	// the tmux badge the operator just drained — and nothing re-polls, so it
	// would stay lit until the next notify or read.
	//
	// The SQLite "sess" CTE does not filter departed rows, so neither does
	// this: a tombstoned alias is still part of the session's identity.
	var candidates []string
	for _, item := range items {
		a := itemToAgent(item)
		if a.SocketPath != socketPath || a.SessionID != sessionID {
			continue
		}
		candidates = append(candidates, a.Alias)
	}
	sort.Strings(candidates)

	// Re-read each member from the BASE table, strongly consistent, and
	// re-confirm the tuple against the fresh item — the index copy may have
	// been written before the alias moved to a different session.
	var session []store.Agent
	aliases := make(map[string]bool)
	for _, alias := range candidates {
		a, found, err := s.agentByAlias(ctx, alias)
		if err != nil {
			return 0, 0, err
		}
		if !found || a.SocketPath != socketPath || a.SessionID != sessionID {
			continue
		}
		session = append(session, a)
		aliases[a.Alias] = true
	}
	if len(session) == 0 {
		return 0, 0, nil
	}

	unread := make(map[int64]map[string]types.AttributeValue)
	for _, a := range session {
		threads, parts, err := s.concerns(ctx, a.Alias, a.Role)
		if err != nil {
			return 0, 0, err
		}
		entries, err := s.entriesAfter(ctx, parts, a.LastReadEntryID)
		if err != nil {
			return 0, 0, err
		}
		for id := range unreadByThread(entries, idSet(threads), aliases, a.LastReadEntryID) {
			unread[id] = threads[id]
		}
	}
	for _, item := range unread {
		if effectiveIntent(strAttr(item, "kind"), strAttr(item, "intent")) == store.IntentAction {
			action++
		}
	}
	return len(unread), action, nil
}
