package humancli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/mustertest"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/store"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// startTestDaemon boots a real in-process daemon on a temp socket, returning
// the underlying store so tests can seed rows (e.g. events at a controlled
// timestamp) directly, bypassing the wire protocol.
func startTestDaemon(t *testing.T) *store.Store {
	t.Helper()
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
	d, err := daemon.Serve(paths.SocketPath(), s, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return s
}

func TestAgentsCommandListsRegistered(t *testing.T) {
	startTestDaemon(t)
	// Register two agents directly via the daemon op (through Dispatch's helper).
	if _, err := callData("register_agent", map[string]any{"alias": "backend", "role": "producer", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Dispatch([]string{"agents"}, &buf); err != nil {
		t.Fatalf("agents: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "backend") || !strings.Contains(out, "consumer") || !strings.Contains(out, "claude") || !strings.Contains(out, "codex") {
		t.Fatalf("agents output missing rows:\n%s", out)
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	if err := Dispatch([]string{"bogus"}, nil); err == nil {
		t.Fatalf("expected error for unknown subcommand")
	}
}

func TestSendThenInboxShowsMessage(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	var sendBuf bytes.Buffer
	if err := Dispatch([]string{"send", "consumer", "the API changed", "--from", "backend", "--subject", "heads up"}, &sendBuf); err != nil {
		t.Fatalf("send: %v", err)
	}
	var inboxBuf bytes.Buffer
	if err := Dispatch([]string{"inbox", "consumer"}, &inboxBuf); err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if !strings.Contains(inboxBuf.String(), "heads up") {
		t.Fatalf("inbox missing sent message:\n%s", inboxBuf.String())
	}
}

// TestInboxTableShowsLastFromAndUnread proves `muster inbox` renders the
// LAST-FROM and UNREAD columns from get_inbox's new annotation fields — the
// CLI side of the fix for the production defect where an inbox listing gave
// no way to tell a peer's reply from the caller's own last send.
func TestInboxTableShowsLastFromAndUnread(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "web", "role": "producer", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("register_agent", map[string]any{"alias": "api", "role": "consumer", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	sendRaw, err := callData("send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "status?", "body": "how's it going"})
	if err != nil {
		t.Fatal(err)
	}
	var sendOut struct {
		ThreadID int64 `json:"thread_id"`
	}
	if err := json.Unmarshal(sendRaw, &sendOut); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("reply", map[string]any{"thread_id": sendOut.ThreadID, "from": "api", "body": "all good"}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Dispatch([]string{"inbox", "web"}, &buf); err != nil {
		t.Fatalf("inbox: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "LAST-FROM") || !strings.Contains(out, "UNREAD") {
		t.Fatalf("inbox table missing new columns:\n%s", out)
	}
	if !strings.Contains(out, "api") {
		t.Fatalf("inbox table missing last_from=api:\n%s", out)
	}
	// The row for the thread web originated must show unread=1 (api's
	// reply), the exact case get_inbox previously left indistinguishable
	// from web's own last send.
	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "status?") {
			found = true
			if !strings.Contains(line, "1") {
				t.Fatalf("inbox row for replied thread missing unread=1:\n%s", line)
			}
		}
	}
	if !found {
		t.Fatalf("inbox table missing the thread row:\n%s", out)
	}
}

// TestSendCommandAcceptsIntent proves --intent lands on the thread (visible
// via list_threads) and renders as a journal tag in `muster events`.
func TestSendCommandAcceptsIntent(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	var sendBuf bytes.Buffer
	if err := Dispatch([]string{"send", "consumer", "please take a look", "--from", "backend", "--subject", "spec review", "--intent", "reply-requested"}, &sendBuf); err != nil {
		t.Fatalf("send --intent: %v", err)
	}

	raw, err := callData("list_threads", map[string]any{"limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Threads []struct {
			Subject string `json:"subject"`
			Intent  string `json:"intent"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Threads) != 1 || res.Threads[0].Intent != "reply-requested" {
		t.Fatalf("expected thread with intent reply-requested, got %+v", res.Threads)
	}

	var eventsBuf bytes.Buffer
	if err := Dispatch([]string{"events", "--kind", "send"}, &eventsBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(eventsBuf.String(), "[reply?]") {
		t.Fatalf("events output missing intent tag:\n%s", eventsBuf.String())
	}
}

// TestSendCommandRejectsInvalidIntent proves an unrecognized --intent value
// is rejected client-side (a clearer error than a daemon round-trip), and
// that no thread is created.
func TestSendCommandRejectsInvalidIntent(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "consumer", "role": "consumer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Dispatch([]string{"send", "consumer", "body", "--from", "backend", "--intent", "urgent"}, &out)
	if err == nil {
		t.Fatalf("expected error for invalid --intent")
	}
	if !strings.Contains(err.Error(), "intent") {
		t.Fatalf("error should mention intent, got: %v", err)
	}
	raw, lerr := callData("list_threads", map[string]any{"limit": 10})
	if lerr != nil {
		t.Fatal(lerr)
	}
	var res struct {
		Threads []json.RawMessage `json:"threads"`
	}
	if uerr := json.Unmarshal(raw, &res); uerr != nil {
		t.Fatal(uerr)
	}
	if len(res.Threads) != 0 {
		t.Fatalf("invalid intent must not create a thread, got %d", len(res.Threads))
	}
}

// soleThreadIntent fetches the one thread list_threads currently holds and
// returns its intent — the two regression tests below both just want to know
// what landed in the DB, not the full row shape.
func soleThreadIntent(t *testing.T) string {
	t.Helper()
	raw, err := callData("list_threads", map[string]any{"limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Threads []struct {
			Intent string `json:"intent"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Threads) != 1 {
		t.Fatalf("expected exactly one thread, got %d", len(res.Threads))
	}
	return res.Threads[0].Intent
}

// TestSendIntentAfterPositionalBody is the literal regression case behind the
// live incident (thread 35, intent stored ”): --intent given AFTER an
// unquoted, multi-word positional body — exactly the shape a real shell
// produces when the body isn't quoted — must still land on the thread.
func TestSendIntentAfterPositionalBody(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "bettor", "role": "consumer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// Unquoted body: the shell would have split "1.2.2 shipped, FYI" into
	// four separate positional tokens, exactly as Dispatch receives them here.
	args := []string{"send", "bettor", "1.2.2", "shipped,", "FYI", "--from", "api", "--intent", "action-requested"}
	if err := Dispatch(args, &out); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := soleThreadIntent(t); got != "action-requested" {
		t.Fatalf("expected intent action-requested, got %q", got)
	}
}

// TestSendIntentNotSwallowedByDanglingValueFlag proves the actual mechanism
// behind the "silently drops the flag" symptom: Go's flag.Parse ALWAYS
// consumes the very next token as a non-boolean flag's value, regardless of
// what that token looks like — so a dangling value flag with no value of its
// own (--subject given no argument here) used to bind the FOLLOWING
// "--intent" token as its own bogus value, leaving "action-requested" as
// stray text flag.Parse silently discards (it stops parsing at the first
// unrecognized-as-flag token and never surfaces it) — intent landed as ""
// with no error at all. splitFlagsAndPositional now detects a value flag
// immediately followed by another flag-looking token and rewrites it to its
// explicit `name=` (empty value) form, so --intent is left untouched for its
// own turn and its value lands correctly.
func TestSendIntentNotSwallowedByDanglingValueFlag(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "bettor", "role": "consumer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []string{"send", "bettor", "body", "--subject", "--intent", "action-requested"}
	if err := Dispatch(args, &out); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := soleThreadIntent(t); got != "action-requested" {
		t.Fatalf("expected intent action-requested (not swallowed by dangling --subject), got %q", got)
	}
}

func TestTasksCommandShowsOnlyTasks(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "rev", "role": "reviewer", "model_type": "codex"}); err != nil {
		t.Fatal(err)
	}
	// One message and one task addressed to rev's role.
	if _, err := callData("send_message", map[string]any{"from": "backend", "to_kind": "role", "to_target": "reviewer", "subject": "just a note", "body": "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("task_create", map[string]any{"from": "backend", "to_kind": "role", "to_target": "reviewer", "subject": "please review", "body": "y"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Dispatch([]string{"tasks", "rev"}, &buf); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "please review") {
		t.Fatalf("tasks output missing the task:\n%s", out)
	}
	if strings.Contains(out, "just a note") {
		t.Fatalf("tasks output should exclude the plain message:\n%s", out)
	}
}

func TestNudgeCommandRejectsUnknownAlias(t *testing.T) {
	startTestDaemon(t)
	var buf bytes.Buffer
	if err := Dispatch([]string{"nudge", "ghost"}, &buf); err == nil {
		t.Fatalf("expected error nudging an unregistered alias")
	}
}

func TestNudgeCommandResolvesAndNudges(t *testing.T) {
	startTestDaemon(t)
	// register an agent with a pane via the daemon op directly
	if _, err := callData("register_agent", map[string]any{"alias": "rev", "role": "reviewer", "model_type": "codex", "socket_path": "/s", "pane_id": "%2", "session_id": "$1"}); err != nil {
		t.Fatal(err)
	}
	var recorded [][]string
	origNudge := nudgeRun
	nudgeRun = func(args ...string) error { recorded = append(recorded, args); return nil }
	t.Cleanup(func() { nudgeRun = origNudge })

	var buf bytes.Buffer
	if err := Dispatch([]string{"nudge", "rev"}, &buf); err != nil {
		t.Fatalf("nudge: %v", err)
	}
	if !strings.Contains(buf.String(), "rev") || len(recorded) == 0 {
		t.Fatalf("expected resolved-target output + a send-keys call; out=%q calls=%v", buf.String(), recorded)
	}
}

// TestNudgePrintsLiveSessionName proves nudge reports the session's CURRENT
// tmux name, not the stale registration-time snapshot: session names are
// mutable (a human can `tmux rename-session` any time), so a stored
// session_name goes stale the moment that happens — the production report
// was 'bettor-help-workspace' printed for what tmux itself now calls
// 'bettor-help-workspace-4'.
func TestNudgePrintsLiveSessionName(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "rev", "role": "reviewer", "model_type": "codex",
		"socket_path": "/s", "pane_id": "%2", "session_id": "$1",
		"session_name": "stale-name",
	}); err != nil {
		t.Fatal(err)
	}

	origNudge := nudgeRun
	nudgeRun = func(_ ...string) error { return nil }
	t.Cleanup(func() { nudgeRun = origNudge })

	origRun := tmuxenv.Run
	tmuxenv.Run = hookRun(map[string]string{"#{session_name}": "renamed-live"})
	t.Cleanup(func() { tmuxenv.Run = origRun })

	var buf bytes.Buffer
	if err := Dispatch([]string{"nudge", "rev"}, &buf); err != nil {
		t.Fatalf("nudge: %v", err)
	}
	if !strings.Contains(buf.String(), "renamed-live") {
		t.Fatalf("expected nudge output to report the LIVE session name, got %q", buf.String())
	}
	if strings.Contains(buf.String(), "stale-name") {
		t.Fatalf("nudge output must not show the stale stored session name once the live query succeeds, got %q", buf.String())
	}
}

// TestNudgeFallsBackToStoredSessionNameWhenLiveQueryFails: when the live
// tmux query can't answer (session gone, tmux unreachable), nudge must fall
// back to the stored session_name rather than printing a blank field.
func TestNudgeFallsBackToStoredSessionNameWhenLiveQueryFails(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{
		"alias": "rev", "role": "reviewer", "model_type": "codex",
		"socket_path": "/s", "pane_id": "%2", "session_id": "$1",
		"session_name": "stored-name",
	}); err != nil {
		t.Fatal(err)
	}

	origNudge := nudgeRun
	nudgeRun = func(_ ...string) error { return nil }
	t.Cleanup(func() { nudgeRun = origNudge })

	origRun := tmuxenv.Run
	tmuxenv.Run = func(_ ...string) (string, error) { return "", fmt.Errorf("no tmux") }
	t.Cleanup(func() { tmuxenv.Run = origRun })

	var buf bytes.Buffer
	if err := Dispatch([]string{"nudge", "rev"}, &buf); err != nil {
		t.Fatalf("nudge: %v", err)
	}
	if !strings.Contains(buf.String(), "stored-name") {
		t.Fatalf("expected fallback to the stored session name when the live query fails, got %q", buf.String())
	}
}

// TestNudgeSelfReportsJournalRow: after a successful nudge, a "nudge" event
// row exists for the target alias (best-effort log_event call from cmdNudge).
func TestNudgeSelfReportsJournalRow(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "rev", "role": "reviewer", "model_type": "codex", "socket_path": "/s", "pane_id": "%2", "session_id": "$1"}); err != nil {
		t.Fatal(err)
	}
	origNudge := nudgeRun
	nudgeRun = func(_ ...string) error { return nil }
	t.Cleanup(func() { nudgeRun = origNudge })

	var buf bytes.Buffer
	if err := Dispatch([]string{"nudge", "rev"}, &buf); err != nil {
		t.Fatalf("nudge: %v", err)
	}

	raw, err := callData("list_events", map[string]any{"kind": "nudge", "backlog": true, "limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Events []struct {
			Kind   string `json:"kind"`
			Target string `json:"target"`
			Detail string `json:"detail"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || res.Events[0].Target != "rev" || res.Events[0].Detail != "submitted" {
		t.Fatalf("expected 1 nudge event for rev/submitted, got %+v", res.Events)
	}
}

// TestNudgeSelfReportsTypedWhenNoSubmit: when nudging with --no-submit flag,
// the journal records detail="typed" (not submitted).
func TestNudgeSelfReportsTypedWhenNoSubmit(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "rev", "role": "reviewer", "model_type": "codex", "socket_path": "/s", "pane_id": "%2", "session_id": "$1"}); err != nil {
		t.Fatal(err)
	}
	origNudge := nudgeRun
	nudgeRun = func(_ ...string) error { return nil }
	t.Cleanup(func() { nudgeRun = origNudge })

	var buf bytes.Buffer
	if err := Dispatch([]string{"nudge", "--no-submit", "rev"}, &buf); err != nil {
		t.Fatalf("nudge --no-submit: %v", err)
	}

	raw, err := callData("list_events", map[string]any{"kind": "nudge", "backlog": true, "limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Events []struct {
			Kind   string `json:"kind"`
			Target string `json:"target"`
			Detail string `json:"detail"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || res.Events[0].Target != "rev" || res.Events[0].Detail != "typed" {
		t.Fatalf("expected 1 nudge event for rev/typed, got %+v", res.Events)
	}
}

func TestSplitFlagsAndPositional(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		wantFlagArgs   []string
		wantPositional []string
	}{
		{
			name:           "flags after positionals",
			args:           []string{"consumer", "the body", "--from", "backend", "--subject", "heads up"},
			wantFlagArgs:   []string{"--from", "backend", "--subject", "heads up"},
			wantPositional: []string{"consumer", "the body"},
		},
		{
			name:           "boolean flag does not consume the next token",
			args:           []string{"rev", "please review", "--role"},
			wantFlagArgs:   []string{"--role"},
			wantPositional: []string{"rev", "please review"},
		},
		{
			name:           "broadcast bool flag plus body",
			args:           []string{"--broadcast", "hello world"},
			wantFlagArgs:   []string{"--broadcast"},
			wantPositional: []string{"hello world"},
		},
		{
			name:           "equals form keeps flag and value together",
			args:           []string{"--from=backend", "x", "y"},
			wantFlagArgs:   []string{"--from=backend"},
			wantPositional: []string{"x", "y"},
		},
		{
			name:           "missing value at end does not panic",
			args:           []string{"a", "b", "--from"},
			wantFlagArgs:   []string{"--from"},
			wantPositional: []string{"a", "b"},
		},
		{
			name:           "dangling value flag followed by another flag does not swallow it",
			args:           []string{"a", "b", "--subject", "--intent", "action-requested"},
			wantFlagArgs:   []string{"--subject=", "--intent", "action-requested"},
			wantPositional: []string{"a", "b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flagArgs, positional := splitFlagsAndPositional(tc.args)
			if !reflect.DeepEqual(flagArgs, tc.wantFlagArgs) {
				t.Errorf("flagArgs = %#v, want %#v", flagArgs, tc.wantFlagArgs)
			}
			if !reflect.DeepEqual(positional, tc.wantPositional) {
				t.Errorf("positional = %#v, want %#v", positional, tc.wantPositional)
			}
		})
	}
}
