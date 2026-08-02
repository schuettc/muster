package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/mustertest"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/store"
)

// TestBecomeOpClaimsAndReports: end-to-end over the wire — identity moves,
// seed retires, journal records the claim, unread rides the response so
// surfaces can say "you are now X — N unread".
func TestBecomeOpClaimsAndReports(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/s", "session_id": "$1", "harness_session_id": "uuid-1",
	})
	call(t, sock, "register_agent", map[string]any{"alias": "peer", "socket_path": "/s", "session_id": "$2"})
	call(t, sock, "send_message", map[string]any{
		"from": "peer", "to_kind": "agent", "to_target": "muster-2", "subject": "s", "body": "b",
	})

	resp := call(t, sock, "become", map[string]any{"from": "muster-2", "to": "alias-routing"})
	if !resp.OK {
		t.Fatalf("become: %+v", resp)
	}
	data, _ := resp.Data.(map[string]any)
	if data["to"] != "alias-routing" || data["from"] != "muster-2" {
		t.Fatalf("response = %+v", data)
	}
	if n, _ := data["unread"].(float64); n < 1 {
		t.Fatalf("unread = %v, want >= 1 (pre-claim mail concerns the claimed identity's session)", data["unread"])
	}

	ev := call(t, sock, "list_events", map[string]any{"agent": "alias-routing"})
	if !containsEventDetail(t, ev, "become", "muster-2 → alias-routing") {
		t.Fatalf("no become journal event: %+v", ev.Data)
	}

	resp = call(t, sock, "become", map[string]any{"from": "alias-routing", "to": "peer"})
	if resp.OK || !strings.Contains(resp.Error, "already has history") {
		t.Fatalf("existing-to guard: %+v", resp)
	}
}

// getAgentFailStore wraps a real *store.Store and injects a GetAgent failure
// for one alias, leaving every other storeAPI method (including Become
// itself) untouched — the error-injecting-wrapper pattern storeAPI's doc
// comment describes.
type getAgentFailStore struct {
	*store.Store
	failAlias string
}

func (s *getAgentFailStore) GetAgent(alias string) (store.Agent, bool, error) {
	if alias == s.failAlias {
		return store.Agent{}, false, fmt.Errorf("injected GetAgent failure")
	}
	return s.Store.GetAgent(alias)
}

// TestBecomeOpDegradesOnPostCommitGetAgentFailure is finding F3: store.Become
// commits the claim first, then the become op reads the claimed row back
// (GetAgent) purely to reconcile the badge and compute unread. A failure in
// that read-back must not turn a successful claim into a caller-visible
// error — a caller that retried on error would hit ErrBecomeToExists for a
// claim that had already gone through. The op must degrade best-effort,
// exactly like the SessionUnread failure path already does, and report
// unread:0 rather than fail(...).
func TestBecomeOpDegradesOnPostCommitGetAgentFailure(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	wrapped := &getAgentFailStore{Store: s, failAlias: "alias-routing"}
	d, err := Serve(paths.SocketPath(), wrapped, &fakeNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	sock := paths.SocketPath()

	call(t, sock, "register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/s", "session_id": "$1",
	})

	resp := call(t, sock, "become", map[string]any{"from": "muster-2", "to": "alias-routing"})
	if !resp.OK {
		t.Fatalf("become must report success — the claim already committed despite the injected GetAgent failure: %+v", resp)
	}
	data, _ := resp.Data.(map[string]any)
	if data["to"] != "alias-routing" || data["from"] != "muster-2" {
		t.Fatalf("response = %+v", data)
	}
	if n, _ := data["unread"].(float64); n != 0 {
		t.Fatalf("unread must degrade to 0 on a post-commit GetAgent failure, got %v", data["unread"])
	}

	// The claim itself must have gone through in the store regardless of the
	// injected read-back failure.
	ag, found, err := s.GetAgent("alias-routing")
	if err != nil || !found || ag.Departed {
		t.Fatalf("claim should have committed: ag=%+v found=%v err=%v", ag, found, err)
	}
}
