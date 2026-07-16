package humancli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// watchOpts carries the loop's injectable seams. Zero value = production:
// wait sleeps interval or returns early on signal; maxPolls 0 = forever.
type watchOpts struct {
	wait     func(d time.Duration) bool // false = shutdown requested
	maxPolls int
	errw     io.Writer // stderr for retry/reset notes; nil = os.Stderr
}

// cmdWatch tails the bus journal: prints the last --backlog matching events,
// then polls list_events with the max_id cursor every --interval until
// interrupted. Side-effect-free: never marks anything read.
func cmdWatch(args []string, out io.Writer, o watchOpts) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agent := fs.String("agent", "", "only events concerning this alias")
	kind := fs.String("kind", "", "only this event kind")
	thread := fs.Int64("thread", 0, "only this thread")
	interval := fs.Duration("interval", time.Second, "poll interval")
	backlog := fs.Int("backlog", 10, "history lines to print before following (0 = none)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: muster watch [--agent <alias>] [--thread <id>] [--kind <k>] [--interval <dur>] [--backlog <n>]")
	}
	if o.errw == nil {
		o.errw = os.Stderr
	}
	if o.wait == nil {
		o.wait = signalWait()
	}

	page, err := fetchEvents(*agent, *kind, *thread, -1, *backlog) // backlog mode
	if err != nil {
		return err
	}
	eventHeader(out)
	for i := len(page.Events) - 1; i >= 0; i-- { // newest-first → print oldest-first
		printEventLine(out, page.Events[i])
	}
	cursor := page.MaxID

	for polls := 0; o.maxPolls == 0 || polls < o.maxPolls; polls++ {
		if !o.wait(*interval) {
			return nil // interrupted
		}
		page, err := fetchEvents(*agent, *kind, *thread, cursor, 0) // follow mode
		if err != nil {
			_, _ = fmt.Fprintln(o.errw, "watch: poll failed, retrying:", err)
			continue
		}
		if page.MaxID < cursor {
			_, _ = fmt.Fprintf(o.errw, "watch: journal reset (max id %d < cursor %d) — DB replaced? following from the new tail\n", page.MaxID, cursor)
			cursor = page.MaxID
			continue
		}
		for _, e := range page.Events { // follow mode is oldest-first
			printEventLine(out, e)
		}
		cursor = page.MaxID
	}
	return nil
}

// signalWait returns the production wait: sleep d, but return false
// immediately on SIGINT/SIGTERM so Ctrl-C never waits out an interval.
func signalWait() func(time.Duration) bool {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	return func(d time.Duration) bool {
		select {
		case <-sig:
			return false
		case <-time.After(d):
			return true
		}
	}
}
