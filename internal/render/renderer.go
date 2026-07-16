package render

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/schuettc/muster/internal/display"
)

// rawFieldWidth bounds display.Sanitize's width cap for row fields (subject,
// detail, who) before their final column truncation in Line() — large enough
// that it never bites ahead of the real budget, while still running every
// field through the one canonical sanitizer (control-char stripping +
// whitespace-run collapsing) rather than a bespoke oneLine/truncate pair.
const rawFieldWidth = 4096

const (
	whoMaxWidth      = 34  // WHO column cap (display columns)
	whoSenderWidth   = 15  // sender half of a 'from → to' pair (display columns)
	defaultLineWidth = 120 // line budget when $COLUMNS is unset; --width overrides
)

// Renderer holds the display state shared by events, watch, and the station
// feed pane: the alias→label map, column widths, and time format. Widths are
// seeded from the rows about to be printed (events) or the backlog
// (watch/station) and only grow, so columns stay aligned to real content
// instead of guessed fixed widths while the feed streams.
type Renderer struct {
	labels   map[string]string // alias → current label ("" or missing = show the alias)
	aliases  bool              // true = raw aliases, ignore labels
	fullTime bool              // true = date + time; false = time only
	whoW     int
	threadW  int
	width    int // total line budget (terminal columns)
}

// NewRenderer sizes columns from rows. labels may be nil (aliases render
// as-is). width <= 0 falls back to $COLUMNS, then defaultLineWidth.
func NewRenderer(rows []EventRow, labels map[string]string, aliases, fullTime bool, width int) *Renderer {
	r := &Renderer{labels: labels, aliases: aliases, fullTime: fullTime, whoW: 6, threadW: 3, width: width}
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

// HasLabel reports whether alias currently has a non-empty entry in the
// renderer's label map — used by streaming callers (watch, station's feed)
// to decide whether the map needs a refresh for a newly-seen agent.
func (r *Renderer) HasLabel(alias string) bool {
	return r.labels[alias] != ""
}

// SetLabels replaces the renderer's label map (e.g. after a streaming
// caller refreshes it for a newly-registered agent).
func (r *Renderer) SetLabels(labels map[string]string) {
	r.labels = labels
}

// fit grows column widths to accommodate e; it never shrinks them, keeping
// alignment stable across a streamed tail.
func (r *Renderer) fit(e EventRow) {
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
func (r *Renderer) disp(alias string) string {
	if !r.aliases {
		if l := r.labels[alias]; l != "" {
			return l
		}
	}
	return alias
}

// dispTarget renders a journal target ('agent:x' / 'role:r' / 'broadcast' /
// bare alias) for display.
func (r *Renderer) dispTarget(target string) string {
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
func (r *Renderer) who(e EventRow) string {
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
func (r *Renderer) thread(e EventRow) string {
	if e.ThreadID == 0 {
		return ""
	}
	return "#" + strconv.FormatInt(e.ThreadID, 10)
}

// intentTag maps a thread's effective intent to the journal suffix what()
// appends on send/task rows — "" (unspecified) renders no tag. The three
// non-empty values mirror internal/store's IntentFYI/IntentReply/IntentAction
// vocabulary (duplicated here deliberately: render is a peer client of the
// daemon over the wire, not a store-internal package).
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
func (r *Renderer) what(e EventRow) string {
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

func (r *Renderer) timeFormat() string {
	if r.fullTime {
		return "2006-01-02 15:04:05"
	}
	return "15:04:05"
}

// PadDisplay right-pads s with spaces to w DISPLAY COLUMNS (not rune count),
// matching the units display.Width and display.Sanitize already use. fmt's
// "%-*s" pads by rune count, which misaligns columns and blows the line
// budget once a field holds wide (CJK/fullwidth) runes — a rune-counted pad
// adds more spaces than the display actually needs, since each wide rune
// already occupies 2 columns worth of the target width. Every column whose
// content can legitimately hold non-ASCII (WHO, and — for symmetry — TIME/
// THREAD, which are ASCII in practice but should never silently rely on
// that) goes through this helper instead.
func PadDisplay(s string, w int) string {
	if n := w - display.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// Header writes the column header line shared by events, watch, and station.
func (r *Renderer) Header(w io.Writer) {
	_, _ = fmt.Fprintf(w, "%s  %-6s  %s  %s  %s\n",
		PadDisplay("TIME", len(r.timeFormat())), "KIND", PadDisplay("WHO", r.whoW), PadDisplay("THREAD", r.threadW), "WHAT")
}

// Line writes exactly one line for e — safe for line-by-line streaming (no
// long-lived tabwriter that only aligns at Flush). The WHAT column is capped
// to the remaining terminal width so a row can never wrap. Column padding is
// display-width based throughout (PadDisplay), matching the `used` budget
// math below, which sums display-width column widths — the two must stay in
// the same unit or the WHAT truncation cap drifts from the line's real
// rendered width.
func (r *Renderer) Line(w io.Writer, e EventRow) {
	r.fit(e)
	ts := time.UnixMilli(e.TS).Format(r.timeFormat())
	used := len(r.timeFormat()) + 2 + 6 + 2 + r.whoW + 2 + r.threadW + 2
	budget := r.width - used
	if budget < 10 {
		budget = 10
	}
	_, _ = fmt.Fprintf(w, "%s  %-6s  %s  %s  %s\n",
		PadDisplay(ts, len(r.timeFormat())), e.Kind, PadDisplay(display.Sanitize(r.who(e), whoMaxWidth), r.whoW), PadDisplay(r.thread(e), r.threadW), display.Sanitize(r.what(e), budget))
}
