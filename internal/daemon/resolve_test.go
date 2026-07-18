package daemon

import (
	"strings"
	"testing"
)

// TestSendMessageResolvesManualLabelInSenderProject is the direct fix for
// the black-hole incident (spec queue item 1): unlike the CLI, an MCP caller
// never pre-resolves to_target client-side — it passes a label straight
// through. The daemon itself must resolve it against the sender's own
// registered project, and the ALIAS it resolves to (not the raw label) is
// what lands in the stored thread, so a later Inbox/get_thread lookup by
// alias actually finds it.
func TestSendMessageResolvesManualLabelInSenderProject(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender-1", "role": "producer", "model_type": "claude", "project": "lake",
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "datalake-1", "role": "worker", "model_type": "claude", "project": "lake",
		"label": "datalake", "label_manual": true,
	})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "datalake", "subject": "s", "body": "b",
	})
	if !resp.OK {
		t.Fatalf("expected label 'datalake' to resolve within sender's project, got %+v", resp)
	}

	list := call(t, sock, "list_threads", map[string]any{"limit": 10})
	var res struct {
		Threads []struct {
			ToTarget string `json:"to_target"`
		} `json:"threads"`
	}
	decode(t, list, &res)
	if len(res.Threads) != 1 || res.Threads[0].ToTarget != "datalake-1" {
		t.Fatalf("expected stored to_target = resolved alias 'datalake-1', got %+v", res.Threads)
	}
}

// TestSendMessageLabelInAnotherProjectFailsWithHint: a label that exists only
// in a DIFFERENT project than the sender's must fail loudly with a
// proj:label hint — never guess across a project boundary.
func TestSendMessageLabelInAnotherProjectFailsWithHint(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender-1", "role": "producer", "model_type": "claude", "project": "muster",
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "datalake-1", "role": "worker", "model_type": "claude", "project": "lake",
		"label": "datalake", "label_manual": true,
	})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "datalake", "subject": "s", "body": "b",
	})
	if resp.OK {
		t.Fatalf("expected cross-project label to fail loudly, got %+v", resp)
	}
	if !strings.Contains(resp.Error, "lake:datalake") {
		t.Fatalf("expected error to hint the qualified target 'lake:datalake', got %q", resp.Error)
	}
}

// TestSendMessageUnknownTargetFailsLoudly is the direct regression test for
// the black-hole incident: an MCP agent addressing a target that resolves to
// NOTHING must fail the op outright — never silently create an
// undeliverable thread.
func TestSendMessageUnknownTargetFailsLoudly(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{"alias": "sender-1", "role": "producer", "model_type": "claude"})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "datalake", "subject": "s", "body": "b",
	})
	if resp.OK {
		t.Fatal("expected unknown target to fail the op")
	}

	list := call(t, sock, "list_threads", map[string]any{"limit": 10})
	var res struct {
		Threads []any `json:"threads"`
	}
	decode(t, list, &res)
	if len(res.Threads) != 0 {
		t.Fatalf("a failed resolution must not create a thread, got %+v", res.Threads)
	}
}

// TestSendMessageDepartedAliasStillAccepted: a departed (tombstoned) agent's
// ALIAS remains addressable — mail may be waiting for it to return.
func TestSendMessageDepartedAliasStillAccepted(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{"alias": "sender-1", "role": "producer", "model_type": "claude"})
	call(t, sock, "register_agent", map[string]any{"alias": "gone-1", "role": "worker", "model_type": "claude"})
	call(t, sock, "deregister_agent", map[string]any{"alias": "gone-1"})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "gone-1", "subject": "s", "body": "b",
	})
	if !resp.OK {
		t.Fatalf("expected a departed agent's ALIAS to still resolve, got %+v", resp)
	}
}

// TestSendMessageDepartedLabelNotAddressable: once an agent has departed,
// its LABEL must stop being addressable — a NEW message naming the label a
// departed agent used to answer to must fail loudly instead of silently
// landing on a thread nobody will ever read again.
func TestSendMessageDepartedLabelNotAddressable(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender-1", "role": "producer", "model_type": "claude", "project": "lake",
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "gone-1", "role": "worker", "model_type": "claude", "project": "lake",
		"label": "datalake", "label_manual": true,
	})
	call(t, sock, "deregister_agent", map[string]any{"alias": "gone-1"})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "datalake", "subject": "s", "body": "b",
	})
	if resp.OK {
		t.Fatal("expected a departed agent's label to no longer be addressable")
	}
}

// TestSendMessageAutoLabelNotAddressable: an auto (non-manual) topic label —
// derived from conversation content, not pinned by the operator — must
// never be addressable, at the daemon exactly as at the CLI (only a
// manually-pinned label is a stable address).
func TestSendMessageAutoLabelNotAddressable(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender-1", "role": "producer", "model_type": "claude", "project": "lake",
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "auto-1", "role": "worker", "model_type": "claude", "project": "lake",
		"label": "some topic", "label_manual": false,
	})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "some topic", "subject": "s", "body": "b",
	})
	if resp.OK {
		t.Fatal("expected an auto (non-manual) label to be unaddressable")
	}
}

// TestSendMessageAmbiguousLabelListsCandidates: two manually-pinned labels
// colliding in the sender's own project must fail with an ambiguity error
// naming both candidate aliases — never silently pick one.
func TestSendMessageAmbiguousLabelListsCandidates(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender-1", "role": "producer", "model_type": "claude", "project": "lake",
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "worker-a", "role": "worker", "model_type": "claude", "project": "lake",
		"label": "datalake", "label_manual": true,
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "worker-b", "role": "worker", "model_type": "claude", "project": "lake",
		"label": "datalake", "label_manual": true,
	})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "datalake", "subject": "s", "body": "b",
	})
	if resp.OK {
		t.Fatal("expected ambiguous label to fail")
	}
	if !strings.Contains(resp.Error, "worker-a") || !strings.Contains(resp.Error, "worker-b") {
		t.Fatalf("expected ambiguity error to list both candidates, got %q", resp.Error)
	}
}

// TestTaskCreateUnregisteredSenderAliasExactOnly: an unregistered sender has
// no registered project to scope a bare label against — task_create still
// resolves an EXACT alias for it, but bare-label resolution is skipped
// entirely rather than guessing "" as if it were a real project.
func TestTaskCreateUnregisteredSenderAliasExactOnly(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{
		"alias": "worker-1", "role": "worker", "model_type": "claude", "project": "",
		"label": "datalake", "label_manual": true,
	})

	okResp := call(t, sock, "task_create", map[string]any{
		"from": "nobody", "to_kind": "agent", "to_target": "worker-1", "subject": "s", "body": "b",
	})
	if !okResp.OK {
		t.Fatalf("expected exact alias to resolve for an unregistered sender, got %+v", okResp)
	}

	failResp := call(t, sock, "task_create", map[string]any{
		"from": "nobody", "to_kind": "agent", "to_target": "datalake", "subject": "s", "body": "b",
	})
	if failResp.OK {
		t.Fatal("expected bare-label resolution to fail for an unregistered sender")
	}
}

// TestSendMessageRoleAndBroadcastUnaffected: role and broadcast targets
// never go through agent-alias resolution — a role name or an empty
// broadcast target is not, and never was, a to_target the daemon resolves.
func TestSendMessageRoleAndBroadcastUnaffected(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{"alias": "sender-1", "role": "producer", "model_type": "claude"})
	call(t, sock, "register_agent", map[string]any{"alias": "rev", "role": "reviewer", "model_type": "claude"})

	if resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "role", "to_target": "reviewer", "subject": "s", "body": "b",
	}); !resp.OK {
		t.Fatalf("role-addressed send must be unaffected by agent resolution, got %+v", resp)
	}
	if resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "broadcast", "subject": "s", "body": "b",
	}); !resp.OK {
		t.Fatalf("broadcast send must be unaffected by agent resolution, got %+v", resp)
	}
}
