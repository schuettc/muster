package channel

import (
	"strings"
	"testing"
)

// splitPush returns the envelope line and the guidance block of one push, or
// fails the test if the Separator is absent or present more than once.
func splitPush(t *testing.T, content string) (string, string) {
	t.Helper()
	if n := strings.Count(content, Separator); n != 1 {
		t.Fatalf("push must carry exactly one separator, got %d in %q", n, content)
	}
	parts := strings.SplitN(content, Separator, 2)
	return parts[0], parts[1]
}

func TestFormatSingleActionRequested(t *testing.T) {
	content, meta := Format([]Event{{ID: 9, Kind: "send", Agent: "reviewer", ThreadID: 42, Subject: "review the channel branch", Intent: "action-requested"}}, nil)
	line, guide := splitPush(t, content)
	if want := `muster: action-requested from reviewer on thread #42 "review the channel branch"`; line != want {
		t.Errorf("envelope:\n got %q\nwant %q", line, want)
	}
	if !strings.Contains(guide, "get_thread 42") || !strings.Contains(guide, "reply") || !strings.Contains(guide, "Act autonomously") {
		t.Errorf("action-requested guidance must name the thread, the reply, and autonomy: %q", guide)
	}
	for k, v := range map[string]string{"kind": "send", "from": "reviewer", "thread_id": "42", "intent": "action-requested", "count": "1"} {
		if meta[k] != v {
			t.Errorf("meta[%s] = %q, want %q", k, meta[k], v)
		}
	}
}

func TestFormatSingleFyiForbidsReply(t *testing.T) {
	content, _ := Format([]Event{{Kind: "send", Agent: "ops", ThreadID: 41, Subject: "deploy done", Intent: "fyi"}}, nil)
	line, guide := splitPush(t, content)
	if !strings.Contains(line, "fyi from ops on thread #41") {
		t.Errorf("envelope: %q", line)
	}
	if !strings.Contains(guide, "get_thread 41") || !strings.Contains(guide, "do not reply") {
		t.Errorf("fyi guidance must say do not reply: %q", guide)
	}
}

func TestFormatSingleReplyRequested(t *testing.T) {
	content, _ := Format([]Event{{Kind: "send", Agent: "lead", ThreadID: 40, Subject: "plan", Intent: "reply-requested"}}, nil)
	_, guide := splitPush(t, content)
	if !strings.Contains(guide, "needs an answer") || !strings.Contains(guide, "get_thread 40") {
		t.Errorf("reply-requested guidance: %q", guide)
	}
}

func TestFormatReplyLabelsAsReplyAndGuidesByThreadIntent(t *testing.T) {
	content, meta := Format([]Event{{Kind: "reply", Agent: "lead", ThreadID: 40, Subject: "plan", Intent: "reply-requested"}}, nil)
	line, guide := splitPush(t, content)
	if !strings.HasPrefix(line, `muster: reply from lead on thread #40 "plan"`) {
		t.Errorf("reply events are labeled reply, not by thread intent: %q", line)
	}
	if meta["kind"] != "reply" {
		t.Errorf("meta kind = %q", meta["kind"])
	}
	if !strings.Contains(guide, "replied on your thread") || !strings.Contains(guide, "only if the sender still needs") {
		t.Errorf("reply guidance must not oblige a reply unconditionally: %q", guide)
	}
	closing, _ := Format([]Event{{Kind: "reply", Agent: "lead", ThreadID: 40, Subject: "plan", Intent: "fyi"}}, nil)
	_, cg := splitPush(t, closing)
	if !strings.Contains(cg, "no reply needed") {
		t.Errorf("a fyi reply is a closing reply: %q", cg)
	}
}

func TestFormatBatchListsItemsUpToCapAndNamesStrictest(t *testing.T) {
	content, meta := Format([]Event{
		{Kind: "send", Agent: "ops", ThreadID: 41, Subject: "deploy done", Intent: "fyi"},
		{Kind: "reply", Agent: "lead", ThreadID: 40, Subject: "plan", Intent: "reply-requested"},
		{Kind: "task", Agent: "reviewer", ThreadID: 42, Subject: "review", Intent: "action-requested"},
	}, nil)
	line, guide := splitPush(t, content)
	want := `muster: 3 new (strictest: action-requested) — fyi from ops on #41 "deploy done"; reply from lead on #40 "plan"; action-requested from reviewer on #42 "review"`
	if line != want {
		t.Errorf("envelope:\n got %q\nwant %q", line, want)
	}
	if !strings.Contains(guide, "get_inbox") || !strings.Contains(guide, "fyi → read only") {
		t.Errorf("batch guidance must route to get_inbox and state every obligation: %q", guide)
	}
	if meta["count"] != "3" || meta["intent"] != "action-requested" || meta["kind"] != "batch" {
		t.Errorf("batch meta carries count + strictest intent: %v", meta)
	}
	for _, k := range []string{"thread_id", "from"} {
		if _, ok := meta[k]; ok {
			t.Errorf("batch meta must not carry a per-event %s (it would be false for the rest): %v", k, meta)
		}
	}
}

func TestFormatBatchCollapsesAboveCap(t *testing.T) {
	old := MaxListed
	MaxListed = 2
	t.Cleanup(func() { MaxListed = old })
	content, meta := Format([]Event{
		{Kind: "send", Agent: "a", ThreadID: 1, Subject: "one", Intent: "fyi"},
		{Kind: "send", Agent: "b", ThreadID: 2, Subject: "two", Intent: "fyi"},
		{Kind: "send", Agent: "c", ThreadID: 3, Subject: "three", Intent: "reply-requested"},
	}, nil)
	line, guide := splitPush(t, content)
	if line != "muster: 3 new (strictest: reply-requested)" {
		t.Errorf("above the cap the envelope is count + strictest only: %q", line)
	}
	if !strings.Contains(guide, "get_inbox") {
		t.Errorf("collapsed batch still routes to get_inbox: %q", guide)
	}
	if meta["count"] != "3" || meta["intent"] != "reply-requested" {
		t.Errorf("meta: %v", meta)
	}
}

func TestStrictestOrdering(t *testing.T) {
	cases := []struct {
		intents []string
		want    string
	}{
		{[]string{"fyi", "reply-requested"}, "reply-requested"},
		{[]string{"reply-requested", "action-requested", "fyi"}, "action-requested"},
		{[]string{"fyi", "fyi"}, "fyi"},
		{[]string{"", ""}, ""},
	}
	for _, c := range cases {
		var evs []Event
		for _, in := range c.intents {
			evs = append(evs, Event{Intent: in})
		}
		if got := strictest(evs); got != c.want {
			t.Errorf("strictest(%v) = %q, want %q", c.intents, got, c.want)
		}
	}
}

// Presence tests: the rules that exist because of real incidents must not
// be quietly edited away. Each intent's guidance keeps its load-bearing phrase.
func TestGuidancePresencePerIntent(t *testing.T) {
	checks := map[string][]string{
		"action-requested": {"get_thread 7", "reply", "do not ask the user"},
		"reply-requested":  {"get_thread 7", "reply"},
		"fyi":              {"get_thread 7", "do not reply"},
	}
	for intent, phrases := range checks {
		g := guidance(Event{Kind: "send", ThreadID: 7, Intent: intent}, nil)
		for _, p := range phrases {
			if !strings.Contains(g, p) {
				t.Errorf("%s guidance lost %q: %q", intent, p, g)
			}
		}
	}
	if !strings.Contains(batchGuidance, "never reply") || !strings.Contains(batchGuidance, "get_inbox") {
		t.Errorf("batch guidance lost a load-bearing phrase: %q", batchGuidance)
	}
}

// TestDrainClauseNamesCLIAndAlias locks in the A fix: the channel push must
// tell the agent that reading a thread does NOT clear the badge, and give it
// the drain in BOTH tool and CLI form, named to the session's own alias — the
// CLI-only mistranslation ('muster inbox <a nearby label>' = a peek that
// clears nothing) is exactly what left the badge lit and re-notifying.
func TestDrainClauseNamesCLIAndAlias(t *testing.T) {
	mine := []string{"tools-workspace/auto-review-audit"}
	// single event
	content, _ := Format([]Event{{Kind: "reply", Agent: "lead", ThreadID: 40, Subject: "plan", Intent: "reply-requested"}}, mine)
	_, guide := splitPush(t, content)
	for _, want := range []string{
		"does not clear the", // states the badge is not cleared by reading
		"get_inbox tool for alias 'tools-workspace/auto-review-audit'", // tool is the method, named alias
		"no muster MCP tools?",                             // CLI is the fallback for a session without tools, not a coequal option
		"muster inbox 'tools-workspace/auto-review-audit'", // CLI form, named alias
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("single-event drain clause missing %q: %q", want, guide)
		}
	}
	// batch
	batch, _ := Format([]Event{
		{Kind: "send", Agent: "a", ThreadID: 1, Subject: "one", Intent: "fyi"},
		{Kind: "send", Agent: "b", ThreadID: 2, Subject: "two", Intent: "reply-requested"},
	}, mine)
	_, bg := splitPush(t, batch)
	if !strings.Contains(bg, "muster inbox 'tools-workspace/auto-review-audit'") {
		t.Errorf("batch drain clause must name the CLI drain: %q", bg)
	}
	// multi-alias: every alias named, generic CLI placeholder
	multi, _ := Format([]Event{{Kind: "reply", Agent: "lead", ThreadID: 40, Intent: "reply-requested"}}, []string{"a", "b"})
	_, mg := splitPush(t, multi)
	if !strings.Contains(mg, "'a', 'b'") || !strings.Contains(mg, "muster inbox <alias>") {
		t.Errorf("multi-alias drain clause must name each alias: %q", mg)
	}
	// no alias resolved yet: generic placeholder, still names the CLI
	none, _ := Format([]Event{{Kind: "reply", Agent: "lead", ThreadID: 40, Intent: "reply-requested"}}, nil)
	_, ng := splitPush(t, none)
	if !strings.Contains(ng, "muster inbox <your-alias>") {
		t.Errorf("alias-less drain clause must fall back to the generic CLI form: %q", ng)
	}
}

func TestFormatFallsBackToKindAndDetail(t *testing.T) {
	content, _ := Format([]Event{{Kind: "task", Agent: "ops", ThreadID: 7, Detail: "from detail"}}, nil)
	line, guide := splitPush(t, content)
	if !strings.Contains(line, `task from ops on thread #7 "from detail"`) {
		t.Errorf("empty intent → kind label, empty subject → detail: %q", line)
	}
	if !strings.Contains(guide, "get_thread 7") {
		t.Errorf("fallback guidance still names the thread: %q", guide)
	}
}

func TestSummary(t *testing.T) {
	content, meta := Summary(2, nil)
	line, guide := splitPush(t, content)
	if line != "muster: 2 unread message(s) waiting" {
		t.Errorf("summary envelope: %q", line)
	}
	if !strings.Contains(guide, "get_inbox") {
		t.Errorf("summary guidance: %q", guide)
	}
	if meta["kind"] != "summary" || meta["count"] != "2" {
		t.Errorf("summary meta: %v", meta)
	}
}

func TestFormatEmptyIsEmpty(t *testing.T) {
	content, meta := Format(nil, nil)
	if content != "" || meta != nil {
		t.Errorf("nothing to say: %q %v", content, meta)
	}
}
