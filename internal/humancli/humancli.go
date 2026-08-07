// Package humancli implements muster's operator subcommands (agents, inbox,
// send, tasks) that read/drive the bus from a plain shell.
package humancli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/device"
	"github.com/schuettc/muster/internal/nudge"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/tmuxenv"
	"github.com/schuettc/muster/internal/version"
)

// nudgeRun lets tests intercept the tmux command executor for nudges.
var nudgeRun func(args ...string) error

// agentFull decodes the daemon's get_agent response.
type agentFull struct {
	Alias       string `json:"alias"`
	ModelType   string `json:"model_type"`
	SocketPath  string `json:"socket_path"`
	PaneID      string `json:"pane_id"`
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name"`
	// SessionCreated mirrors store.Agent.SessionCreated — cmdNudge uses it
	// (with tmuxenv.IsSessionAlive) to tell a merely-stale pane row (session
	// still alive, pane reaped underneath it) from a fully dead one, so it
	// can refuse the dead-pane case with a remedy instead of a doomed
	// send-keys.
	SessionCreated int64 `json:"session_created"`
	// HarnessSessionID mirrors agentRow's own copy (see store.Agent.HarnessSessionID)
	// — stampHarnessLinks reads it via hookGetAgent to skip a row that already
	// has a link.
	HarnessSessionID string `json:"harness_session_id"`
	// Departed mirrors store.Agent.Departed (see agentRow's own copy above):
	// get_agent returns tombstoned rows with found=true, so hook gates must
	// decode this to tell a live owner from a dead one.
	Departed bool `json:"departed"`
	// Label mirrors agentRow's own copy — the resume reclaim test asserts a
	// manually-pinned label survives the reclaim onto the new tuple.
	Label string `json:"label"`
	// SupersededBy mirrors store.Agent.SupersededBy — hookSessionStartResume
	// reads it via hookGetAgent/harnessOwnedRows to tell a become-retired
	// seed (never reclaim) from an ordinary tombstone (reclaim as before).
	SupersededBy string `json:"superseded_by"`
}

type agentRow struct {
	Alias      string `json:"alias"`
	Role       string `json:"role"`
	ModelType  string `json:"model_type"`
	SocketPath string `json:"socket_path"`
	// PaneID feeds hookSessionEnd's per-alias ownership check (a sibling
	// pane's registration is not the dying pane's to tombstone).
	PaneID    string `json:"pane_id"`
	SessionID string `json:"session_id"`
	// SessionCreated feeds tmuxenv.IsSessionAlive's recycled-session-ID
	// discrimination (see store.Agent.SessionCreated).
	SessionCreated int64 `json:"session_created"`
	// HarnessSessionID links a row to its harness session (see
	// store.Agent.HarnessSessionID) — how a daemon-hosted session's hooks,
	// which see no tmux, find their own rows.
	HarnessSessionID string `json:"harness_session_id"`
	SessionName      string `json:"session_name"`
	// DeviceID names the MACHINE this row was registered from (see
	// store.Agent.DeviceID). It is location, never identity: nothing
	// addressable is scoped by it, and internal/resolve takes no device
	// argument — an alias means the same agent from every device on the bus.
	// cmdAgents renders it purely so an operator can answer "which box is
	// that on", a question a bus spanning machines makes askable and
	// nothing else in the CLI could answer.
	DeviceID    string `json:"device_id"`
	Project     string `json:"project"`
	Label       string `json:"label"`
	LabelManual bool   `json:"label_manual"`
	LastSeen    int64  `json:"last_seen"`
	// Departed is true once the agent has been deregistered (tombstoned, not
	// deleted — see store.Store.DepartAgent): gc's default reap and
	// --purge-agents both key off this to decide whether a row still needs
	// reaping or is already history.
	Departed bool `json:"departed"`
	// SupersededBy mirrors store.Agent.SupersededBy — non-empty on a row
	// retired via `become`, naming the alias that now carries its identity
	// forward. hookSessionStartResume uses this as ground truth: a departed
	// row with SupersededBy set must never reclaim on resume, regardless of
	// whether its successor is currently live.
	SupersededBy string `json:"superseded_by"`
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
// It also owns muster's help/version surface (`help`, `-h`, `--help`,
// `version`, `--version`) — cmd/muster's main() routes anything that isn't
// serve/mcp/debug here, and those three special-case help themselves before
// ever reaching Dispatch (see cmd/muster/main.go), so this is the one place
// that needs to recognize them.
func Dispatch(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: muster <command> [args] (see 'muster help')")
	}
	switch args[0] {
	case "help":
		return dispatchHelp(args[1:], out)
	case "-h", "--help":
		Usage(out)
		return nil
	case "version", "--version":
		_, err := fmt.Fprintln(out, version.Line())
		return err
	}
	cmd, ok := lookup(args[0])
	if !ok || cmd.Run == nil {
		return usageErrorf("unknown command %q (see 'muster help')", args[0])
	}
	return cmd.Run(args[1:], out)
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
	// The device column appears only when the roster actually SPANS devices.
	// On a local bus every row carries this machine's id, so a column reading
	// "this" all the way down is pure noise — and local is the default and the
	// overwhelmingly common case. Rendering it conditionally means the column
	// shows up exactly when it answers a question the operator can now ask.
	localDevice := device.Existing()
	devices := make(map[string]struct{}, 2)
	for _, a := range agents {
		if a.DeviceID != "" {
			devices[a.DeviceID] = struct{}{}
		}
	}
	showDevice := len(devices) > 1

	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	header := "PROJECT\tALIAS\tLABEL\tMODEL\tLIVE"
	if showDevice {
		header = "PROJECT\tALIAS\tLABEL\tMODEL\tDEVICE\tLIVE"
	}
	if _, err := fmt.Fprintln(tw, header); err != nil {
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
		switch {
		case !a.Departed && localDevice != "" && a.DeviceID != "" && a.DeviceID != localDevice:
			// ANOTHER machine's agent. This case must precede a.Live, because
			// a.Live is a LOCAL tmux probe and running it against a remote
			// row is not merely uninformative but unsound: socket paths are
			// per-machine strings that collide freely across machines
			// (/private/tmp/tmux-501/proj-foo exists on every box that has
			// that project), so probing one here can match an unrelated local
			// session and report ITS liveness as the remote agent's. There is
			// no way to answer the question from this device, so say so.
			live = "◌"
		case a.Live:
			live = "●"
		case a.SocketPath == "" && !a.Departed:
			// Paneless agents (harness daemon-hosted sessions) have no tmux
			// session to probe: liveness is unknowable here, and rendering ✗
			// would read as "dead" for what is usually a live session.
			live = "◌"
		}
		var err error
		if showDevice {
			_, err = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				proj, a.Alias, label, a.ModelType, deviceCell(a.DeviceID, localDevice), live)
		} else {
			_, err = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", proj, a.Alias, label, a.ModelType, live)
		}
		if err != nil {
			return err
		}
	}
	return tw.Flush()
}

// deviceShortLen is how much of a device id identifies it in the roster.
// Device ids are UUIDs, so the first 8 hex characters distinguish any
// plausible number of machines while staying narrow enough to sit in a table.
const deviceShortLen = 8

// deviceCell renders one agent's device for the roster: "this" for the
// machine the command is running on, a short id for any other, and "—" for a
// row that predates device ids or was written by a backend that does not set
// them. Naming the local device rather than showing its id is the point —
// "is that agent here or somewhere else" is the question being asked, and an
// operator cannot answer it by comparing two hex strings at a glance.
func deviceCell(id, local string) string {
	switch {
	case id == "":
		return "—"
	case local != "" && id == local:
		return "this"
	case len(id) > deviceShortLen:
		return id[:deviceShortLen]
	default:
		return id
	}
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

// sendFlagVals holds cmdSend's parsed flag pointers.
type sendFlagVals struct {
	from, subject, ref, intent, project *string
	role, broadcast                     *bool
}

// newSendFlagsWithVals declares send's flags and returns both the FlagSet
// and typed access to its values — the one declaration cmdSend (real
// parsing) and newSendFlags (registry help/man rendering) both build on, so
// the two can't drift apart.
func newSendFlagsWithVals() (*flag.FlagSet, sendFlagVals) {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var v sendFlagVals
	v.from = fs.String("from", "human", "sending agent alias")
	v.subject = fs.String("subject", "", "message subject")
	v.ref = fs.String("ref", "", "pointer to the work")
	v.role = fs.Bool("role", false, "treat target as a role")
	v.broadcast = fs.Bool("broadcast", false, "send to everyone")
	v.project = fs.String("project", "", "with --broadcast: send only to this project's agents")
	v.intent = fs.String("intent", "", "message intent: fyi, reply-requested, or action-requested")
	return fs, v
}

// newSendFlags builds send's flag.FlagSet for registry-driven help/man
// rendering (Command.NewFlags); its values are unused, only the declared
// flags (names, defaults, usage strings) matter here.
func newSendFlags() *flag.FlagSet {
	fs, _ := newSendFlagsWithVals()
	return fs
}

// cmdSend sends a message to an agent, role, or broadcast target and prints
// the resulting thread ID.
func cmdSend(args []string, out io.Writer) error {
	fs, v := newSendFlagsWithVals()
	flagArgs, rest := splitFlagsAndPositional(args)
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpFor("send", out)
		}
		return err
	}
	if err := validateIntent(*v.intent); err != nil {
		return err
	}
	if *v.project != "" && !*v.broadcast {
		return fmt.Errorf("--project requires --broadcast")
	}
	toKind, toTarget := "agent", ""
	switch {
	case *v.broadcast:
		toKind = "broadcast"
	case *v.role:
		toKind = "role"
	}
	var body string
	if *v.broadcast {
		if len(rest) < 1 {
			return fmt.Errorf("usage: muster send --broadcast [--project <p>] <body> [--intent fyi|reply-requested|action-requested]")
		}
		toTarget = *v.project
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
		"from": *v.from, "to_kind": toKind, "to_target": toTarget,
		"subject": *v.subject, "ref": *v.ref, "body": body, "intent": *v.intent,
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
		// "h"/"help" are always boolean, regardless of the caller's bf map:
		// the flag package itself never consumes a value for them (an
		// unrecognized -h/-help short-circuits to flag.ErrHelp before value
		// consumption is even considered), so treating them as a value flag
		// here would wrongly swallow the next token — e.g. `send -h alice
		// hello` must NOT bind "alice" to -h as its value.
		if hasValue || bf[name] || name == "h" || name == "help" {
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
	if helpRequested(args) {
		return HelpFor("inbox", out)
	}
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
	if helpRequested(args) {
		return HelpFor("tasks", out)
	}
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

// newNudgeFlagsWithVals declares nudge's flags and returns typed access to
// their values, mirroring newSendFlagsWithVals.
func newNudgeFlagsWithVals() (fs *flag.FlagSet, noSubmit *bool) {
	fs = flag.NewFlagSet("nudge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	noSubmit = fs.Bool("no-submit", false, "type the nudge but do not press Enter")
	return fs, noSubmit
}

// newNudgeFlags builds nudge's flag.FlagSet for registry-driven help/man
// rendering.
func newNudgeFlags() *flag.FlagSet {
	fs, _ := newNudgeFlagsWithVals()
	return fs
}

// cmdNudge resolves alias to its registered tmux pane and types the
// check-inbox line into it, auto-submitting when the model type accepts it.
func cmdNudge(args []string, out io.Writer) error {
	fs, noSubmit := newNudgeFlagsWithVals()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpFor("nudge", out)
		}
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
	// A stored pane can go stale between registration and nudge — most
	// commonly a reaped teammate's pane, closed out from under a row that
	// still names it. Refuse rather than guess: with teammate panes sharing
	// the session, typing into whatever pane happens to be there next is
	// worse than failing loudly (spec §3, no auto-retargeting). The row
	// heals itself the next time that session starts/resumes and
	// re-registers, or the operator can re-register from the live pane now.
	if ag.SocketPath != "" && !tmuxenv.IsPaneAlive(ag.SocketPath, ag.PaneID) &&
		tmuxenv.IsSessionAlive(ag.SocketPath, ag.SessionID, ag.SessionCreated) {
		return fmt.Errorf("nudge %s: stored pane %s is gone but its session is alive — the row heals at the session's next start/resume (or re-register from the live pane); refusing to type into a guessed pane", ag.Alias, ag.PaneID)
	}
	// session_name is mutable — tmux lets an operator rename a session at any
	// time — so the stored (registration-time) snapshot goes stale the
	// moment that happens. Query the LIVE name at nudge time; fall back to
	// the stored snapshot, then the alias, only if the live query comes back
	// empty (tmux unreachable, or the session no longer exists).
	sessionName := tmuxenv.SessionName(ag.SocketPath, ag.SessionID)
	if sessionName == "" {
		sessionName = ag.SessionName
	}
	if sessionName == "" {
		sessionName = ag.Alias
	}
	if _, err := fmt.Fprintf(out, "nudging %s → session %s / pane %s on %s\n", ag.Alias, sessionName, ag.PaneID, ag.SocketPath); err != nil {
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
