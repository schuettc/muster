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

// MaxListed is the largest batch that still lists every thread on the
// envelope line. Above it the push collapses to a count and the strictest
// intent — at that size the list is noise and get_inbox is the right lookup.
// A knob (MUSTER_CHANNEL_MAX_LISTED), not a constant.
var MaxListed = 5

// Separator divides the envelope line from the guidance that travels with
// the push. It is a convention INSIDE content, not a schema field: a client
// that splits on the first occurrence gets envelope and guidance apart and
// can dedupe identical guidance across a batch; a client that does not
// split (Claude Code) just renders a readable rule ahead of the guidance.
const Separator = "\n---\n"

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

// intentRank orders intents by obligation: an action outranks an answer,
// which outranks a read. Unknown or empty intents rank lowest.
func intentRank(intent string) int {
	switch intent {
	case "action-requested":
		return 3
	case "reply-requested":
		return 2
	case "fyi":
		return 1
	}
	return 0
}

// strictest is the intent a batch summary names, so urgency survives the
// collapse of many events into one line. Empty when no event carries one.
func strictest(events []Event) string {
	best := ""
	for _, e := range events {
		if intentRank(e.Intent) > intentRank(best) {
			best = e.Intent
		}
	}
	return best
}

// drainSuffix closes every per-event rule: get_thread and reply never move
// the read watermark or touch the tmux badge (only an owned get_inbox does,
// via the daemon's setSessionBadge), so a channel-handled thread would leave
// the operator's 📬 lit forever without this final drain.
const drainSuffix = " Finish by calling get_inbox for your alias — that marks the mail read and clears the inbox badge."

// guidance is the rule that travels with ONE event: what this push obliges
// the agent to do, stated next to the action it governs rather than in a
// handshake blob the agent read thousands of tokens ago.
func guidance(e Event) string {
	id := e.ThreadID
	if e.Kind == "reply" {
		if e.Intent == "fyi" {
			return fmt.Sprintf("A closing reply on your thread: read it with get_thread %d; no reply needed.", id) + drainSuffix
		}
		return fmt.Sprintf("Someone replied on your thread: call get_thread %d and reply only if the sender still needs something from you; close out with fyi so nobody is woken.", id) + drainSuffix
	}
	switch e.Intent {
	case "action-requested":
		return fmt.Sprintf("This thread asks you to do something: call get_thread %d, do what it asks, then answer with reply. Act autonomously — do not ask the user whether to check mail.", id) + drainSuffix
	case "reply-requested":
		return fmt.Sprintf("The sender needs an answer: call get_thread %d, then reply with it. Act autonomously.", id) + drainSuffix
	case "fyi":
		return fmt.Sprintf("Informational only: read it with get_thread %d; do not reply — a reply would wake the sender.", id) + drainSuffix
	}
	return fmt.Sprintf("Call get_thread %d, act on it, then reply.", id) + drainSuffix
}

// batchGuidance is the rule for a summary line that stands for many events.
// It names every obligation once; the client keeps whichever apply.
const batchGuidance = "Call get_inbox and work through each thread. action-requested → do it and reply; reply-requested → answer with reply; fyi → read only, never reply. Act autonomously — do not ask the user whether to check mail."

// Format renders one push for everything one poll tick found: an envelope
// line, the Separator, then guidance. The body never travels. With one
// event, meta carries its identity (kind, from, thread_id, intent); with
// several, meta carries only count and the strictest intent — a thread_id
// taken from the first event would be actively false for the rest.
func Format(events []Event) (string, map[string]string) {
	if len(events) == 0 {
		return "", nil
	}
	if len(events) == 1 {
		first := events[0]
		meta := map[string]string{
			"kind":      first.Kind,
			"from":      first.Agent,
			"thread_id": strconv.FormatInt(first.ThreadID, 10),
			"intent":    first.Intent,
			"count":     "1",
		}
		line := fmt.Sprintf("muster: %s from %s on thread #%d %q", label(first), first.Agent, first.ThreadID, subject(first))
		return line + Separator + guidance(first), meta
	}
	top := strictest(events)
	meta := map[string]string{
		"kind":   "batch",
		"intent": top,
		"count":  strconv.Itoa(len(events)),
	}
	line := fmt.Sprintf("muster: %d new", len(events))
	if top != "" {
		line += fmt.Sprintf(" (strictest: %s)", top)
	}
	if len(events) <= MaxListed {
		items := make([]string, 0, len(events))
		for _, e := range events {
			items = append(items, fmt.Sprintf("%s from %s on #%d %q", label(e), e.Agent, e.ThreadID, subject(e)))
		}
		line += " — " + strings.Join(items, "; ")
	}
	return line + Separator + batchGuidance, meta
}

// Summary renders the startup push for mail that was already waiting when
// the channel attached. Same envelope/guidance shape as Format.
func Summary(unread int) (string, map[string]string) {
	line := fmt.Sprintf("muster: %d unread message(s) waiting", unread)
	return line + Separator + batchGuidance, map[string]string{"kind": "summary", "count": strconv.Itoa(unread)}
}
