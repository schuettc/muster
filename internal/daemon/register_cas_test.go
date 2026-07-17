package daemon

import (
	"strings"
	"sync"
	"testing"

	"github.com/schuettc/muster/internal/store"
)

// TestRegisterAgentIfAbsentSucceedsWhenAbsent covers the common case: no
// existing record, if_absent=true registers exactly like a plain call.
func TestRegisterAgentIfAbsentSucceedsWhenAbsent(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	resp := call(t, sock, "register_agent", map[string]any{
		"alias": "station", "role": "operator", "model_type": "station",
		"socket_path": "/s", "session_id": "$1", "if_absent": true,
	})
	if !resp.OK {
		t.Fatalf("if_absent register on an absent alias should succeed: %+v", resp)
	}
}

// TestRegisterAgentIfAbsentIdempotentOnSameTuple covers the "same tuple"
// branch of station's probe loop: re-registering with the SAME
// (socket_path, session_id) tuple must succeed even with if_absent=true —
// this is an idempotent re-register, not a conflict.
func TestRegisterAgentIfAbsentIdempotentOnSameTuple(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{
		"alias": "station", "socket_path": "/s", "session_id": "$1",
	})
	resp := call(t, sock, "register_agent", map[string]any{
		"alias": "station", "role": "operator", "model_type": "station",
		"socket_path": "/s", "session_id": "$1", "if_absent": true,
	})
	if !resp.OK {
		t.Fatalf("if_absent register with the SAME tuple already present should succeed, got %+v", resp)
	}
}

// TestRegisterAgentIfAbsentConflictsOnDifferentTuple covers spec §5's CAS
// guard: a DIFFERENT tuple already registered under the alias must make an
// if_absent=true call fail rather than upsert (clobbering it).
func TestRegisterAgentIfAbsentConflictsOnDifferentTuple(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{
		"alias": "station", "socket_path": "/other", "session_id": "$OTHER",
	})
	resp := call(t, sock, "register_agent", map[string]any{
		"alias": "station", "socket_path": "/mine", "session_id": "$MINE", "if_absent": true,
	})
	if resp.OK {
		t.Fatalf("if_absent register over a different existing tuple should fail, got %+v", resp)
	}
	if !strings.Contains(resp.Error, "if_absent conflict") {
		t.Fatalf("error = %q, want it to mention an if_absent conflict (station's probe loop matches on this)", resp.Error)
	}

	// The original record must be untouched.
	getResp := call(t, sock, "get_agent", map[string]any{"alias": "station"})
	var out struct {
		Found bool        `json:"found"`
		Agent store.Agent `json:"agent"`
	}
	decode(t, getResp, &out)
	if !out.Found || out.Agent.SocketPath != "/other" || out.Agent.SessionID != "$OTHER" {
		t.Fatalf("a failed if_absent conflict must not touch the existing record, got %+v", out)
	}
}

// TestRegisterAgentPlainCallStillUpsertsOverDifferentTuple is the back-compat
// regression: omitting if_absent (today's behavior, every existing caller)
// must still silently overwrite a different tuple exactly as before.
func TestRegisterAgentPlainCallStillUpsertsOverDifferentTuple(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{
		"alias": "station", "socket_path": "/other", "session_id": "$OTHER",
	})
	resp := call(t, sock, "register_agent", map[string]any{
		"alias": "station", "socket_path": "/mine", "session_id": "$MINE",
	})
	if !resp.OK {
		t.Fatalf("a plain register_agent call (if_absent absent) must still upsert, got %+v", resp)
	}
	getResp := call(t, sock, "get_agent", map[string]any{"alias": "station"})
	var out struct {
		Found bool        `json:"found"`
		Agent store.Agent `json:"agent"`
	}
	decode(t, getResp, &out)
	if !out.Found || out.Agent.SocketPath != "/mine" || out.Agent.SessionID != "$MINE" {
		t.Fatalf("plain register_agent must overwrite the old tuple, got %+v", out)
	}
}

// TestRegisterAgentIfAbsentConcurrentRaceExactlyOneWins covers the actual
// race the CAS eliminates: several goroutines simultaneously if_absent-
// registering the SAME alias with DIFFERENT tuples — the alias's lock must
// serialize them so exactly one succeeds and every other loses with an
// if_absent conflict, never a silently-overwritten winner.
func TestRegisterAgentIfAbsentConcurrentRaceExactlyOneWins(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	const n = 8
	var wg sync.WaitGroup
	oks := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := call(t, sock, "register_agent", map[string]any{
				"alias": "station", "role": "operator", "model_type": "station",
				"socket_path": "/s", "session_id": string(rune('A' + i)), "if_absent": true,
			})
			oks[i] = resp.OK
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, ok := range oks {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one concurrent if_absent register should win, got %d of %d", wins, n)
	}
}

// TestGetThreadTotalField covers the newest-entries-gap fix (spec §5
// carried-over item): get_thread's response now carries a live "total" —
// len(entries) BEFORE pagination — alongside the paginated "entries", and a
// no-args call (the existing back-compat contract) is untouched.
func TestGetThreadTotalField(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{"alias": "api", "model_type": "claude"})
	resp := call(t, sock, "send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "body": "e0"})
	tid := threadIDOf(t, resp)
	for _, body := range []string{"e1", "e2", "e3", "e4"} {
		call(t, sock, "reply", map[string]any{"thread_id": tid, "from": "api", "body": body})
	}

	pageResp := call(t, sock, "get_thread", map[string]any{"thread_id": tid, "offset": 1, "limit": 2})
	var pageOut struct {
		Entries []store.Entry `json:"entries"`
		Total   int           `json:"total"`
	}
	decode(t, pageResp, &pageOut)
	if pageOut.Total != 5 {
		t.Fatalf("total = %d, want 5 (the full entry count, not the paginated window)", pageOut.Total)
	}
	if len(pageOut.Entries) != 2 {
		t.Fatalf("paginated entries = %d, want 2 (total must not affect pagination)", len(pageOut.Entries))
	}

	allResp := call(t, sock, "get_thread", map[string]any{"thread_id": tid})
	all := decodeThreadEntries(t, allResp)
	if len(all) != 5 {
		t.Fatalf("no-args get_thread (back-compat) should still return all 5 entries, got %d", len(all))
	}
}
