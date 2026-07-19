package humancli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// threadDetail decodes the get_thread op's response: the thread header plus
// its entries, oldest first. total is the live entry count before any
// pagination (unused here — cmdThread always prints the whole thread).
type threadDetail struct {
	Thread struct {
		ID        int64  `json:"id"`
		Kind      string `json:"kind"`
		FromAgent string `json:"from_agent"`
		ToKind    string `json:"to_kind"`
		ToTarget  string `json:"to_target"`
		Subject   string `json:"subject"`
		Status    string `json:"status"`
		Intent    string `json:"intent"`
	} `json:"thread"`
	Entries []struct {
		FromAgent    string `json:"from_agent"`
		Body         string `json:"body"`
		StatusChange string `json:"status_change"`
		CreatedAt    int64  `json:"created_at"`
	} `json:"entries"`
}

// cmdThread prints one thread in full: the header line, then every entry
// oldest-first with its author, timestamp, and verbatim body. This is the
// CLI half of the MCP get_thread tool — the read step of the
// inbox → thread → reply loop, usable when an agent has a shell but no
// muster MCP connection. Side-effect-free: printing a thread never marks
// anything read ('muster inbox' owns the read watermark, exactly like the
// get_inbox tool).
func cmdThread(args []string, out io.Writer) error {
	if helpRequested(args) {
		return HelpFor("thread", out)
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: muster thread <id>")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("thread id must be a number, got %q", args[0])
	}
	raw, err := callData("get_thread", map[string]any{"thread_id": id})
	if err != nil {
		return err
	}
	var d threadDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	th := d.Thread
	if th.ID == 0 && len(d.Entries) == 0 {
		return fmt.Errorf("no thread %d", id)
	}
	to := th.ToKind
	if th.ToTarget != "" {
		to = th.ToKind + ":" + th.ToTarget
	}
	header := fmt.Sprintf("thread %d · %s · %s → %s", th.ID, th.Kind, th.FromAgent, to)
	if th.Status != "" {
		header += " · status " + th.Status
	}
	if th.Intent != "" {
		header += " · intent " + th.Intent
	}
	if _, err := fmt.Fprintln(out, header); err != nil {
		return err
	}
	if th.Subject != "" {
		if _, err := fmt.Fprintf(out, "subject: %s\n", th.Subject); err != nil {
			return err
		}
	}
	for _, e := range d.Entries {
		stamp := time.UnixMilli(e.CreatedAt).Format("2006-01-02 15:04")
		line := fmt.Sprintf("\n[%s] %s", stamp, e.FromAgent)
		if e.StatusChange != "" {
			line += " · → " + e.StatusChange
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
		for _, bl := range strings.Split(e.Body, "\n") {
			if _, err := fmt.Fprintf(out, "  %s\n", bl); err != nil {
				return err
			}
		}
	}
	return nil
}

// newReplyFlagsWithVals declares reply's flags and returns typed access to
// their values — shared by cmdReply (real parsing) and newReplyFlags
// (registry help/man rendering).
func newReplyFlagsWithVals() (fs *flag.FlagSet, from *string) {
	fs = flag.NewFlagSet("reply", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from = fs.String("from", "human", "replying agent alias")
	return fs, from
}

// newReplyFlags builds reply's flag.FlagSet for registry-driven help/man
// rendering.
func newReplyFlags() *flag.FlagSet {
	fs, _ := newReplyFlagsWithVals()
	return fs
}

// cmdReply appends an entry to an existing thread — the CLI half of the MCP
// reply tool, and the write step of the inbox → thread → reply loop for an
// agent (or the operator) working without a muster MCP connection.
// Recipients' mailboxes are flagged by the daemon exactly as for a
// tool-sent reply.
func cmdReply(args []string, out io.Writer) error {
	fs, from := newReplyFlagsWithVals()
	// reply has no boolean flags: --from takes a value, so pass an explicit
	// empty bool-flags set (the implicit default would wrongly reuse send's
	// boolean --role).
	flagArgs, rest := splitFlagsAndPositional(args, map[string]bool{})
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpFor("reply", out)
		}
		return err
	}
	if len(rest) < 2 {
		return fmt.Errorf(`usage: muster reply <thread-id> "body" [--from <alias>]`)
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return fmt.Errorf("thread id must be a number, got %q", rest[0])
	}
	body := strings.Join(rest[1:], " ")
	raw, err := callData("reply", map[string]any{"thread_id": id, "from": *from, "body": body})
	if err != nil {
		return err
	}
	var res struct {
		EntryID int64 `json:"entry_id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "replied to thread %d (entry %d)\n", id, res.EntryID)
	return err
}
