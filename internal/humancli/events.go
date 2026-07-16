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

	"github.com/schuettc/muster/internal/display"
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
	// Intent is the event's thread's EFFECTIVE intent (store's effectiveIntent),
	// joined at query time exactly like Subject: "" (unspecified) | "fyi" |
	// "reply-requested" | "action-requested". what() renders it as a tag on
	// send/task rows.
	Intent string `json:"intent"`
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

// rawFieldWidth bounds display.Sanitize's width cap for row fields (subject,
// detail, who) before their final column truncation in line() — large enough
// that it never bites ahead of the real budget, while still running every
// field through the one canonical sanitizer (control-char stripping +
// whitespace-run collapsing) rather than a bespoke oneLine/truncate pair.
const rawFieldWidth = 4096

const (
	whoMaxWidth      = 34  // WHO column cap (display columns)
	whoSenderWidth   = 15  // sender half of a 'from → to' pair (display columns)
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
	if n := display.Width(r.who(e)); n > r.whoW {
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
		return display.Sanitize(r.disp(e.Agent), whoSenderWidth) + " → " + r.dispTarget(e.Target)
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

// intentTag maps a thread's effective intent to the journal suffix what()
// appends on send/task rows — "" (unspecified) renders no tag. The three
// non-empty values mirror internal/store's IntentFYI/IntentReply/IntentAction
// vocabulary (see cmdSend's validIntents in humancli.go for why humancli
// doesn't import internal/store for this — it's a peer client of the daemon,
// not a store-internal package).
func intentTag(intent string) string {
	switch intent {
	case "fyi":
		return " [fyi]"
	case "reply-requested":
		return " [reply?]"
	case "action-requested":
		return " [action]"
	default:
		return ""
	}
}

// what renders the payload column without repeating itself: a send/task
// detail duplicates the subject by construction, so only the subject prints,
// with an intent tag appended when the thread's effective intent is set;
// notify folds its unread count into the outcome ('lit(2)') and appends the
// subject for context; a reply with a non-empty body preview (Detail) shows
// '↳ <preview>' instead of the thread subject — showing both makes an
// announcement and its reply look like a duplicate send.
func (r *renderer) what(e eventRow) string {
	subject := display.Sanitize(e.Subject, rawFieldWidth)
	detail := display.Sanitize(e.Detail, rawFieldWidth)
	switch e.Kind {
	case "send", "task":
		if subject != "" {
			return subject + intentTag(e.Intent)
		}
		return detail + intentTag(e.Intent)
	case "reply":
		if detail != "" {
			return "↳ " + detail
		}
		if subject != "" {
			return subject
		}
		return detail
	case "claim":
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

// padDisplay right-pads s with spaces to w DISPLAY COLUMNS (not rune count),
// matching the units display.Width and display.Sanitize already use. fmt's
// "%-*s" pads by rune count, which misaligns columns and blows the line
// budget once a field holds wide (CJK/fullwidth) runes — a rune-counted pad
// adds more spaces than the display actually needs, since each wide rune
// already occupies 2 columns worth of the target width. Every column whose
// content can legitimately hold non-ASCII (WHO, and — for symmetry — TIME/
// THREAD, which are ASCII in practice but should never silently rely on
// that) goes through this helper instead.
func padDisplay(s string, w int) string {
	if n := w - display.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// header writes the column header line shared by events and watch.
func (r *renderer) header(w io.Writer) {
	_, _ = fmt.Fprintf(w, "%s  %-6s  %s  %s  %s\n",
		padDisplay("TIME", len(r.timeFormat())), "KIND", padDisplay("WHO", r.whoW), padDisplay("THREAD", r.threadW), "WHAT")
}

// line writes exactly one line for e — safe for line-by-line streaming (no
// long-lived tabwriter that only aligns at Flush). The WHAT column is capped
// to the remaining terminal width so a row can never wrap. Column padding is
// display-width based throughout (padDisplay), matching the `used` budget
// math below, which sums display-width column widths — the two must stay in
// the same unit or the WHAT truncation cap drifts from the line's real
// rendered width.
func (r *renderer) line(w io.Writer, e eventRow) {
	r.fit(e)
	ts := time.UnixMilli(e.TS).Format(r.timeFormat())
	used := len(r.timeFormat()) + 2 + 6 + 2 + r.whoW + 2 + r.threadW + 2
	budget := r.width - used
	if budget < 10 {
		budget = 10
	}
	_, _ = fmt.Fprintf(w, "%s  %-6s  %s  %s  %s\n",
		padDisplay(ts, len(r.timeFormat())), e.Kind, padDisplay(display.Sanitize(r.who(e), whoMaxWidth), r.whoW), padDisplay(r.thread(e), r.threadW), display.Sanitize(r.what(e), budget))
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
