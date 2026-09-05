// Package cli implements muster's operator subcommands (agents, inbox,
// send, tasks) that read/drive the bus from a plain shell.
package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/schuettc/muster/internal/client"
	"github.com/schuettc/muster/internal/device"
	"github.com/schuettc/muster/internal/harnessenv"
	"github.com/schuettc/muster/internal/nudge"
	"github.com/schuettc/muster/internal/nudgeguard"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
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
	SessionCreated int64  `json:"session_created"`
	DeviceID       string `json:"device_id"`
	DeviceName     string `json:"device_name"`
	// HarnessSessionID mirrors agentRow's own copy (see store.Agent.HarnessSessionID)
	// — stampHarnessLinks reads it via hookGetAgent to skip a row that already
	// has a link.
	HarnessSessionID string `json:"harness_session_id"`
	// TranscriptPath mirrors agentRow's own copy (see store.Agent.TranscriptPath)
	// — stampHarnessLinks reads it via hookGetAgent to decide whether a row's
	// transcript link is already current.
	TranscriptPath string `json:"transcript_path"`
	// Departed mirrors store.Agent.Departed (see agentRow's own copy above):
	// get_agent returns tombstoned rows with found=true, so hook gates must
	// decode this to tell a live owner from a dead one.
	Departed bool `json:"departed"`
	// Label mirrors agentRow's own copy — the resume reclaim test asserts a
	// manually-pinned label survives the reclaim onto the new tuple.
	Label string `json:"label"`
	// SupersededBy mirrors store.Agent.SupersededBy — hookSessionStartResume
	// reads it via hookGetAgent/conversationRows to tell a become-retired
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
	// TranscriptPath links a row to the harness conversation's transcript
	// file (see store.Agent.TranscriptPath) — the identity anchor that
	// survives a resume even when the harness mints a NEW harness session id
	// each time (unlike HarnessSessionID, which does not). conversationRows
	// matches on it first.
	TranscriptPath string `json:"transcript_path"`
	SessionName    string `json:"session_name"`
	// DeviceID names the MACHINE this row was registered from (see
	// store.Agent.DeviceID). It is location, never identity: nothing
	// addressable is scoped by it, and internal/resolve takes no device
	// argument. What device DOES affect is presentation. The STORED alias is
	// globally unique and carries this machine's name; the DISPLAYED alias
	// drops that prefix on the machine that minted it, and a short name typed
	// there expands back before resolution. So an alias still means the same
	// agent from every device — it is just written two ways, short at home
	// and in full abroad. cmdAgents renders DeviceID purely so an operator can
	// answer "which box is that on", a question a bus spanning machines makes
	// askable and nothing else in the CLI could answer.
	DeviceID string `json:"device_id"`
	// DeviceName is that machine's operator-chosen name, rendered in the
	// DEVICE column because "work-laptop" is what someone would say and
	// "cd59b4cb" is not. Display only; scoping keys off DeviceID.
	DeviceName  string `json:"device_name"`
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

// decodeInboxResponse decodes a get_inbox payload, tolerating BOTH shapes
// (Finding 2): a 0.13.0+ daemon returns {threads, marked_read}, but a client
// upgraded ahead of a still-running pre-0.13.0 daemon (or a device forwarding
// to a not-yet-redeployed hosted lambda) still gets the old bare-array shape.
// Decoding that as marked_read=true matches the old daemon's actual
// behavior — every read moved the watermark — so this is not a guess, it's
// what happened. Deliberately no version handshake: this is decode-only.
func decodeInboxResponse(raw []byte) ([]threadRow, bool, error) {
	var body struct {
		Threads    []threadRow `json:"threads"`
		MarkedRead bool        `json:"marked_read"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		return body.Threads, body.MarkedRead, nil
	}
	var legacy []threadRow
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil, false, err
	}
	return legacy, true, nil
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

	all := make([]string, 0, len(agents))
	for _, a := range agents {
		all = append(all, a.Alias)
	}
	disp := aliasDisplay(all)

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
				proj, disp[a.Alias], label, a.ModelType, deviceCell(a.DeviceID, a.DeviceName, localDevice), live)
		} else {
			_, err = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", proj, disp[a.Alias], label, a.ModelType, live)
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

// deviceCell renders one agent's device for the roster. The precedence is
// "this" for the machine the command is running on, then the machine's
// operator-chosen name, then a short id, then "—" for a row that predates
// device ids.
//
// "this" outranks the name deliberately: the question an operator asks of
// this column is "is that agent here or somewhere else", and answering it
// should not require them to remember what they called the machine they are
// sitting at. The name is what makes the OTHER rows legible — and what a
// model matches when a human says "on my work laptop", which a hex prefix
// could never support.
func deviceCell(id, name, local string) string {
	switch {
	case id == "" && name == "":
		return "—"
	case local != "" && id == local:
		return "this"
	case name != "":
		return name
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
	role, broadcast, standing, wake     *bool
	yes                                 *bool
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
	v.standing = fs.Bool("standing", false, "with --broadcast: also reach sessions that start later, until they read it (standing orders)")
	v.wake = fs.Bool("wake", false, "with --broadcast: BREAK-GLASS — actively interrupt every recipient now instead of the polite next-turn default (use sparingly)")
	v.intent = fs.String("intent", "", "message intent: fyi, reply-requested, or action-requested")
	v.yes = fs.Bool("yes", false, "with --broadcast: skip the blast-radius confirmation prompt (answer yes)")
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
	if *v.standing && !*v.broadcast {
		return fmt.Errorf("--standing requires --broadcast")
	}
	if *v.wake && !*v.broadcast {
		return fmt.Errorf("--wake requires --broadcast (a direct message already delivers promptly)")
	}
	if *v.yes && !*v.broadcast {
		return fmt.Errorf("--yes requires --broadcast (only a broadcast asks to confirm its blast radius)")
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
			return fmt.Errorf("usage: muster send --broadcast [--project <p>] [--standing] <body> [--intent fyi|reply-requested|action-requested]")
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
	// --from is an operator-typed name, exactly like reply's --from
	// (thread.go) — local-first expand it before it reaches the daemon, or a
	// bare --from (the very short form the CLI prints as its own hint)
	// either mis-attributes the thread to an unrelated foreign agent of that
	// name or, with none, an unresolvable orphan FromAgent.
	fromAlias := expandAlias(*v.from, rosterAliasExists())
	sendArgs := map[string]any{
		"from": fromAlias, "to_kind": toKind, "to_target": toTarget,
		"subject": *v.subject, "ref": *v.ref, "body": body, "intent": *v.intent,
		"standing": *v.standing, "wake": *v.wake, "confirm": *v.broadcast && *v.yes,
	}
	raw, err := callData("send_message", sendArgs)
	if err != nil {
		return err
	}
	var res struct {
		ThreadID        int64    `json:"thread_id"`
		ConfirmRequired bool     `json:"confirm_required"`
		RecipientCount  int      `json:"recipient_count"`
		Recipients      []string `json:"recipients"`
		ToTarget        string   `json:"to_target"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	// The daemon refused the unconfirmed broadcast and handed back its blast
	// radius. Show it, ask, and only re-send with confirm=true on a yes —
	// unless there's no terminal to prompt on, where an explicit --yes is the
	// only way through.
	if res.ConfirmRequired {
		if !sendInteractive() {
			return fmt.Errorf("broadcast reaches %d agent(s) in %s — re-run with --yes to confirm (no terminal to prompt on)",
				res.RecipientCount, broadcastScope(res.ToTarget))
		}
		who := "(none currently live)"
		if len(res.Recipients) > 0 {
			who = strings.Join(res.Recipients, ", ")
		}
		_, _ = fmt.Fprintf(out, "broadcast reaches %d agent(s) in %s: %s\nSend? [y/N] ",
			res.RecipientCount, broadcastScope(res.ToTarget), who)
		if !readAffirmative(sendConfirmIn) {
			_, _ = fmt.Fprintln(out, "aborted.")
			return nil
		}
		sendArgs["confirm"] = true
		if raw, err = callData("send_message", sendArgs); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(out, "sent (thread %d)\n", res.ThreadID)
	return err
}

// sendConfirmIn is where the broadcast confirmation prompt reads its answer
// (os.Stdin in production; overridden in tests). sendInteractive reports
// whether a prompt can even be shown — false under `go test` and in pipes, so
// there a broadcast demands an explicit --yes rather than hanging on a read.
var (
	sendConfirmIn   io.Reader = os.Stdin
	sendInteractive           = stdinIsTTY
)

func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func readAffirmative(r io.Reader) bool {
	line, _ := bufio.NewReader(r).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// broadcastScope renders a broadcast's reach for a prompt or error: the whole
// bus for an empty target, else the named project.
func broadcastScope(project string) string {
	if project == "" {
		return "every agent on the bus"
	}
	return "project " + project
}

// sendBoolFlags are cmdSend's flags that take no value, needed so
// splitFlagsAndPositional knows not to consume the following token as a value.
var sendBoolFlags = map[string]bool{"role": true, "broadcast": true, "standing": true, "wake": true, "yes": true}

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
// kind=task threads are shown. The caller's tmux/harness identity is sent as
// proof (spec 2026-08-21 §3.2): the daemon only moves alias's read watermark
// for a caller who can prove it IS that session, sourced exactly like every
// other mint/proof site (tmuxenv.CaptureEnv for the tuple, harnessenv.FromEnv
// for the harness UUID; no caller_pane_id — ownership is session-granular,
// not pane-granular). Reading someone else's alias (an operator checking on
// another agent, `muster inbox` run outside any session) still works — it
// comes back as a harmless peek, threads shown, nothing marked read — and a
// trailing notice says so, or an operator has no way to tell a peek from a
// real drain of their own mail.
func printThreads(out io.Writer, alias string, tasksOnly bool) error {
	c := tmuxenv.CaptureEnv()
	h := harnessenv.FromEnv()
	raw, err := callData("get_inbox", map[string]any{
		"alias":                     alias,
		"caller_socket_path":        c.SocketPath,
		"caller_session_id":         c.SessionID,
		"caller_session_created":    c.SessionCreated,
		"caller_harness_session_id": h.SessionID,
	})
	if err != nil {
		return err
	}
	threads, markedRead, err := decodeInboxResponse(raw)
	if err != nil {
		return err
	}
	// Only FROM, LAST-FROM, and a to_kind=agent TO target are aliases — a
	// to_kind=role/broadcast target is a role or project name, never an
	// alias, and must not be run through the alias display map.
	aliases := make([]string, 0, len(threads)*3)
	for _, th := range threads {
		aliases = append(aliases, th.FromAgent, th.LastFrom)
		if th.ToKind == "agent" {
			aliases = append(aliases, th.ToTarget)
		}
	}
	disp := aliasDisplay(aliases)
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
			target := th.ToTarget
			if th.ToKind == "agent" {
				target = disp[th.ToTarget]
			}
			to = th.ToKind + ":" + target
		}
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", th.ID, th.Kind, disp[th.FromAgent], to, th.Status, disp[th.LastFrom], th.Unread, th.Subject); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if !markedRead {
		_, err := fmt.Fprintf(out, "peek only: '%s' is not this pane's alias — unread state unchanged\n", dispAlias(alias))
		return err
	}
	return nil
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
	if err := nudgeguard.Check(store.Agent{
		Alias: ag.Alias, DeviceID: ag.DeviceID, DeviceName: ag.DeviceName,
		SocketPath: ag.SocketPath, SessionID: ag.SessionID, SessionCreated: ag.SessionCreated, PaneID: ag.PaneID,
	}, dispAlias(ag.Alias)); err != nil {
		return err
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
		// Stripped, like the alias beside it: this field's vocabulary is tmux
		// session names, which never carry the device prefix, and the stripped
		// alias is exactly the name the session was registered under.
		sessionName = dispAlias(ag.Alias)
	}
	if _, err := fmt.Fprintf(out, "nudging %s → session %s / pane %s on %s\n", dispAlias(ag.Alias), sessionName, ag.PaneID, ag.SocketPath); err != nil {
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
