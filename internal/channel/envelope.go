// Package channel is the muster channel carrier: it tails the bus journal on
// behalf of one session's aliases and pushes compact envelopes into that
// session over a claude/channel MCP server. It is a peer client of the daemon
// — it never reads the store or touches tmux options itself.
package channel

import (
	"fmt"
	"strconv"
	"strings"
)

// Event is one journal row as list_events returns it. Tags mirror
// store.Event's wire shape so a daemon response decodes straight into it
// without importing the store.
type Event struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	Agent    string `json:"agent"`
	Target   string `json:"target"`
	ThreadID int64  `json:"thread_id"`
	Detail   string `json:"detail"`
	Subject  string `json:"subject"`
	Intent   string `json:"intent"`
}

// label is the one word an agent reads first: a reply is a reply whatever
// the thread's intent; anything else is the thread's effective intent, or
// the bare event kind when the thread carries none.
func label(e Event) string {
	if e.Kind == "reply" {
		return "reply"
	}
	if e.Intent != "" {
		return e.Intent
	}
	return e.Kind
}

func subject(e Event) string {
	if e.Subject != "" {
		return e.Subject
	}
	return e.Detail
}

// Format renders one push for everything one poll tick found. The body never
// travels: the content line tells the agent what arrived and which tool to
// call; meta carries the same facts as strings for the harness. With several
// events, meta describes the first and count carries the total.
func Format(events []Event) (string, map[string]string) {
	if len(events) == 0 {
		return "", nil
	}
	first := events[0]
	meta := map[string]string{
		"kind":      first.Kind,
		"from":      first.Agent,
		"thread_id": strconv.FormatInt(first.ThreadID, 10),
		"intent":    first.Intent,
		"count":     strconv.Itoa(len(events)),
	}
	if len(events) == 1 {
		tail := fmt.Sprintf("call get_thread %d, act, then reply.", first.ThreadID)
		if label(first) == "fyi" {
			tail = fmt.Sprintf("read it with get_thread %d; no reply needed.", first.ThreadID)
		}
		return fmt.Sprintf("muster: %s from %s on thread #%d %q — %s", label(first), first.Agent, first.ThreadID, subject(first), tail), meta
	}
	items := make([]string, 0, len(events))
	for _, e := range events {
		items = append(items, fmt.Sprintf("%s from %s on #%d %q", label(e), e.Agent, e.ThreadID, subject(e)))
	}
	return fmt.Sprintf("muster: %d new — %s — call get_inbox.", len(events), strings.Join(items, "; ")), meta
}
