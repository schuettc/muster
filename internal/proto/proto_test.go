package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/proto"
)

func TestRequestOmitsEmptyIdemKey(t *testing.T) {
	b, err := json.Marshal(proto.Request{Op: "list_agents"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "idem") {
		t.Fatalf("empty IdemKey must not appear on the wire: %s", b)
	}
}

func TestRequestRoundTripsIdemKey(t *testing.T) {
	b, err := json.Marshal(proto.Request{Op: "send_message", IdemKey: "k-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got proto.Request
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.IdemKey != "k-1" {
		t.Fatalf("IdemKey = %q, want k-1", got.IdemKey)
	}
}
