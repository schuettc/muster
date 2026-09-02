package daemon

import (
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/store"
)

// standing_set creates a listable, greeting order; standing_list returns it;
// standing_retract stops it greeting and drops it from the list. Unknown
// projects are rejected on set/retract; the key defaults to invariants.
func TestStandingOrderOpsLifecycle(t *testing.T) {
	n := &fakeNotifier{}
	sock, s := startWithNotifierAndStore(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1", "session_created": 100})

	// set with an omitted key defaults to invariants.
	if resp := call(t, sock, "standing_set", map[string]any{"from": "web1", "project": "web", "body": "rebase before push"}); !resp.OK {
		t.Fatalf("standing_set: %s", resp.Error)
	}
	orders, _ := s.ListStandingOrders("web")
	if len(orders) != 1 || orders[0].Key != "invariants" || orders[0].Body != "rebase before push" {
		t.Fatalf("after set, orders = %+v", orders)
	}

	// list op returns it.
	resp := call(t, sock, "standing_list", map[string]any{"project": "web"})
	if !resp.OK {
		t.Fatalf("standing_list: %s", resp.Error)
	}
	var listed struct {
		Orders []store.StandingOrder `json:"orders"`
	}
	decode(t, resp, &listed)
	if len(listed.Orders) != 1 || listed.Orders[0].Body != "rebase before push" {
		t.Fatalf("standing_list orders = %+v", listed.Orders)
	}

	// retract stops greeting and empties the list.
	if resp := call(t, sock, "standing_retract", map[string]any{"from": "web1", "project": "web"}); !resp.OK {
		t.Fatalf("standing_retract: %s", resp.Error)
	}
	if orders, _ := s.ListStandingOrders("web"); len(orders) != 0 {
		t.Fatalf("after retract, orders = %+v, want none", orders)
	}
}

func TestStandingSetRejectsUnknownProject(t *testing.T) {
	n := &fakeNotifier{}
	sock := startWithNotifier(t, n)
	call(t, sock, "register_agent", map[string]any{"alias": "web1", "project": "web", "socket_path": "/s", "session_id": "$1", "session_created": 100})

	resp := call(t, sock, "standing_set", map[string]any{"from": "web1", "project": "wbe", "body": "x"})
	if resp.OK {
		t.Fatal("standing_set to an unknown project must be rejected")
	}
	if !strings.Contains(resp.Error, `no registered agents in project "wbe"`) {
		t.Fatalf("error should name the project, got: %q", resp.Error)
	}
}
