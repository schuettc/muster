package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/store"
)

func TestStandingCLIRoundTrip(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "web1", "project": "web", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}

	// set (default key)
	var buf bytes.Buffer
	if err := cmdStanding([]string{"set", "web", "rebase", "before", "push", "--from", "web1"}, &buf); err != nil {
		t.Fatalf("standing set: %v", err)
	}
	if !strings.Contains(buf.String(), "standing order set") {
		t.Fatalf("set output = %q", buf.String())
	}

	// list --json
	buf.Reset()
	if err := cmdStanding([]string{"web", "--json"}, &buf); err != nil {
		t.Fatalf("standing list: %v", err)
	}
	var orders []store.StandingOrder
	if err := json.Unmarshal(buf.Bytes(), &orders); err != nil {
		t.Fatalf("list --json not valid JSON: %v (%s)", err, buf.String())
	}
	if len(orders) != 1 || orders[0].Key != "invariants" || orders[0].Body != "rebase before push" {
		t.Fatalf("list = %+v", orders)
	}

	// retract
	buf.Reset()
	if err := cmdStanding([]string{"retract", "web", "--from", "web1"}, &buf); err != nil {
		t.Fatalf("standing retract: %v", err)
	}
	if !strings.Contains(buf.String(), "retracted") {
		t.Fatalf("retract output = %q", buf.String())
	}

	// list is now empty
	buf.Reset()
	if err := cmdStanding([]string{"web"}, &buf); err != nil {
		t.Fatalf("standing list: %v", err)
	}
	if !strings.Contains(buf.String(), "no standing orders") {
		t.Fatalf("empty list output = %q", buf.String())
	}
}

func TestStandingSetTooFewArgs(t *testing.T) {
	startTestDaemon(t)
	var buf bytes.Buffer
	if err := cmdStanding([]string{"set", "web"}, &buf); err == nil {
		t.Fatal("standing set without a body must error")
	}
}
