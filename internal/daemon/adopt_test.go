package daemon

import "testing"

// TestRegisterAdoptsBySameTranscript covers spec §3.2: a second register_agent
// call for the SAME transcript (e.g. /login rotates the harness session id,
// or a differently-named MCP registration races the hook-seeded row) must
// adopt the existing conversation row rather than birth a sibling.
func TestRegisterAdoptsBySameTranscript(t *testing.T) {
	sock, st := startWithNotifierAndStore(t, &fakeNotifier{})

	reg := func(alias, harness string) map[string]any {
		resp := call(t, sock, "register_agent", map[string]any{
			"alias": alias, "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1",
			"harness_session_id": harness, "transcript_path": "/t/c.jsonl",
		})
		if !resp.OK {
			t.Fatalf("register_agent(%q): %+v", alias, resp)
		}
		data, _ := resp.Data.(map[string]any)
		return data
	}
	reg("first", "uuid-A")
	resp := reg("second", "uuid-B") // same transcript, /login changed the harness id
	if resp["outcome"] != "adopted" || resp["alias"] != "first" {
		t.Fatalf("want adopted as first, got %v", resp)
	}
	if _, ok, _ := st.GetAgent("second"); ok {
		t.Fatal("no sibling row may be born")
	}
	a, _, _ := st.GetAgent("first")
	if a.HarnessSessionID != "uuid-B" {
		t.Fatalf("harness id must be re-stamped, got %q", a.HarnessSessionID)
	}
}

// TestRegisterAdoptsBySamePane covers the pane-tuple fallback of
// FindConversation: no transcript path yet (a hook-seeded row racing an MCP
// register before the harness has written its first transcript entry), but
// the full live pane tuple already identifies the same conversation.
func TestRegisterAdoptsBySamePane(t *testing.T) {
	sock, st := startWithNotifierAndStore(t, &fakeNotifier{})

	call(t, sock, "register_agent", map[string]any{
		"alias": "hook-seed", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1",
		"transcript_path": "/t/c.jsonl",
	})
	resp := call(t, sock, "register_agent", map[string]any{
		"alias": "mcp-name", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1",
		"harness_session_id": "uuid-old",
	})
	if !resp.OK {
		t.Fatalf("register_agent(mcp-name): %+v", resp)
	}
	data, _ := resp.Data.(map[string]any)
	if data["outcome"] != "adopted" || data["alias"] != "hook-seed" {
		t.Fatalf("MCP register on an occupied pane must adopt, got %v", data)
	}
	if _, ok, _ := st.GetAgent("mcp-name"); ok {
		t.Fatal("no sibling row may be born")
	}
}

// TestRegisterAdoptReapsGhostOnNewTuple covers the ordering the non-adopt
// path already documents: ghost reaping must run BEFORE badge
// reconciliation, or a sibling left on the tuple an adopt just moved onto —
// a leftover from a previous tmux server incarnation that recycled the
// session id — survives to be counted into the pushed badge. Here "ghost-old"
// occupies (socket, session, pane) under an old session_created; adopting
// "seed" (found by transcript_path) onto that same tuple under a NEW
// session_created must tombstone the ghost.
func TestRegisterAdoptReapsGhostOnNewTuple(t *testing.T) {
	sock, st := startWithNotifierAndStore(t, &fakeNotifier{})

	// The conversation to be adopted, on its own (unrelated) tuple.
	call(t, sock, "register_agent", map[string]any{
		"alias": "seed", "socket_path": "/other", "session_id": "$9", "session_created": 1, "pane_id": "%9",
		"transcript_path": "/t/c.jsonl",
	})
	// A ghost from a dead tmux server incarnation sitting on the tuple the
	// adopt below is about to move "seed" onto.
	call(t, sock, "register_agent", map[string]any{
		"alias": "ghost-old", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1",
	})

	resp := call(t, sock, "register_agent", map[string]any{
		"alias": "newcomer", "socket_path": "/s", "session_id": "$1", "session_created": 10, "pane_id": "%1",
		"transcript_path": "/t/c.jsonl",
	})
	data, _ := resp.Data.(map[string]any)
	if data["outcome"] != "adopted" || data["alias"] != "seed" {
		t.Fatalf("want adopted as seed, got %v", data)
	}
	if a, ok, _ := st.GetAgent("ghost-old"); !ok || !a.Departed {
		t.Fatalf("ghost on the adopted-onto tuple must be departed, got ok=%v %+v", ok, a)
	}
}

// TestRegisterSameAliasStillRefreshes guards against the adopt path
// misfiring on the ordinary revive/refresh case: FindConversation resolving
// to the SAME alias the caller already asked for is a normal upsert, not an
// adoption.
func TestRegisterSameAliasStillRefreshes(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})

	args := map[string]any{
		"alias": "a", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1",
		"transcript_path": "/t/c.jsonl",
	}
	call(t, sock, "register_agent", args)
	resp := call(t, sock, "register_agent", args)
	data, _ := resp.Data.(map[string]any)
	if data["outcome"] != "refreshed" {
		t.Fatalf("got %v", data)
	}
}

// TestRegisterAdoptModelFromCallerLabelFromRow covers the field-ownership
// split an adopt must observe: role/model_type come from the CALLER (the
// live process is the authority on what it's running, and can change model
// between sessions — the same rule reclaimRow already applies), falling back
// to the adopted row's when the caller sends none, while label/label_manual
// stay conversation-owned (an MCP caller's captured label may be stale) and
// must never be overwritten by an adopt.
func TestRegisterAdoptModelFromCallerLabelFromRow(t *testing.T) {
	sock, st := startWithNotifierAndStore(t, &fakeNotifier{})

	call(t, sock, "register_agent", map[string]any{
		"alias": "seed", "socket_path": "/other", "session_id": "$9", "session_created": 1, "pane_id": "%9",
		"transcript_path": "/t/c.jsonl", "model_type": "opus", "label": "conversation-label", "label_manual": true,
	})

	// Caller sends a non-empty model_type: it must win over the row's.
	call(t, sock, "register_agent", map[string]any{
		"alias": "newcomer", "socket_path": "/s", "session_id": "$1", "session_created": 5, "pane_id": "%1",
		"transcript_path": "/t/c.jsonl", "model_type": "sonnet", "label": "stale-mcp-label",
	})
	a, _, _ := st.GetAgent("seed")
	if a.ModelType != "sonnet" {
		t.Fatalf("caller's non-empty model_type must win, got %q", a.ModelType)
	}
	if a.Label != "conversation-label" || !a.LabelManual {
		t.Fatalf("label/label_manual must stay conversation-owned, got label=%q manual=%v", a.Label, a.LabelManual)
	}

	// Caller sends an empty model_type: the row's existing value survives.
	call(t, sock, "register_agent", map[string]any{
		"alias": "newcomer2", "socket_path": "/s2", "session_id": "$2", "session_created": 5, "pane_id": "%2",
		"transcript_path": "/t/c.jsonl",
	})
	a, _, _ = st.GetAgent("seed")
	if a.ModelType != "sonnet" {
		t.Fatalf("caller's empty model_type must leave the row's value intact, got %q", a.ModelType)
	}
}
