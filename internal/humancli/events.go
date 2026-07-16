package humancli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// eventRow mirrors store.Event's wire JSON.
type eventRow struct {
	ID       int64  `json:"id"`
	TS       int64  `json:"ts"`
	Kind     string `json:"kind"`
	Agent    string `json:"agent"`
	Target   string `json:"target"`
	ThreadID int64  `json:"thread_id"`
	Count    int    `json:"count"`
	Detail   string `json:"detail"`
	Subject  string `json:"subject"`
}

// eventsPage is the decoded {events, max_id} envelope list_events returns.
type eventsPage struct {
	Events []eventRow `json:"events"`
	MaxID  int64      `json:"max_id"`
}

// fetchEvents calls list_events with the given filters. afterID < 0 selects
// backlog mode (send backlog:true + limit, omit after_id); afterID >= 0
// selects follow mode (send after_id as a decimal string, omit backlog) —
// never both in the same call.
func fetchEvents(agent, kind string, threadID, afterID int64, limit int) (eventsPage, error) {
	args := map[string]any{"agent": agent, "kind": kind, "thread_id": threadID, "limit": limit}
	if afterID >= 0 {
		args["after_id"] = strconv.FormatInt(afterID, 10)
	} else {
		args["backlog"] = true
	}
	raw, err := callData("list_events", args)
	if err != nil {
		return eventsPage{}, err
	}
	var page eventsPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return eventsPage{}, err
	}
	return page, nil
}

// oneLine collapses tabs/newlines/carriage-returns to spaces so a single
// event never spans more than one output line.
func oneLine(s string) string {
	s = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
	return s
}

// truncate shortens s to at most n runes, appending an ellipsis if cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// eventHeader writes the column header line shared by events and watch.
func eventHeader(w io.Writer) {
	_, _ = fmt.Fprintf(w, "%-15s %-8s %-12s %-14s %-8s %-5s %-30s %s\n",
		"TIME", "KIND", "AGENT", "TARGET", "THREAD", "COUNT", "DETAIL", "SUBJECT")
}

// printEventLine writes exactly one line for e — safe for line-by-line
// streaming (no long-lived tabwriter that only aligns at Flush).
func printEventLine(w io.Writer, e eventRow) {
	thread := ""
	if e.ThreadID != 0 {
		thread = strconv.FormatInt(e.ThreadID, 10)
	}
	ts := time.UnixMilli(e.TS).Format("01-02 15:04:05")
	_, _ = fmt.Fprintf(w, "%-15s %-8s %-12s %-14s %-8s %-5d %-30s %s\n",
		ts, e.Kind, e.Agent, e.Target, thread, e.Count, oneLine(e.Detail), truncate(oneLine(e.Subject), 60))
}

// cmdEvents prints the daemon's observability event log — every mailbox
// notify outcome (lit / cleared / skipped / error), inbox read, send, task,
// and nudge event, oldest first. This is the "when was whose mailbox
// actually lit" answer.
func cmdEvents(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agent := fs.String("agent", "", "only events for this agent alias")
	kind := fs.String("kind", "", "only events of this kind")
	thread := fs.Int64("thread", 0, "only events for this thread id")
	limit := fs.Int("limit", 50, "max events to show (default 50)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: muster events [--agent <alias>] [--kind <kind>] [--thread <id>] [--limit <n>]")
	}
	page, err := fetchEvents(*agent, *kind, *thread, -1, *limit)
	if err != nil {
		return err
	}
	eventHeader(out)
	// The daemon returns newest first; print oldest first so the log reads
	// top to bottom like a timeline.
	for i := len(page.Events) - 1; i >= 0; i-- {
		printEventLine(out, page.Events[i])
	}
	return nil
}
