package dynamostore

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/schuettc/muster/internal/clock"
)

const (
	// Idempotency record states. These deliberately duplicate the SQLite
	// backend's own constants rather than sharing them: a record never crosses
	// backends — it is written and read by one deployment's store — so the two
	// vocabularies are independent storage details, not a shared contract.
	// What must agree is the store.API semantics, which the conformance suite
	// holds both to.
	idemPending = "pending"
	idemDone    = "done"

	// idemTTL is how long an idempotency record survives. It is a const rather
	// than an operator knob because it is a CORRECTNESS parameter, not a
	// storage/cost tradeoff: the window has to outlast the longest horizon over
	// which a client may redeliver a request, and shortening it below that
	// silently re-enables the double execution the record exists to prevent.
	// 24 hours is far beyond any client's retry budget.
	idemTTL = 24 * time.Hour
)

func idemKey(key string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": attrS(pkIdem(key)),
		"sk": attrN(metaSK),
	}
}

// IdemBegin claims key for a first delivery.
//
// found=false means this caller owns execution and MUST call IdemComplete when
// the op finishes. found=true with done=true means the op already ran and resp
// is its recorded response, which the caller replays verbatim. found=true with
// done=false means an identical request is in flight — the caller neither
// executes nor replays; it tells the client to retry.
//
// The claim is a conditional PutItem. attribute_not_exists(pk) is evaluated by
// DynamoDB serially per item, so N concurrent callers on one key yield exactly
// one success and N-1 ConditionalCheckFailedException — the same guarantee the
// SQLite backend gets from INSERT ... ON CONFLICT DO NOTHING, and for the same
// reason: the claim IS the write, never a read followed by one.
//
// The GetItem on the condition-failed path is STRONGLY CONSISTENT, and that is
// the package's read-consistency rule applied literally: this read exists only
// because the caller's own PutItem just lost a race it caused, and it decides
// whether the caller replays a response or backs off. An eventually consistent
// read could return no item at all for a record DynamoDB has just told us
// exists, and there is no answer to give from that state that is not wrong.
//
// The `claim` attribute closes a hazard the SQLite backend does not have. A
// conditional PutItem is NOT idempotent under retry and, unlike
// TransactWriteItems, it takes no ClientRequestToken — so when the write
// COMMITS and the acknowledgement is lost, the SDK's own retry re-evaluates
// attribute_not_exists(pk) against the record it just created and comes back
// ConditionalCheckFailed. Reading that at face value tells the caller who
// actually won the claim that an identical request is in flight; the client
// then retries, sees the same pending record, and the op never runs at all
// until the 24-hour TTL clears it. Stamping a per-CALL token on the record and
// recognising it on the failed path turns that into what it is — our own
// commit — and hands the claim back. It must be per call, not per Store: a
// genuine redelivery to the same process has to see found=true.
//
// A record that has genuinely vanished between the two calls — the TTL reaped
// it in the gap — reports in-flight rather than claimable. Reporting it
// claimable would let this caller execute an op that may still be running.
// A retry claims it cleanly, and the SQLite backend answers the same way.
func (s *Store) IdemBegin(key string) (resp []byte, done bool, found bool, err error) {
	ctx := backgroundCtx()
	now := clock.NowMillis()
	claim := rand.Text()
	_, err = s.c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"pk":         attrS(pkIdem(key)),
			"sk":         attrN(metaSK),
			"key":        attrS(key),
			"state":      attrS(idemPending),
			"claim":      attrS(claim),
			"created_at": attrN(now),
			"ttl":        attrN(expireAt(now, idemTTL)),
		},
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	})
	if err == nil {
		return nil, false, false, nil
	}
	if !isConditionFailed(err) {
		return nil, false, false, fmt.Errorf("dynamostore: idem begin %q: %w", key, err)
	}

	out, err := s.c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            idemKey(key),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, false, false, fmt.Errorf("dynamostore: idem read %q: %w", key, err)
	}
	resp, done, found = idemOutcome(out.Item, claim)
	return resp, done, found, nil
}

// idemOutcome decides what a condition-failed claim means, given the record
// that is actually there and the token this call stamped on its own write. It
// is a pure function so all four branches are covered without an endpoint —
// two of them (the lost acknowledgement, the TTL gap) cannot be provoked
// against a live DynamoDB from a test.
func idemOutcome(item map[string]types.AttributeValue, claim string) (resp []byte, done, found bool) {
	switch {
	case len(item) == 0:
		// Reaped between the PutItem and this read. In-flight, not claimable.
		return nil, false, true
	case strAttr(item, "claim") == claim:
		// Our own commit, acknowledged only to the SDK's retry. We hold it.
		return nil, false, false
	case strAttr(item, "state") != idemDone:
		return nil, false, true
	default:
		// Done-ness is carried by `state` alone, never by the presence of
		// `resp`: an op that recorded an empty response must still read as
		// done, not degrade to in-flight.
		return binAttr(item, "resp"), true, true
	}
}

// IdemComplete records resp as key's response and marks the record done, so a
// redelivery replays it. An unknown key is a no-op.
//
// The condition is what makes that no-op true. DynamoDB's UpdateItem is an
// upsert, so without attribute_exists(pk) this would CREATE a done record for a
// key nobody ever claimed — and the next caller of IdemBegin on that key would
// be told its op had already run and handed a response for work that never
// happened. The SQLite UPDATE simply matches no rows; this is the same
// contract, spelled out.
//
// An EMPTY response is stored by REMOVING the attribute, not by writing a
// zero-length one: DynamoDB rejects an empty AttributeValue in an update
// expression outright ("Supplied AttributeValue is empty"), so a completed op
// that recorded no body would fail the whole call. Absence is a faithful
// encoding of it — SQLite stores NULL there for exactly the same case, and
// done-ness is read from `state`, never from whether `resp` is present.
//
// The ttl is deliberately not refreshed: the retention window runs from first
// delivery, which is when the redelivery horizon starts.
func (s *Store) IdemComplete(key string, resp []byte) error {
	attrs := map[string]types.AttributeValue{"state": attrS(idemDone)}
	if len(resp) > 0 {
		attrs["resp"] = attrB(resp)
	}
	expr, names, values := setExpr(attrs)
	if len(resp) == 0 {
		expr += " REMOVE #resp"
		names["#resp"] = "resp"
	}
	_, err := s.c.UpdateItem(backgroundCtx(), &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.table),
		Key:                       idemKey(key),
		UpdateExpression:          aws.String(expr),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	})
	if err != nil {
		if isConditionFailed(err) {
			return nil // unknown key: no-op, matching the SQLite contract
		}
		return fmt.Errorf("dynamostore: idem complete %q: %w", key, err)
	}
	return nil
}
