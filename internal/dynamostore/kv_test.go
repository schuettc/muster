package dynamostore

import "testing"

// TestKVKeysWithReservedNames guards the DynamoDB reserved-word trap: "value",
// "key" and "name" are all reserved, and a KV key is operator-chosen text that
// lands in a partition key rather than an expression.
func TestKVKeysWithReservedNames(t *testing.T) {
	s := newTestStore(t)
	for _, key := range []string{"value", "name", "status", "size", "a.b#c"} {
		if err := s.KVSet(key, key+"-v", "op"); err != nil {
			t.Fatalf("set %q: %v", key, err)
		}
		p, ok, err := s.KVGet(key)
		if err != nil || !ok || p.Value != key+"-v" {
			t.Fatalf("get %q: ok=%v value=%q err=%v", key, ok, p.Value, err)
		}
	}
}
