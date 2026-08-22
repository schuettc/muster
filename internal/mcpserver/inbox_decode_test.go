package mcpserver

import "testing"

// TestDecodeInboxResponseLegacyBareArray: Finding 2. A 0.13.0+ MCP server
// forwarding to a still-running pre-0.13.0 daemon (or a device forwarding to
// a not-yet-redeployed hosted lambda) gets the OLD bare-array get_inbox
// shape, not {threads, marked_read}. The decode must fall back to that shape
// and report marked_read=true — exactly what the old daemon actually did on
// every read — rather than hard-failing every inbox call until the daemon
// restarts.
func TestDecodeInboxResponseLegacyBareArray(t *testing.T) {
	legacy := []byte(`[{"id":1,"kind":"message","from_agent":"a","subject":"hi"}]`)
	threads, markedRead, err := decodeInboxResponse(legacy)
	if err != nil {
		t.Fatalf("decodeInboxResponse(legacy) error: %v", err)
	}
	if !markedRead {
		t.Fatal("legacy bare-array payload must decode as marked_read=true")
	}
	if len(threads) != 1 || threads[0].ID != 1 || threads[0].Subject != "hi" {
		t.Fatalf("threads = %+v, want one thread id=1 subject=hi", threads)
	}
}

// TestDecodeInboxResponseNewShape: the current {threads, marked_read} object
// shape still decodes, carrying its own marked_read value through untouched.
func TestDecodeInboxResponseNewShape(t *testing.T) {
	current := []byte(`{"threads":[{"id":2,"kind":"task"}],"marked_read":false}`)
	threads, markedRead, err := decodeInboxResponse(current)
	if err != nil {
		t.Fatalf("decodeInboxResponse(current) error: %v", err)
	}
	if markedRead {
		t.Fatal("marked_read=false in the payload must not be coerced to true")
	}
	if len(threads) != 1 || threads[0].ID != 2 {
		t.Fatalf("threads = %+v, want one thread id=2", threads)
	}
}
