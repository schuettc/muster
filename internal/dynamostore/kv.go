package dynamostore

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/schuettc/muster/internal/clock"
	"github.com/schuettc/muster/internal/store"
)

func kvKey(key string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": attrS(pkKV(key)),
		"sk": attrN(metaSK),
	}
}

// KVSet upserts a shared fact — a whole-item PutItem, which is
// last-write-wins exactly like the SQLite ON CONFLICT DO UPDATE (every column
// of the row is replaced there too, so there is no partial-update semantics to
// preserve).
//
// A PutItem writes an item map rather than an update expression, so the
// reserved-word problem setExpr exists for does not arise here: "value" and
// "key" are both reserved words, and both are safe as literal attribute names
// in an item.
func (s *Store) KVSet(key, value, updatedBy string) error {
	_, err := s.c.PutItem(backgroundCtx(), &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"pk":         attrS(pkKV(key)),
			"sk":         attrN(metaSK),
			"key":        attrS(key),
			"value":      attrS(value),
			"updated_by": attrS(updatedBy),
			"updated_at": attrN(clock.NowMillis()),
		},
	})
	if err != nil {
		return fmt.Errorf("dynamostore: kv set %q: %w", key, err)
	}
	return nil
}

// KVGet returns the pair for key; ok is false if the key is absent.
//
// Strongly consistent, per the package's read-consistency rule. The blackboard
// is a coordination primitive: the caller that just wrote a fact and reads it
// back — or reads a fact whose write it triggered a peer to make and then
// waited on — must not be handed the superseded value, and an agent
// coordinating on stale shared state fails silently rather than loudly. It is
// one base-table single-item read on a synchronous op path, the same shape as
// agentByAlias and threadMeta.
func (s *Store) KVGet(key string) (store.KVPair, bool, error) {
	out, err := s.c.GetItem(backgroundCtx(), &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            kvKey(key),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return store.KVPair{}, false, fmt.Errorf("dynamostore: kv get %q: %w", key, err)
	}
	if len(out.Item) == 0 {
		return store.KVPair{}, false, nil
	}
	return store.KVPair{
		Key:       strAttr(out.Item, "key"),
		Value:     strAttr(out.Item, "value"),
		UpdatedBy: strAttr(out.Item, "updated_by"),
		UpdatedAt: numAttr(out.Item, "updated_at"),
	}, true, nil
}
