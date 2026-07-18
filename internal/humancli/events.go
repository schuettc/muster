package humancli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/schuettc/muster/internal/render"
)

// eventRow and eventsPage are humancli's aliases for the shared render types
// (the renderer machinery moved to internal/render; humancli stays a thin
// consumer of it, exactly like the station TUI).
type eventRow = render.EventRow
type eventsPage = render.EventsPage

// callDataCaller adapts humancli's callData to render.Caller, so render
// never dials a socket itself — humancli wraps its own daemon-client
// plumbing (callData, itself a thin wrapper over internal/client.Call).
type callDataCaller struct{}

func (callDataCaller) Call(op string, args map[string]any) (json.RawMessage, error) {
	return callData(op, args)
}

// fetchEvents calls list_events with the given filters. afterID < 0 selects
// backlog mode; afterID >= 0 selects follow mode. See render.FetchEvents.
func fetchEvents(agent, kind string, threadID, afterID int64, limit int) (eventsPage, error) {
	return render.FetchEvents(callDataCaller{}, agent, kind, threadID, afterID, limit)
}

// newRenderer sizes columns from rows. See render.NewRenderer.
func newRenderer(rows []eventRow, labels map[string]string, aliases, fullTime bool, width int) *render.Renderer {
	return render.NewRenderer(rows, labels, aliases, fullTime, width)
}

// loadLabels fetches the current alias→label map, best-effort. See
// render.LoadLabels.
func loadLabels() map[string]string {
	return render.LoadLabels(callDataCaller{})
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
	r.Header(out)
	// The daemon returns newest first; print oldest first so the log reads
	// top to bottom like a timeline.
	for i := len(page.Events) - 1; i >= 0; i-- {
		r.Line(out, page.Events[i])
	}
	return nil
}
