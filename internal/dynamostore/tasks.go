package dynamostore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/schuettc/muster/internal/clock"
	"github.com/schuettc/muster/internal/store"
)

// ClaimTask atomically moves a task from open → claimed and records it.
//
// The SQLite backend's `UPDATE … WHERE id=? AND status='open'` plus its
// RowsAffected check IS this method's specification, and on this backend the
// equivalent is a ConditionExpression — never a read-then-write. There is no
// single-writer backstop here (store.Open's SetMaxOpenConns(1) has no hosted
// counterpart; the Lambda scales), so the condition is the entire mechanism
// keeping two agents from picking up the same work. A read-then-write would
// reintroduce exactly the race it exists to prevent.
//
// An UNKNOWN thread returns ErrNotClaimable, not ErrThreadNotFound: the SQLite
// UPDATE simply matches no rows for an id that does not exist, and the daemon's
// task_claim op is built on that. This is the one place applyTaskWrite's
// `missing` field earns its keep.
func (s *Store) ClaimTask(threadID int64, byAgent string) error {
	return s.applyTaskWrite(taskWrite{
		threadID:   threadID,
		byAgent:    byAgent,
		newStatus:  "claimed",
		onlyIfOpen: true,
		missing:    store.ErrNotClaimable,
	})
}

// TransitionTask sets a new (validated) status and records the change as an
// entry.
//
// newStatus is validated against store.TaskStates BEFORE any DynamoDB call,
// matching SQLite: an invalid status is a caller error, not a write that fails
// somewhere in the service. Unlike ClaimTask there is no status precondition —
// the SQLite UPDATE carries none — so any state may move to any other,
// including back to open, which makes the task claimable again.
func (s *Store) TransitionTask(threadID int64, byAgent, newStatus, note string) error {
	if !store.TaskStates[newStatus] {
		return fmt.Errorf("invalid task status %q", newStatus)
	}
	return s.applyTaskWrite(taskWrite{
		threadID:  threadID,
		byAgent:   byAgent,
		body:      note,
		newStatus: newStatus,
		missing:   store.ErrThreadNotFound,
	})
}

// taskWrite is one status change: the thread's new status plus the entry that
// records it, which the SQLite backend writes inside a single transaction and
// this backend writes as a single TransactWriteItems.
//
// onlyIfOpen and missing are where the two callers differ, and both differences
// come straight from the SQL:
//
//	onlyIfOpen — ClaimTask's `AND status='open'` predicate. TransitionTask's
//	             UPDATE has none.
//	missing    — what a thread that is not there returns. Both SQL statements
//	             just match no rows; the two methods map that to different
//	             sentinels.
type taskWrite struct {
	threadID   int64
	byAgent    string
	body       string
	newStatus  string
	onlyIfOpen bool
	missing    error
}

// applyTaskWrite performs the status change and its entry in one transaction.
//
// The metadata read that opens it is NOT the claim decision — the decision is
// the ConditionExpression below and nothing else. The read is there because an
// entry must carry its thread's recipient (the gsi1 denormalization every
// unread query depends on), the same reason AppendEntry reads first. Whatever
// it observes about `status` is deliberately unused.
//
// Like AppendEntry, the entry id is allocated ONCE, outside the retry loop, so
// a retry never burns a second id or double-counts entry_count. A losing
// claimant does burn the id it allocated; that leaves a gap in the entry
// sequence, which is harmless because every unread bound is a strict
// `id > watermark` over WRITTEN index items, never a dense scan.
func (s *Store) applyTaskWrite(w taskWrite) error {
	ctx := backgroundCtx()
	meta, err := s.threadMeta(ctx, w.threadID)
	if err != nil {
		if errors.Is(err, store.ErrThreadNotFound) {
			return w.missing
		}
		return err
	}
	now := clock.NowMillis()
	entryID, err := s.nextID(ctx, "entry")
	if err != nil {
		return err
	}
	entry := entryItem(w.threadID, entryID, w.byAgent, w.body, w.newStatus, now,
		rcpt(strAttr(meta, "to_kind"), strAttr(meta, "to_target")))

	// advanceLast starts true for the same reason it does in AppendEntry: the
	// write claims the denormalized last-entry trio under a forward-only guard,
	// which is the durable form of SQLite's MAX(entries.id). A concurrent
	// AppendEntry holding a higher id can commit first, and the claim must not
	// drag the trio backwards over it.
	advanceLast := true
	for attempt := 0; ; attempt++ {
		err := s.taskTransact(ctx, entry, w, now, entryID, advanceLast)
		switch {
		case err == nil:
			return nil

		case isTransactionConflict(err) && attempt < appendMaxRetries:
			// Another writer held the thread item mid-transaction. Transient,
			// and nothing was written, so the whole thing retries safely. The
			// SDK's standard retryer does NOT cover this.
			time.Sleep(appendBackoff(attempt))

		case isTransactionConditionFailed(err):
			// Three guards can fail and they mean opposite things: the thread
			// vanished, someone else won the claim (a legitimate outcome that
			// must NEVER be retried), or a higher-id entry already owns the
			// last-entry trio (retry without competing for it). Only a strongly
			// consistent re-read can tell them apart.
			stillEligible, rErr := s.classifyTaskFailure(ctx, w)
			if rErr != nil {
				return rErr
			}
			if !stillEligible || !advanceLast {
				// Nothing left to concede: the write's own preconditions hold
				// and it was not competing for the trio, so this is a genuine
				// failure rather than a race to re-run.
				return fmt.Errorf("dynamostore: %s thread %d: %w", w.newStatus, w.threadID, err)
			}
			advanceLast = false

		default:
			return fmt.Errorf("dynamostore: %s thread %d: %w", w.newStatus, w.threadID, err)
		}
	}
}

// classifyTaskFailure re-reads the thread to decide what a cancelled
// transaction meant. It returns (true, nil) when the write's own preconditions
// still hold — so the failure was the forward-only last-entry guard and the
// caller should retry without advancing the trio — and a terminal error
// otherwise.
//
// The ErrNotClaimable branch is the load-bearing one: a claim that lost is a
// legitimate outcome, and retrying it would let a second claimant succeed after
// the first already won.
func (s *Store) classifyTaskFailure(ctx context.Context, w taskWrite) (bool, error) {
	meta, err := s.threadMeta(ctx, w.threadID)
	if err != nil {
		if errors.Is(err, store.ErrThreadNotFound) {
			return false, w.missing
		}
		return false, err
	}
	if w.onlyIfOpen && strAttr(meta, "status") != "open" {
		return false, store.ErrNotClaimable
	}
	return true, nil
}

// taskTransact writes the status change and its entry in one TransactWriteItems
// — the DynamoDB expression of the SQLite transaction, so a task never changes
// status without the entry recording who changed it.
func (s *Store) taskTransact(ctx context.Context, entry map[string]types.AttributeValue,
	w taskWrite, now, entryID int64, advanceLast bool,
) error {
	_, err := s.c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{TableName: aws.String(s.table), Item: entry}},
			{Update: taskUpdate(s.table, w, now, entryID, advanceLast)},
		},
	})
	return err
}

// taskUpdate builds the metadata-item update. Split out and kept free of the
// client so the guards can be asserted without an endpoint — none of them fails
// loudly if it silently disappears.
//
// "status" is a DynamoDB reserved word, so it is aliased like every other
// attribute this package writes (see setExpr's note).
func taskUpdate(table string, w taskWrite, now, entryID int64, advanceLast bool) *types.Update {
	names := map[string]string{
		"#status":      "status",
		"#updated_at":  "updated_at",
		"#entry_count": "entry_count",
	}
	values := map[string]types.AttributeValue{
		":status": attrS(w.newStatus),
		":now":    attrN(now),
		":one":    attrN(1),
	}
	set := "SET #status = :status, #updated_at = :now"
	cond := "attribute_exists(pk)"

	if w.onlyIfOpen {
		// ClaimTask's compare-and-swap, and the ONLY thing serializing
		// concurrent claimants. Do not weaken it into a prior read.
		cond += " AND #status = :open"
		values[":open"] = attrS("open")
	}
	if advanceLast {
		names["#last_entry_id"] = "last_entry_id"
		names["#last_from"] = "last_from"
		names["#last_at"] = "last_at"
		values[":eid"] = attrN(entryID)
		values[":from"] = attrS(w.byAgent)
		set += ", #last_entry_id = :eid, #last_from = :from, #last_at = :now"
		cond += " AND (attribute_not_exists(#last_entry_id) OR #last_entry_id < :eid)"
	}

	return &types.Update{
		TableName:                 aws.String(table),
		Key:                       threadKey(w.threadID),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
		ConditionExpression:       aws.String(cond),
		// ADD, not SET: the count is a counter, so it stays right even when
		// this update races another entry's.
		UpdateExpression: aws.String(set + " ADD #entry_count :one"),
	}
}
