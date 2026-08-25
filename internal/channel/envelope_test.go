package channel

import (
	"strings"
	"testing"
)

func TestFormatSingleActionRequested(t *testing.T) {
	content, meta := Format([]Event{{ID: 9, Kind: "send", Agent: "reviewer", ThreadID: 42, Subject: "review the channel branch", Intent: "action-requested"}})
	want := `muster: action-requested from reviewer on thread #42 "review the channel branch" — call get_thread 42, act, then reply.`
	if content != want {
		t.Errorf("content:\n got %q\nwant %q", content, want)
	}
	for k, v := range map[string]string{"kind": "send", "from": "reviewer", "thread_id": "42", "intent": "action-requested", "count": "1"} {
		if meta[k] != v {
			t.Errorf("meta[%s] = %q, want %q", k, meta[k], v)
		}
	}
}

func TestFormatSingleFyiNeedsNoReply(t *testing.T) {
	content, _ := Format([]Event{{Kind: "send", Agent: "ops", ThreadID: 41, Subject: "deploy done", Intent: "fyi"}})
	if !strings.Contains(content, "fyi from ops on thread #41") || !strings.Contains(content, "no reply needed") {
		t.Errorf("fyi push must say no reply is needed: %q", content)
	}
}

func TestFormatReplyLabelsAsReply(t *testing.T) {
	content, meta := Format([]Event{{Kind: "reply", Agent: "lead", ThreadID: 40, Subject: "plan", Intent: "reply-requested"}})
	if !strings.HasPrefix(content, `muster: reply from lead on thread #40 "plan"`) {
		t.Errorf("reply events are labeled reply, not by thread intent: %q", content)
	}
	if meta["kind"] != "reply" {
		t.Errorf("meta kind = %q", meta["kind"])
	}
}

func TestFormatCoalescesABurst(t *testing.T) {
	content, meta := Format([]Event{
		{Kind: "send", Agent: "reviewer", ThreadID: 42, Subject: "review", Intent: "action-requested"},
		{Kind: "reply", Agent: "lead", ThreadID: 40, Subject: "plan", Intent: "reply-requested"},
		{Kind: "task", Agent: "ops", ThreadID: 41, Subject: "rotate keys", Intent: "action-requested"},
	})
	want := `muster: 3 new — action-requested from reviewer on #42 "review"; reply from lead on #40 "plan"; action-requested from ops on #41 "rotate keys" — call get_inbox.`
	if content != want {
		t.Errorf("content:\n got %q\nwant %q", content, want)
	}
	if meta["count"] != "3" || meta["thread_id"] != "42" || meta["from"] != "reviewer" {
		t.Errorf("meta describes the first event and the total: %v", meta)
	}
}

func TestFormatFallsBackToKindAndDetail(t *testing.T) {
	content, _ := Format([]Event{{Kind: "task", Agent: "ops", ThreadID: 7, Detail: "from detail"}})
	if !strings.Contains(content, `task from ops on thread #7 "from detail"`) {
		t.Errorf("empty intent → kind label, empty subject → detail: %q", content)
	}
}

func TestFormatEmptyIsEmpty(t *testing.T) {
	content, meta := Format(nil)
	if content != "" || meta != nil {
		t.Errorf("nothing to say: %q %v", content, meta)
	}
}
