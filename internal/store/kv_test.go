package store

import "testing"

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
	if p.Value != "http://localhost:4001" || p.UpdatedBy != "backend" {
		t.Fatalf("unexpected value: %+v", p)
	}
}
