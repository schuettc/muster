package station

import (
	"encoding/json"
	"testing"
)

// TestFetchLastActiveSkipsPeekEvents covers Finding 5: a "peek" event's Agent
// field is the PEEKED alias, not the caller who actually read it — an
// unowned get_inbox read that moves no watermark is not that alias's own
// activity. Before the fix, fetchLastActiveCmd treated any event whose Agent
// matched alias as that agent acting, so a sweep of an idle agent's inbox by
// someone else would show it as just-active. The newest REAL actor event
// underneath the peek must still be found.
func TestFetchLastActiveSkipsPeekEvents(t *testing.T) {
	caller := fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op != "list_events" {
			t.Fatalf("unexpected op %s", op)
		}
		return json.RawMessage(`{"events":[
			{"id":2,"ts":2000,"kind":"peek","agent":"alpha-1","detail":"someone-else"},
			{"id":1,"ts":1000,"kind":"reply","agent":"alpha-1"}
		],"max_id":2}`), nil
	}}

	cmd := fetchLastActiveCmd(caller, "alpha-1", 7)
	msg, ok := cmd().(lastActiveMsg)
	if !ok {
		t.Fatalf("expected lastActiveMsg, got %T", cmd())
	}
	if msg.err != nil {
		t.Fatalf("unexpected error: %v", msg.err)
	}
	if msg.ts != 1000 {
		t.Fatalf("ts = %d, want 1000 (the real actor event, skipping the peek)", msg.ts)
	}
}

// TestFetchLastActiveAllPeeksMeansUnknown: when every candidate row is a peek
// of this alias, there is no real actor event to report — ts stays 0 (no
// panic, no false-positive activity).
func TestFetchLastActiveAllPeeksMeansUnknown(t *testing.T) {
	caller := fakeCaller{fn: func(_ string, _ map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"events":[{"id":1,"ts":500,"kind":"peek","agent":"alpha-1"}],"max_id":1}`), nil
	}}

	cmd := fetchLastActiveCmd(caller, "alpha-1", 7)
	msg, ok := cmd().(lastActiveMsg)
	if !ok {
		t.Fatalf("expected lastActiveMsg, got %T", cmd())
	}
	if msg.ts != 0 {
		t.Fatalf("ts = %d, want 0 when only peek events are present", msg.ts)
	}
}
