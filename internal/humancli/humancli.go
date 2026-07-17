// Package humancli implements muster's operator subcommands (agents, inbox,
// send, tasks) that read/drive the bus from a plain shell.
package humancli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/nudge"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/station"
)

// nudgeRun lets tests intercept the tmux command executor for nudges.
var nudgeRun func(args ...string) error

// agentFull decodes the daemon's get_agent response.
type agentFull struct {
	Alias       string `json:"alias"`
	ModelType   string `json:"model_type"`
	SocketPath  string `json:"socket_path"`
	PaneID      string `json:"pane_id"`
	SessionName string `json:"session_name"`
}

type agentRow struct {
	Alias       string `json:"alias"`
	Role        string `json:"role"`
	ModelType   string `json:"model_type"`
	SocketPath  string `json:"socket_path"`
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name"`
	Project     string `json:"project"`
	Label       string `json:"label"`
	LabelManual bool   `json:"label_manual"`
	LastSeen    int64  `json:"last_seen"`
}

// threadRow decodes daemon thread responses (get_inbox, get_thread, list_tasks).
// LastFrom and Unread are query-time annotations get_inbox populates (see
// store.Thread) — zero-valued on surfaces that don't compute them.
type threadRow struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	FromAgent string `json:"from_agent"`
	ToKind    string `json:"to_kind"`
	ToTarget  string `json:"to_target"`
	Subject   string `json:"subject"`
	Status    string `json:"status"`
	LastFrom  string `json:"last_from"`
	Unread    int    `json:"unread"`
}

// callData sends one op to the daemon and returns its Data as JSON, or an error
// if the transport failed or the daemon reported !OK.
func callData(op string, args map[string]any) (json.RawMessage, error) {
	resp, err := client.Call(paths.SocketPath(), proto.Request{Op: op, Args: args})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s: %s", op, resp.Error)
	}
	b, err := json.Marshal(resp.Data)
	if err != nil {
		return nil, fmt.Errorf("marshal %s result: %w", op, err)
	}
	return b, nil
}

// Dispatch routes an operator subcommand. args[0] is the subcommand name.
func Dispatch(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: muster <agents|inbox|send|tasks|events|watch|station|nudge|register|deregister|gc|hook|label> [args]")
	}
	switch args[0] {
	case "agents":
		return cmdAgents(out)
	case "station":
		return station.Run(args[1:])
	case "send":
		return cmdSend(args[1:], out)
	case "inbox":
		return cmdInbox(args[1:], out)
	case "tasks":
		return cmdTasks(args[1:], out)
	case "events":
		return cmdEvents(args[1:], out)
	case "watch":
		return cmdWatch(args[1:], out, watchOpts{})
	case "nudge":
		return cmdNudge(args[1:], out)
	case "register":
		return cmdRegister(args[1:], out)
	case "deregister":
		return cmdDeregister(args[1:], out)
	case "gc":
		return cmdGC(args[1:], out)
	case "hook":
		return cmdHook(args[1:], os.Stdin, out)
	case "label":
		return cmdLabel(args[1:], out)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// cmdAgents lists registered agents grouped by project, showing each
// agent's addressable label and live tmux session status.
func cmdAgents(out io.Writer) error {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return err
	}
	var rows []agentRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return err
	}
	agents := enrichAgents(rows)
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].Project != agents[j].Project {
			return agents[i].Project < agents[j].Project
		}
		return agents[i].Alias < agents[j].Alias
	})
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "PROJECT\tALIAS\tLABEL\tMODEL\tLIVE"); err != nil {
		return err
	}
	for _, a := range agents {
		proj := a.Project
		if proj == "" {
			proj = "(none)"
		}
		label := a.EffLabel
		switch {
		case label == "":
			label = "—"
		case !a.EffManual:
			label = "(" + label + ")" // auto-topic: shown but not addressable
		}
		live := "✗"
		if a.Live {
			live = "●"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", proj, a.Alias, label, a.ModelType, live); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// validIntents is the client-side copy of the intent vocabulary store.CreateThread
// enforces (internal/store/threads.go's validIntent) — "" (unspecified) plus
// the three named intents. Duplicated here deliberately: humancli is a peer
// client of the daemon over the wire, not a store-internal package, so it
// checks against the same three literal strings rather than importing
// internal/store. The store re-validates regardless; this only buys a
// clearer client-side error than a daemon round-trip.
var validIntents = map[string]bool{"": true, "fyi": true, "reply-requested": true, "action-requested": true}

// validateIntent returns a clear error for an intent value the store would
// otherwise reject after a round-trip.
func validateIntent(intent string) error {
	if !validIntents[intent] {
		return fmt.Errorf("invalid --intent %q: must be fyi, reply-requested, or action-requested", intent)
	}
	return nil
}

// cmdSend sends a message to an agent, role, or broadcast target and prints
// the resulting thread ID.
func cmdSend(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "human", "sending agent alias")
	subject := fs.String("subject", "", "message subject")
	ref := fs.String("ref", "", "pointer to the work")
	role := fs.Bool("role", false, "treat target as a role")
	broadcast := fs.Bool("broadcast", false, "send to everyone")
	intent := fs.String("intent", "", "message intent: fyi, reply-requested, or action-requested")
	flagArgs, rest := splitFlagsAndPositional(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if err := validateIntent(*intent); err != nil {
		return err
	}
	toKind, toTarget := "agent", ""
	switch {
	case *broadcast:
		toKind = "broadcast"
	case *role:
		toKind = "role"
	}
	var body string
	if *broadcast {
		if len(rest) < 1 {
			return fmt.Errorf("usage: muster send --broadcast <body> [--intent fyi|reply-requested|action-requested]")
		}
		body = strings.Join(rest, " ")
	} else {
		if len(rest) < 2 {
			return fmt.Errorf("usage: muster send <alias|label|proj:label> <body> [--from X --subject S --ref R --role --broadcast --intent fyi|reply-requested|action-requested]")
		}
		toTarget = rest[0]
		if toKind == "agent" {
			resolved, err := resolveVia(rest[0])
			if err != nil {
				return err
			}
			toTarget = resolved
		}
		body = strings.Join(rest[1:], " ")
	}
	raw, err := callData("send_message", map[string]any{
		"from": *from, "to_kind": toKind, "to_target": toTarget,
		"subject": *subject, "ref": *ref, "body": body, "intent": *intent,
	})
	if err != nil {
		return err
	}
	var res struct {
		ThreadID int64 `json:"thread_id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "sent (thread %d)\n", res.ThreadID)
	return err
}

// sendBoolFlags are cmdSend's flags that take no value, needed so
// splitFlagsAndPositional knows not to consume the following token as a value.
var sendBoolFlags = map[string]bool{"role": true, "broadcast": true}

// splitFlagsAndPositional separates args into flag.FlagSet-parseable tokens
// and positional arguments, regardless of whether flags appear before or
// after the positionals — Go's flag.Parse otherwise stops at the first
// non-flag token, which breaks `send <target> <body> [--from X ...]`.
//
// boolFlags names the calling command's no-value flags (so the following
// token isn't mistaken for that flag's value); when omitted it defaults to
// sendBoolFlags for backward compatibility with cmdSend's original call
// site. Commands whose flags are all value flags (e.g. register's
// --role/--model) must pass an explicit empty set rather than rely on that
// default, since flag names collide across commands (send's --role is
// boolean; register's --role takes a value).
//
// A value flag is registered identically regardless of which value flag it
// is (--from, --subject, --ref, --intent, …): anything not named in bf is
// assumed to take a value, so a NEW value flag (like --intent) never needs
// its own entry here to be recognized — it falls out of the same "not a bool
// flag" branch --from/--subject already use. The one gap this closes: if a
// PRECEDING value flag is itself missing its value (a caller bug, or a
// dangling flag at odds with what a human intended), the naive "always
// consume the next token" rule used to swallow the FOLLOWING flag as that
// flag's bogus value — e.g. `--subject --intent action-requested` bound
// "--intent" to --subject and left "action-requested" as stray text
// flag.Parse itself then silently discards (Go's flag.Parse stops at the
// first token that isn't a recognized flag and never surfaces it), so
// --intent was never parsed and silently stored empty — with no error at
// all. Go's flag.Parse ALWAYS consumes the very next flagArgs entry as a
// non-boolean flag's value, regardless of what that entry looks like, so
// merely leaving the dangling flag and the next flag as adjacent SEPARATE
// entries (as an earlier version of this fix did) doesn't stop the
// misbinding — flag.Parse does its own greedy pairing independent of how
// this function grouped them. The actual fix has to make the dangling flag
// visibly complete-with-no-value BEFORE flag.Parse ever sees it: when the
// next token itself looks like a flag, the dangling flag is rewritten to its
// explicit `name=` form (an unambiguous empty value), so flag.Parse consumes
// nothing further from it and the following token is left untouched for its
// own turn through this same loop. A flag dangling at the very end of args
// (no next token at all) is left bare, unchanged from before — flag.Parse's
// own "flag needs an argument" error is still the right outcome there, since
// there's no following flag it could otherwise swallow.
func splitFlagsAndPositional(args []string, boolFlags ...map[string]bool) (flagArgs, positional []string) {
	bf := sendBoolFlags
	if len(boolFlags) > 0 {
		bf = boolFlags[0]
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		idx := strings.Index(name, "=")
		hasValue := idx >= 0
		if hasValue {
			name = name[:idx]
		}
		if hasValue || bf[name] {
			flagArgs = append(flagArgs, a)
			continue
		}
		// a is a value flag with no explicit "=value" of its own.
		switch {
		case i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
			flagArgs = append(flagArgs, a, args[i+1])
			i++
		case i+1 < len(args):
			// Dangling, immediately followed by another flag: force an
			// explicit empty value so flag.Parse doesn't reach past this
			// flag and swallow the next one as its bogus value.
			flagArgs = append(flagArgs, a+"=")
		default:
			// Dangling at the very end: unchanged, bare — flag.Parse's own
			// "flag needs an argument" error is exactly right here.
			flagArgs = append(flagArgs, a)
		}
	}
	return flagArgs, positional
}

// cmdInbox prints the given alias's threads.
func cmdInbox(args []string, out io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: muster inbox <alias|label|proj:label>")
	}
	alias, err := resolveVia(args[0])
	if err != nil {
		return err
	}
	return printThreads(out, alias, false)
}

// cmdTasks prints the given alias's inbox filtered to kind=task threads.
func cmdTasks(args []string, out io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: muster tasks <alias|label|proj:label>")
	}
	alias, err := resolveVia(args[0])
	if err != nil {
		return err
	}
	return printThreads(out, alias, true)
}

// printThreads fetches an alias's inbox and prints it; if tasksOnly, only
// kind=task threads are shown.
func printThreads(out io.Writer, alias string, tasksOnly bool) error {
	raw, err := callData("get_inbox", map[string]any{"alias": alias})
	if err != nil {
		return err
	}
	var threads []threadRow
	if err := json.Unmarshal(raw, &threads); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tKIND\tFROM\tTO\tSTATUS\tLAST-FROM\tUNREAD\tSUBJECT"); err != nil {
		return err
	}
	for _, th := range threads {
		if tasksOnly && th.Kind != "task" {
			continue
		}
		to := th.ToKind
		if th.ToTarget != "" {
			to = th.ToKind + ":" + th.ToTarget
		}
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", th.ID, th.Kind, th.FromAgent, to, th.Status, th.LastFrom, th.Unread, th.Subject); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// cmdNudge resolves alias to its registered tmux pane and types the
// check-inbox line into it, auto-submitting when the model type accepts it.
func cmdNudge(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("nudge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	noSubmit := fs.Bool("no-submit", false, "type the nudge but do not press Enter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("usage: muster nudge <alias|label|proj:label> [--no-submit]")
	}
	alias, err := resolveVia(rest[0])
	if err != nil {
		return err
	}
	raw, err := callData("get_agent", map[string]any{"alias": alias})
	if err != nil {
		return err
	}
	var res struct {
		Found bool      `json:"found"`
		Agent agentFull `json:"agent"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	if !res.Found {
		return fmt.Errorf("no agent registered as %q", alias)
	}
	ag := res.Agent
	if _, err := fmt.Fprintf(out, "nudging %s → session %s / pane %s on %s\n", ag.Alias, ag.SessionName, ag.PaneID, ag.SocketPath); err != nil {
		return err
	}
	n := nudge.TmuxNudger{Run: nudgeRun} // nil in prod → real tmux
	submitted, err := n.Nudge(ag.SocketPath, ag.PaneID, ag.ModelType, !*noSubmit)
	if err != nil {
		return err
	}
	detailWord := "typed"
	if submitted {
		detailWord = "submitted"
	}
	_, _ = callData("log_event", map[string]any{"target": alias, "detail": detailWord}) // best-effort journal
	if submitted {
		_, err = fmt.Fprintln(out, "delivered + submitted.")
	} else {
		_, err = fmt.Fprintf(out, "delivered (not auto-submitted for %s — press Enter in that pane).\n", ag.ModelType)
	}
	return err
}
