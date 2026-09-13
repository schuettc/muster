package dynamostore

import (
	"fmt"
	"sort"
	"strings"

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

// KVList returns every blackboard pair whose key has the literal prefix,
// sorted lexicographically. The blackboard is intentionally small, so this is
// one complete base-table scan with filtering in memory.
func (s *Store) KVList(prefix string) ([]store.KVPair, error) {
	paginator := dynamodb.NewScanPaginator(s.c, &dynamodb.ScanInput{TableName: aws.String(s.table)})
	pairs := []store.KVPair{}
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(backgroundCtx())
		if err != nil {
			return nil, fmt.Errorf("dynamostore: kv list: %w", err)
		}
		for _, item := range page.Items {
			if !strings.HasPrefix(strAttr(item, "pk"), "KV#") || numAttr(item, "sk") != metaSK {
				continue
			}
			key := strAttr(item, "key")
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			pairs = append(pairs, store.KVPair{
				Key: key, Value: strAttr(item, "value"), UpdatedBy: strAttr(item, "updated_by"), UpdatedAt: numAttr(item, "updated_at"),
			})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Key < pairs[j].Key })
	return pairs, nil
}

// KVDelete removes key and reports whether a pair existed.
func (s *Store) KVDelete(key string) (bool, error) {
	out, err := s.c.DeleteItem(backgroundCtx(), &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table), Key: kvKey(key), ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return false, fmt.Errorf("dynamostore: kv delete %q: %w", key, err)
	}
	return len(out.Attributes) > 0, nil
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
