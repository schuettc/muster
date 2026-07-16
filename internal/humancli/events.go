package humancli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"
)

// eventRow mirrors store.Event's wire JSON.
type eventRow struct {
	TS       int64  `json:"ts"`
	Kind     string `json:"kind"`
	Agent    string `json:"agent"`
	ThreadID int64  `json:"thread_id"`
	Count    int    `json:"count"`
	Detail   string `json:"detail"`
}

// cmdEvents prints the daemon's observability event log — every mailbox
// notify outcome (lit / cleared / skipped / error) and inbox read, oldest
// first. This is the "when was whose mailbox actually lit" answer.
func cmdEvents(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agent := fs.String("agent", "", "only events for this agent alias")
	limit := fs.Int("limit", 50, "max events to show (default 50)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: muster events [--agent <alias>] [--limit <n>]")
	}
	raw, err := callData("list_events", map[string]any{"agent": *agent, "limit": *limit, "backlog": true})
	if err != nil {
		return err
	}
	var res struct {
		Events []eventRow `json:"events"`
		MaxID  int64      `json:"max_id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	events := res.Events
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "TIME\tKIND\tAGENT\tTHREAD\tCOUNT\tDETAIL"); err != nil {
		return err
	}
	// The daemon returns newest first; print oldest first so the log reads
	// top to bottom like a timeline.
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		thread := ""
		if e.ThreadID != 0 {
			thread = fmt.Sprintf("%d", e.ThreadID)
		}
		ts := time.UnixMilli(e.TS).Format("01-02 15:04:05")
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", ts, e.Kind, e.Agent, thread, e.Count, e.Detail); err != nil {
			return err
		}
	}
	return tw.Flush()
}
