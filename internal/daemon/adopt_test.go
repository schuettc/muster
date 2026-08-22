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
