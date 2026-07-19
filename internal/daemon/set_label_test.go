package daemon

import (
	"encoding/json"
	"testing"
)

// TestSetLabelMakesLabelResolvableImmediately pins the CLI/MCP consistency
// contract (see resolveAgentTarget's doc comment): after `muster label`
// pushes a set_label op, an MCP-path sender resolving the label against the
// STORE must land on the same alias a CLI sender resolving against live tmux
// would — immediately, not at the session's next re-register.
func TestSetLabelMakesLabelResolvableImmediately(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})
	call(t, sock, "register_agent", map[string]any{
		"alias": "workspace-2", "socket_path": "/s", "session_id": "$0",
		"project": "bettor-help-workspace",
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender", "socket_path": "/s", "session_id": "$7",
		"project": "bettor-help-workspace",
	})

	// Before the sync: the stored label is blank, so the label must NOT
	// resolve for a daemon-path sender.
	pre := call(t, sock, "send_message", map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "datalake", "body": "hi",
	})
	if pre.OK {
		t.Fatalf("label must not resolve before set_label lands it in the store: %+v", pre)
	}

	resp := call(t, sock, "set_label", map[string]any{
		"socket_path": "/s", "session_id": "$0",
		"label": "datalake", "label_manual": true,
	})
	if !resp.OK {
		t.Fatalf("set_label: %+v", resp)
	}
	data, _ := json.Marshal(resp.Data)
	var out struct {
		Updated int64 `json:"updated"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Updated != 1 {
		t.Fatalf("set_label updated = %s (err %v), want 1", data, err)
	}

	post := call(t, sock, "send_message", map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "datalake", "body": "hi",
	})
	if !post.OK {
		t.Fatalf("label must resolve immediately after set_label: %+v", post)
	}
	th := getThreadForTest(t, sock, threadIDOf(t, post))
	if th.ToTarget != "workspace-2" {
		t.Fatalf("resolved target = %q, want workspace-2", th.ToTarget)
	}

	// Clearing withdraws addressability again.
	call(t, sock, "set_label", map[string]any{
		"socket_path": "/s", "session_id": "$0", "label": "", "label_manual": false,
	})
	cleared := call(t, sock, "send_message", map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "datalake", "body": "hi",
	})
	if cleared.OK {
		t.Fatalf("a cleared label must stop resolving: %+v", cleared)
	}
}

// getThreadForTest fetches one thread's header via the get_thread op.
func getThreadForTest(t *testing.T, sock string, id int64) (th struct {
	ToTarget string `json:"to_target"`
}) {
	t.Helper()
	resp := call(t, sock, "get_thread", map[string]any{"thread_id": id})
	data, _ := json.Marshal(resp.Data)
	var out struct {
		Thread struct {
			ToTarget string `json:"to_target"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("get_thread %d: %v (%s)", id, err, data)
	}
	th.ToTarget = out.Thread.ToTarget
	return th
}
