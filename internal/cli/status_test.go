package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/schuettc/muster/internal/store"
)

func TestStatusJSONRoundTrip(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "web/a", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{"from": "sender", "to_kind": "agent", "to_target": "web/a", "intent": "action-requested", "body": "go"}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := cmdStatus([]string{"--json"}, &buf); err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var rows []store.AliasStatus
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("status --json is not a top-level array: %v (%s)", err, buf.String())
	}
	var got store.AliasStatus
	for _, r := range rows {
		if r.Alias == "web/a" {
			got = r
		}
	}
	if got.Alias != "web/a" || got.Unread != 1 || got.ActionRequired != 1 {
		t.Fatalf("status for web/a = %+v, want unread 1 / action 1", got)
	}

	// --alias scopes to one row.
	buf.Reset()
	if err := cmdStatus([]string{"--json", "--alias", "web/a"}, &buf); err != nil {
		t.Fatalf("status --alias: %v", err)
	}
	var scoped []store.AliasStatus
	if err := json.Unmarshal(buf.Bytes(), &scoped); err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].Alias != "web/a" {
		t.Fatalf("--alias should return exactly web/a, got %+v", scoped)
	}
}
