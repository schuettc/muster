package dynamostore

import "testing"

// TestKVSetGet mirrors the SQLite backend's TestKVSetGet exactly: absent key
// reads ok=false, and a second Set is last-write-wins rather than an error.
func TestKVSetGet(t *testing.T) {
	s := newTestStore(t)
	if _, ok, err := s.KVGet("api.base"); err != nil || ok {
		t.Fatalf("missing key should return ok=false, got ok=%v err=%v", ok, err)
	}
	if err := s.KVSet("api.base", "http://localhost:4000", "backend"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.KVSet("api.base", "http://localhost:4001", "backend"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	p, ok, err := s.KVGet("api.base")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if p.Key != "api.base" || p.Value != "http://localhost:4001" || p.UpdatedBy != "backend" {
		t.Fatalf("unexpected pair: %+v", p)
	}
	if p.UpdatedAt == 0 {
		t.Fatalf("UpdatedAt must be stamped, got %+v", p)
	}
}

// TestKVGetIsReadYourWrites is the reason KVGet is a strongly consistent read.
// The blackboard is a coordination primitive — an agent that writes a fact and
// reads it back must not be handed the superseded value. An eventually
// consistent GetItem makes this assertion a coin flip.
func TestKVGetIsReadYourWrites(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 20; i++ {
		if err := s.KVSet("k", "v", "writer"); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
		p, ok, err := s.KVGet("k")
		if err != nil || !ok || p.Value != "v" {
			t.Fatalf("read %d: ok=%v value=%q err=%v", i, ok, p.Value, err)
		}
	}
}

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
