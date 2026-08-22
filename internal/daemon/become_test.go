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
		"alias": "muster-2", "socket_path": "/s", "session_id": "$1", "session_created": 100, "harness_session_id": "uuid-1",
	})
	call(t, sock, "register_agent", map[string]any{"alias": "peer", "socket_path": "/s", "session_id": "$2", "session_created": 100})
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

	// "peer" is LIVE, so become must refuse — and the message must not
	// advise purging, which cannot remove a live row (see the handler).
	resp = call(t, sock, "become", map[string]any{"from": "alias-routing", "to": "peer"})
	if resp.OK || !strings.Contains(resp.Error, "held by a live session") {
		t.Fatalf("existing-to guard: %+v", resp)
	}
	if strings.Contains(resp.Error, "purge") {
		t.Fatalf("refusal must not advise purging a live alias: %+v", resp)
	}
}

// TestBecomeReportsReclaimed pins the `reclaimed` bool the CLI needs to tell
// an operator apart two very different becomes: claiming a brand-new name
// (reclaimed:false) versus reclaiming a name that once belonged to someone
// else and was deregistered (reclaimed:true) — store.Become itself treats
// both the same way (a tombstoned `to` is silently reused, DELETE + clone
// INSERT), so the daemon must look BEFORE calling Become to tell them apart.
func TestBecomeReportsReclaimed(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "muster-2", "socket_path": "/s", "session_id": "$1", "session_created": 100})

	// Fresh name: never registered before.
	resp := call(t, sock, "become", map[string]any{"from": "muster-2", "to": "alias-routing"})
	if !resp.OK {
		t.Fatalf("become: %+v", resp)
	}
	data, _ := resp.Data.(map[string]any)
	if reclaimed, _ := data["reclaimed"].(bool); reclaimed {
		t.Fatalf("a never-before-seen name must report reclaimed:false, got %+v", data)
	}

	// Deregister the successor, register a fresh seed, then become the
	// now-departed name: this IS a reclaim.
	call(t, sock, "deregister_agent", map[string]any{"alias": "alias-routing"})
	call(t, sock, "register_agent", map[string]any{"alias": "muster-3", "socket_path": "/s", "session_id": "$2", "session_created": 100})
	resp = call(t, sock, "become", map[string]any{"from": "muster-3", "to": "alias-routing"})
	if !resp.OK {
		t.Fatalf("become (reclaim): %+v", resp)
	}
	data, _ = resp.Data.(map[string]any)
	if reclaimed, _ := data["reclaimed"].(bool); !reclaimed {
		t.Fatalf("becoming a departed name must report reclaimed:true, got %+v", data)
	}
}

// TestBecomeUnreadResolvesIncarnationOnTheClaimersDevice is the cross-device
// tuple collision. (socket_path, session_id) is unique per MACHINE, not per
// bus: two laptops' tmux servers both use /private/tmp/tmux-501/default and
// both hand out $1. On the hosted backend one daemon fronts a store holding
// every device's rows, so a peer laptop whose tmux server started later
// carries a HIGHER session_created on the identical tuple.
//
// If the incarnation feeding become's unread count is resolved on the tuple
// alone, the peer's creation time wins the "highest non-zero created" rule,
// no row of the claiming device carries it, SessionUnread's device-scoped
// base case comes back empty, and become reports 0 unread to a session that
// has mail. Which laptop's tmux started first decides whether it misreports —
// that is the whole bug. The resolution must be scoped to the CLAIMER's
// device.
//
// The lambda is built with daemon.New(s, nil, ""), so d.up == nil on the server
// and sessionIncarnation's remote-mode guard does NOT fire there: the local
// path with a shared store is exactly the hosted configuration. This test
// reproduces it over the wire against SQLite by registering two devices onto
// one tuple.
func TestBecomeUnreadResolvesIncarnationOnTheClaimersDevice(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	const tmuxSock, tmuxSess = "/private/tmp/tmux-501/default", "$1"
	const mine, theirs = 1700000000, 1900000000 // their tmux server started later

	call(t, sock, "register_agent", map[string]any{
		"alias": "here", "device_id": "laptop-a",
		"socket_path": tmuxSock, "session_id": tmuxSess, "session_created": mine,
	})
	// The colliding row: a DIFFERENT machine, identical tuple, higher created.
	// DepartStaleSiblings is device-scoped, so this does not tombstone "here".
	call(t, sock, "register_agent", map[string]any{
		"alias": "elsewhere", "device_id": "laptop-b",
		"socket_path": tmuxSock, "session_id": tmuxSess, "session_created": theirs,
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender", "device_id": "laptop-a",
		"socket_path": tmuxSock, "session_id": "$9", "session_created": mine,
	})
	call(t, sock, "send_message", map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "here", "subject": "s", "body": "b",
	})

	resp := call(t, sock, "become", map[string]any{"from": "here", "to": "here-renamed"})
	if !resp.OK {
		t.Fatalf("become: %+v", resp)
	}
	data, _ := resp.Data.(map[string]any)
	if n, _ := data["unread"].(float64); n < 1 {
		t.Fatalf("unread = %v, want >= 1: a peer device's row on the same "+
			"(socket, session) tuple must not decide this device's incarnation",
			data["unread"])
	}
}

// TestSessionIncarnationOfIsDeviceScoped pins the resolver directly: the same
// colliding tuple, asked from each side, must answer with the asking device's
// own incarnation — never the highest on the bus.
func TestSessionIncarnationOfIsDeviceScoped(t *testing.T) {
	const sock, sess = "/private/tmp/tmux-501/default", "$1"
	roster := []store.Agent{
		{Alias: "here", DeviceID: "laptop-a", SocketPath: sock, SessionID: sess, SessionCreated: 1700000000},
		{Alias: "elsewhere", DeviceID: "laptop-b", SocketPath: sock, SessionID: sess, SessionCreated: 1900000000},
		{Alias: "ghost", DeviceID: "laptop-a", SocketPath: sock, SessionID: sess, SessionCreated: 1600000000, Departed: true},
	}
	for _, tc := range []struct {
		device   string
		fallback int64
		want     int64
	}{
		{"laptop-a", 1700000000, 1700000000}, // its own, not laptop-b's higher one
		{"laptop-b", 1900000000, 1900000000},
		{"laptop-c", 42, 42}, // no row on this device proves anything: fallback stands
	} {
		if got := sessionIncarnationOf(roster, tc.device, sock, sess, tc.fallback); got != tc.want {
			t.Errorf("sessionIncarnationOf(%q) = %d, want %d", tc.device, got, tc.want)
		}
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
	d, err := Serve(paths.SocketPath(), wrapped, &fakeNotifier{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	sock := paths.SocketPath()

	call(t, sock, "register_agent", map[string]any{
		"alias": "muster-2", "socket_path": "/s", "session_id": "$1", "session_created": 100,
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
