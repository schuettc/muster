package humancli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
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

const (
	whoMaxWidth      = 34  // WHO column cap (runes)
	whoSenderWidth   = 15  // sender half of a 'from → to' pair
	defaultLineWidth = 120 // line budget when $COLUMNS is unset; --width overrides
)

// renderer holds the display state shared by events and watch: the
// alias→label map, column widths, and time format. Widths are seeded from
// the rows about to be printed (events) or the backlog (watch) and only
// grow, so columns stay aligned to real content instead of guessed fixed
// widths while watch streams.
type renderer struct {
	labels   map[string]string // alias → current label ("" or missing = show the alias)
	aliases  bool              // true = raw aliases, ignore labels
	fullTime bool              // true = date + time; false = time only
	whoW     int
	threadW  int
	width    int // total line budget (terminal columns)
}

// newRenderer sizes columns from rows. labels may be nil (aliases render
// as-is). width <= 0 falls back to $COLUMNS, then defaultLineWidth.
func newRenderer(rows []eventRow, labels map[string]string, aliases, fullTime bool, width int) *renderer {
	r := &renderer{labels: labels, aliases: aliases, fullTime: fullTime, whoW: 6, threadW: 3, width: width}
	if r.width <= 0 {
		if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && c > 40 {
			r.width = c
		} else {
			r.width = defaultLineWidth
		}
	}
	for _, e := range rows {
		r.fit(e)
	}
	return r
}

// fit grows column widths to accommodate e; it never shrinks them, keeping
// alignment stable across a streamed tail.
func (r *renderer) fit(e eventRow) {
	if n := len([]rune(r.who(e))); n > r.whoW {
		r.whoW = min(n, whoMaxWidth)
	}
	if n := len(r.thread(e)); n > r.threadW {
		r.threadW = n
	}
}

// disp resolves an alias for display: the agent's current label when one is
// known (pinned or auto topic), the alias otherwise. Labels resolve at
// render time, so old events show whoever the agent is *today* — use
// --aliases for the stable raw view.
func (r *renderer) disp(alias string) string {
	if !r.aliases {
		if l := r.labels[alias]; l != "" {
			return l
		}
	}
	return alias
}

// dispTarget renders a journal target ('agent:x' / 'role:r' / 'broadcast' /
// bare alias) for display.
func (r *renderer) dispTarget(target string) string {
	if a, ok := strings.CutPrefix(target, "agent:"); ok {
		return r.disp(a)
	}
	if strings.HasPrefix(target, "role:") || target == "broadcast" || target == "" {
		return target
	}
	return r.disp(target) // nudge's bare alias
}

// who renders the direction column. Sends and tasks are directed at an
// agent or role and show 'from → to'; notifies and nudges are deliveries TO
// the shown agent ('→ x'); replies, claims, and transitions are directed at
// the THREAD — the paired notify rows below show who that fanned out to —
// so they show the actor alone.
func (r *renderer) who(e eventRow) string {
	switch e.Kind {
	case "send", "task":
		return truncate(r.disp(e.Agent), whoSenderWidth) + " → " + r.dispTarget(e.Target)
	case "notify":
		return "→ " + r.disp(e.Agent)
	case "nudge":
		return "→ " + r.dispTarget(e.Target)
	default:
		return r.disp(e.Agent)
	}
}

// thread renders '#<id>' or nothing for thread-less events.
func (r *renderer) thread(e eventRow) string {
	if e.ThreadID == 0 {
		return ""
	}
	return "#" + strconv.FormatInt(e.ThreadID, 10)
}

// what renders the payload column without repeating itself: a send/task
// detail duplicates the subject by construction, so only the subject prints;
// notify folds its unread count into the outcome ('lit(2)') and appends the
// subject for context.
func (r *renderer) what(e eventRow) string {
	subject := oneLine(e.Subject)
	detail := oneLine(e.Detail)
	switch e.Kind {
	case "send", "task", "reply", "claim":
		if subject != "" {
			return subject
		}
		return detail
	case "notify":
		out := detail
		if e.Count > 0 {
			out = fmt.Sprintf("%s(%d)", detail, e.Count)
		}
		if subject != "" {
			out += " — " + subject
		}
		return out
	case "transition":
		if subject != "" {
			return detail + " — " + subject
		}
		return detail
	default: // read, nudge
		return detail
	}
}

func (r *renderer) timeFormat() string {
	if r.fullTime {
		return "2006-01-02 15:04:05"
	}
	return "15:04:05"
}

// header writes the column header line shared by events and watch.
func (r *renderer) header(w io.Writer) {
	_, _ = fmt.Fprintf(w, "%-*s  %-6s  %-*s  %-*s  %s\n",
		len(r.timeFormat()), "TIME", "KIND", r.whoW, "WHO", r.threadW, "THREAD", "WHAT")
}

// line writes exactly one line for e — safe for line-by-line streaming (no
// long-lived tabwriter that only aligns at Flush). The WHAT column is capped
// to the remaining terminal width so a row can never wrap.
func (r *renderer) line(w io.Writer, e eventRow) {
	r.fit(e)
	ts := time.UnixMilli(e.TS).Format(r.timeFormat())
	used := len(r.timeFormat()) + 2 + 6 + 2 + r.whoW + 2 + r.threadW + 2
	budget := r.width - used
	if budget < 10 {
		budget = 10
	}
	_, _ = fmt.Fprintf(w, "%-*s  %-6s  %-*s  %-*s  %s\n",
		len(r.timeFormat()), ts, e.Kind, r.whoW, truncate(r.who(e), whoMaxWidth), r.threadW, r.thread(e), truncate(r.what(e), budget))
}

// loadLabels fetches the current alias→label map, best-effort: on any error
// the map is nil and aliases render as-is. Labels are LIVE (enrichAgents
// queries tmux for each live agent's current label, exactly like `muster
// agents`), so the journal reads in today's terms.
func loadLabels() map[string]string {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return nil
	}
	var rows []agentRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	m := make(map[string]string, len(rows))
	for _, a := range enrichAgents(rows) {
		m[a.Alias] = a.EffLabel
	}
	return m
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
	aliases := fs.Bool("aliases", false, "show raw aliases instead of current labels")
	fullTime := fs.Bool("full-time", false, "show dates, not just times")
	width := fs.Int("width", 0, "line budget in columns (default $COLUMNS or 120)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: muster events [--agent <alias>] [--kind <kind>] [--thread <id>] [--limit <n>] [--aliases] [--full-time] [--width <cols>]")
	}
	page, err := fetchEvents(*agent, *kind, *thread, -1, *limit)
	if err != nil {
		return err
	}
	r := newRenderer(page.Events, loadLabels(), *aliases, *fullTime, *width)
	r.header(out)
	// The daemon returns newest first; print oldest first so the log reads
	// top to bottom like a timeline.
	for i := len(page.Events) - 1; i >= 0; i-- {
		r.line(out, page.Events[i])
	}
	return nil
}
