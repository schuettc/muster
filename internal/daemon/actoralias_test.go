package daemon

import (
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

// dispatchTask creates an open task from → to and returns its thread ID.
// t.Fatal on failure so a broken fixture never masquerades as an actor-alias
// bug in the test that calls it.
func dispatchTask(t *testing.T, d *Daemon, from, to string) int64 {
	t.Helper()
	resp := d.Dispatch(proto.Request{Op: "task_create", Args: map[string]any{
		"from": from, "to_kind": "agent", "to_target": to, "subject": "s", "body": "b",
	}})
	if !resp.OK {
		t.Fatalf("task_create: %+v", resp)
	}
	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("task_create data = %T, want map", resp.Data)
	}
	id, ok := data["thread_id"].(int64)
	if !ok {
		t.Fatalf("thread_id = %T %v, want int64", data["thread_id"], data["thread_id"])
	}
	return id
}

// statusChangeAuthor returns the from_agent recorded on the entry carrying
// status_change==want — the durable record of who claimed or transitioned a
// task, which is exactly what a short alias corrupts.
func statusChangeAuthor(t *testing.T, s *store.Store, threadID int64, want string) string {
	t.Helper()
	_, entries, err := s.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	for _, e := range entries {
		if e.StatusChange == want {
			return e.FromAgent
		}
	}
	t.Fatalf("no entry with status_change=%q in %+v", want, entries)
	return ""
}

// threadStatus reads a thread's current status.
func threadStatus(t *testing.T, s *store.Store, threadID int64) string {
	t.Helper()
	th, _, err := s.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	return th.Status
}

// TestTaskClaimExpandsAShortLocalAlias: `by` is an ACTOR alias, and the store
// writes it verbatim into entries.from_agent, so an unexpanded short name is
// durably recorded as the claimer of a task nobody on the roster claimed.
// MCP passes `by` straight through with no client-side resolution, so the
// daemon is the only place this can be caught (spec §3).
func TestTaskClaimExpandsAShortLocalAlias(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil, "personal")
	registerTestAgent(t, d, "personal-sender")
	registerTestAgent(t, d, "personal-worker")
	tid := dispatchTask(t, d, "personal-sender", "personal-worker")

	resp := d.Dispatch(proto.Request{Op: "task_claim", Args: map[string]any{"thread_id": float64(tid), "by": "worker"}})
	if !resp.OK {
		t.Fatalf("task_claim with a short local alias: %+v", resp)
	}
	if got := statusChangeAuthor(t, s, tid, "claimed"); got != "personal-worker" {
		t.Fatalf("recorded claimer = %q, want %q", got, "personal-worker")
	}
}

// TestTaskTransitionExpandsAShortLocalAlias is task_claim's twin: the same
// verbatim from_agent write happens on every status change.
func TestTaskTransitionExpandsAShortLocalAlias(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil, "personal")
	registerTestAgent(t, d, "personal-sender")
	registerTestAgent(t, d, "personal-worker")
	tid := dispatchTask(t, d, "personal-sender", "personal-worker")

	resp := d.Dispatch(proto.Request{Op: "task_transition", Args: map[string]any{
		"thread_id": float64(tid), "by": "worker", "status": "completed", "note": "done",
	}})
	if !resp.OK {
		t.Fatalf("task_transition with a short local alias: %+v", resp)
	}
	if got := statusChangeAuthor(t, s, tid, "completed"); got != "personal-worker" {
		t.Fatalf("recorded actor = %q, want %q", got, "personal-worker")
	}
}

// TestTaskClaimPrefersTheLocalSeededActor is the local-first discriminator:
// with BOTH a foreign bare "worker" and this device's "personal-worker" on
// the roster, a short `by` must record THIS device's agent. An isolated test
// (only one row present) cannot tell local-first from exact-first.
func TestTaskClaimPrefersTheLocalSeededActor(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil, "personal")
	registerTestAgent(t, d, "personal-sender")
	registerTestAgent(t, d, "worker")          // foreign bare alias, exact match on the literal input
	registerTestAgent(t, d, "personal-worker") // this device's own seeded alias
	tid := dispatchTask(t, d, "personal-sender", "personal-worker")

	resp := d.Dispatch(proto.Request{Op: "task_claim", Args: map[string]any{"thread_id": float64(tid), "by": "worker"}})
	if !resp.OK {
		t.Fatalf("task_claim: %+v", resp)
	}
	if got := statusChangeAuthor(t, s, tid, "claimed"); got != "personal-worker" {
		t.Fatalf("recorded claimer = %q, want %q (local-first must win over the foreign exact match)", got, "personal-worker")
	}
}

// TestTaskClaimUnknownActorFailsLoudly: after expansion, an actor matching no
// roster row must fail the op outright rather than durably recording a
// claimer nobody holds and notifying an alias nobody reads. The task must
// stay open — a rejected claim that still consumed the task would be worse
// than the phantom it replaces.
func TestTaskClaimUnknownActorFailsLoudly(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil, "personal")
	registerTestAgent(t, d, "personal-sender")
	registerTestAgent(t, d, "personal-worker")
	tid := dispatchTask(t, d, "personal-sender", "personal-worker")

	resp := d.Dispatch(proto.Request{Op: "task_claim", Args: map[string]any{"thread_id": float64(tid), "by": "ghost"}})
	if resp.OK {
		t.Fatal("expected an unregistered claimer to fail the op")
	}
	// The error names what was actually sent, not the prefixed string the
	// caller never wrote.
	if !strings.Contains(resp.Error, `"ghost"`) {
		t.Fatalf("error = %q, want it to name the alias actually sent (\"ghost\")", resp.Error)
	}
	if got := threadStatus(t, s, tid); got != "open" {
		t.Fatalf("thread status after a rejected claim = %q, want %q", got, "open")
	}
}

// TestTaskTransitionUnknownActorFailsLoudly is the transition twin, with the
// same "the op must not have taken effect" guarantee.
func TestTaskTransitionUnknownActorFailsLoudly(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil, "personal")
	registerTestAgent(t, d, "personal-sender")
	registerTestAgent(t, d, "personal-worker")
	tid := dispatchTask(t, d, "personal-sender", "personal-worker")

	resp := d.Dispatch(proto.Request{Op: "task_transition", Args: map[string]any{
		"thread_id": float64(tid), "by": "ghost", "status": "completed",
	}})
	if resp.OK {
		t.Fatal("expected an unregistered actor to fail the op")
	}
	if !strings.Contains(resp.Error, `"ghost"`) {
		t.Fatalf("error = %q, want it to name the alias actually sent (\"ghost\")", resp.Error)
	}
	if got := threadStatus(t, s, tid); got != "open" {
		t.Fatalf("thread status after a rejected transition = %q, want %q", got, "open")
	}
}

// TestTaskClaimDepartedActorStillAccepted: a tombstoned row is still a roster
// row, and an agent draining its old identity's mail may legitimately close
// out that identity's tasks.
func TestTaskClaimDepartedActorStillAccepted(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil, "personal")
	registerTestAgent(t, d, "personal-sender")
	registerTestAgent(t, d, "personal-worker")
	tid := dispatchTask(t, d, "personal-sender", "personal-worker")
	if resp := d.Dispatch(proto.Request{Op: "deregister_agent", Args: map[string]any{"alias": "personal-worker"}}); !resp.OK {
		t.Fatalf("deregister_agent: %+v", resp)
	}

	resp := d.Dispatch(proto.Request{Op: "task_claim", Args: map[string]any{"thread_id": float64(tid), "by": "worker"}})
	if !resp.OK {
		t.Fatalf("a departed agent's own task claim must still be accepted, got %+v", resp)
	}
	if got := statusChangeAuthor(t, s, tid, "claimed"); got != "personal-worker" {
		t.Fatalf("recorded claimer = %q, want %q", got, "personal-worker")
	}
}

// TestGetInboxExpandsAShortLocalAlias: an unexpanded short alias reads as an
// empty inbox forever — the model is told "no mail" while its mail sits under
// the seeded row.
func TestGetInboxExpandsAShortLocalAlias(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil, "personal")
	registerTestAgent(t, d, "personal-sender")
	registerTestAgent(t, d, "personal-worker")
	if resp := d.Dispatch(proto.Request{Op: "send_message", Args: map[string]any{
		"from": "personal-sender", "to_kind": "agent", "to_target": "personal-worker",
		"subject": "s", "body": "b",
	}}); !resp.OK {
		t.Fatalf("send_message: %+v", resp)
	}

	resp := d.Dispatch(proto.Request{Op: "get_inbox", Args: map[string]any{"alias": "worker"}})
	if !resp.OK {
		t.Fatalf("get_inbox with a short local alias: %+v", resp)
	}
	m, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("get_inbox data = %T, want map[string]any", resp.Data)
	}
	threads, ok := m["threads"].([]store.Thread)
	if !ok {
		t.Fatalf("get_inbox threads = %T, want []store.Thread", m["threads"])
	}
	if len(threads) != 1 {
		t.Fatalf("get_inbox returned %d thread(s), want 1 — a short alias must reach the seeded row's mail", len(threads))
	}
}

// TestGetInboxUnknownAliasFailsLoudly: reporting an empty inbox for an agent
// that does not exist is the same silent wrongness as the phantom claimer —
// a model reading "no mail" cannot tell it asked the wrong question.
func TestGetInboxUnknownAliasFailsLoudly(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil, "personal")
	registerTestAgent(t, d, "personal-worker")

	resp := d.Dispatch(proto.Request{Op: "get_inbox", Args: map[string]any{"alias": "ghost"}})
	if resp.OK {
		t.Fatal("expected an unknown inbox alias to fail the op")
	}
	if !strings.Contains(resp.Error, `"ghost"`) {
		t.Fatalf("error = %q, want it to name the alias actually sent (\"ghost\")", resp.Error)
	}
}

// TestGetInboxDepartedAliasStillDrains is the boundary the error decision must
// not cross: a departed row's mail still needs draining, and a tombstoned row
// is still a roster row, so it keeps working — in both its full and its short
// form.
func TestGetInboxDepartedAliasStillDrains(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil, "personal")
	registerTestAgent(t, d, "personal-sender")
	registerTestAgent(t, d, "personal-worker")
	if resp := d.Dispatch(proto.Request{Op: "send_message", Args: map[string]any{
		"from": "personal-sender", "to_kind": "agent", "to_target": "personal-worker",
		"subject": "s", "body": "b",
	}}); !resp.OK {
		t.Fatalf("send_message: %+v", resp)
	}
	if resp := d.Dispatch(proto.Request{Op: "deregister_agent", Args: map[string]any{"alias": "personal-worker"}}); !resp.OK {
		t.Fatalf("deregister_agent: %+v", resp)
	}

	for _, alias := range []string{"personal-worker", "worker"} {
		resp := d.Dispatch(proto.Request{Op: "get_inbox", Args: map[string]any{"alias": alias}})
		if !resp.OK {
			t.Fatalf("get_inbox %q on a departed row: %+v", alias, resp)
		}
		m, ok := resp.Data.(map[string]any)
		if !ok {
			t.Fatalf("get_inbox data = %T, want map[string]any", resp.Data)
		}
		threads, ok := m["threads"].([]store.Thread)
		if !ok {
			t.Fatalf("get_inbox threads = %T, want []store.Thread", m["threads"])
		}
		if len(threads) != 1 {
			t.Fatalf("get_inbox %q returned %d thread(s), want 1", alias, len(threads))
		}
	}
}

// TestActorAliasUnexpandedWithNoDeviceName: deviceName=="" is Lambda mode's
// deliberate choice (it serves many devices and must never guess one). The
// EXISTENCE check still applies there — a phantom claimer is wrong on the
// hosted bus too — but no seeding is attempted, so a bare alias that really
// is a roster row goes through untouched.
func TestActorAliasUnexpandedWithNoDeviceName(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil, "")
	registerTestAgent(t, d, "sender")
	registerTestAgent(t, d, "worker")
	tid := dispatchTask(t, d, "sender", "worker")

	resp := d.Dispatch(proto.Request{Op: "task_claim", Args: map[string]any{"thread_id": float64(tid), "by": "worker"}})
	if !resp.OK {
		t.Fatalf("task_claim: %+v", resp)
	}
	if got := statusChangeAuthor(t, s, tid, "claimed"); got != "worker" {
		t.Fatalf("recorded claimer = %q, want %q", got, "worker")
	}
}
