package humancli

import (
	"errors"
	"flag"
	"io"
	"os"

	"github.com/schuettc/muster/internal/station"
)

// Group buckets a Command for the grouped bare/`help` listing and the man
// page's SECTION headings. Order matters: groupOrder below is the only place
// display order is decided, so adding a group means updating both here and
// groupOrder.
type Group int

// The four command groups, in the order the operator sees them everywhere
// (bare usage, `muster help`, the man page): talk first (the thing you do
// most), watch second, identity third, plumbing last (daemon/dev internals,
// rarely typed by hand).
const (
	GroupTalk Group = iota
	GroupWatch
	GroupIdentity
	GroupPlumbing
)

// groupOrder is display order for the four groups.
var groupOrder = []Group{GroupTalk, GroupWatch, GroupIdentity, GroupPlumbing}

// groupHeading names each group's listing header.
var groupHeading = map[Group]string{
	GroupTalk:     "Talk",
	GroupWatch:    "Watch",
	GroupIdentity: "Identity",
	GroupPlumbing: "Plumbing",
}

// Command is one row of muster's command registry — the single table that
// drives bare `muster` / `muster help` (grouped usage), `muster help <cmd>`
// and `muster <cmd> -h/--help` (per-command usage), and the generated man
// page. There is deliberately no second list anywhere: cmd/muster's main()
// routes serve/mcp/debug itself (they need process-level setup this package
// has no business doing — daemon startup, stdio protocol framing) but still
// declares a Registry row so help/man rendering covers them; Run is nil for
// exactly those three, everything else is dispatched through Dispatch.
type Command struct {
	// Name is the subcommand word, e.g. "send".
	Name string
	// Synopsis is the argument shape shown after the name in usage output,
	// e.g. `send <target> "body" [--from <alias>] ...`. It does NOT repeat
	// "muster " or the command name.
	Synopsis string
	// Summary is the one-line description shown in grouped usage listings.
	Summary string
	// Help is one or more longer paragraphs shown by `muster help <cmd>` /
	// `muster <cmd> -h`, below the synopsis. May be empty.
	Help string
	// Group buckets this command for display.
	Group Group
	// NewFlags builds a fresh *flag.FlagSet declaring this command's flags,
	// for `PrintDefaults`-driven help/man rendering. It is the SAME
	// constructor the command's real Run function calls to parse its own
	// args (see e.g. newSendFlags) — one declaration, not a help-text copy
	// that can drift from the real flags. nil means the command takes no
	// flags.
	NewFlags func() *flag.FlagSet
	// Run executes the command. nil for serve/mcp/debug, which cmd/muster's
	// main() owns directly (see the Command doc comment above).
	Run func(args []string, out io.Writer) error
}

// Registry is every muster subcommand, operator-facing and plumbing alike.
// Do not add a command anywhere else: Dispatch, Usage, HelpFor, and the man
// renderer all walk this slice, so an entry here is the only thing required
// for a command to show up consistently everywhere.
//
// Built in init() rather than as a var literal on purpose: several Run
// closures below call HelpFor, which (via lookup) reads Registry itself.
// Go's package-initialization-cycle check is a static, conservative
// approximation that follows every function a var initializer references,
// transitively — it can't tell that HelpFor is only ever called long after
// init(), so a `var Registry = []Command{...Run: closureThatCallsHelpFor...}`
// literal trips a false-positive "initialization cycle for Registry" at
// compile time. Assigning inside init() sidesteps that check entirely: it's
// ordinary sequential code, not a variable initializer expression.
var Registry []Command

func init() {
	Registry = []Command{
		{
			Name:     "send",
			Synopsis: `send <target> "body" [--from <alias>] [--subject <s>] [--ref <r>] [--role] [--broadcast] [--intent fyi|reply-requested|action-requested]`,
			Summary:  "Send a message to an agent, role, or everyone.",
			Help: `target is an alias, a label, or a "project:label" pair, resolved the same
way for every muster surface (send, nudge, inbox, tasks). --role treats
target as a role name instead of an agent; --broadcast ignores target and
sends to every registered agent (target is then omitted: 'muster send
--broadcast "body"'). --intent tags the message for the recipient's
inbox/hook rendering: fyi (default, no action implied), reply-requested, or
action-requested.`,
			Group:    GroupTalk,
			NewFlags: newSendFlags,
			Run:      cmdSend,
		},
		{
			Name:     "nudge",
			Synopsis: `nudge <alias|label|proj:label> [--no-submit]`,
			Summary:  "Type a check-inbox nudge into an agent's tmux pane.",
			Help: `Resolves the target to its registered tmux pane and types the check-inbox
line into it, auto-submitting (pressing Enter) when the agent's model type
accepts an unattended submit. --no-submit types the line but leaves it for
the operator (or the agent) to submit by hand.`,
			Group:    GroupTalk,
			NewFlags: newNudgeFlags,
			Run:      cmdNudge,
		},
		{
			Name:     "reply",
			Synopsis: `reply <thread-id> "body" [--from <alias>]`,
			Summary:  "Append a reply to an existing thread.",
			Help: `The CLI half of the MCP reply tool: appends an entry to the thread and
flags every participant's mailbox, exactly as a tool-sent reply would.
--from is the replying agent's alias (default "human"). Together with
'muster inbox' and 'muster thread' this completes the read-and-respond
loop from a plain shell — the fallback when a session has no muster MCP
connection.`,
			Group:    GroupTalk,
			NewFlags: newReplyFlags,
			Run:      cmdReply,
		},
		{
			Name:     "agents",
			Synopsis: "agents",
			Summary:  "List registered agents, grouped by project, with live status.",
			Help:     `Shows every registered agent's project, alias, label, model, and whether its tmux session is still alive.`,
			Group:    GroupWatch,
			Run: func(args []string, out io.Writer) error {
				if helpRequested(args) {
					return HelpFor("agents", out)
				}
				return cmdAgents(out)
			},
		},
		{
			Name:     "inbox",
			Synopsis: "inbox <alias|label|proj:label>",
			Summary:  "Show an agent's threads.",
			Help:     `Prints every thread in the target agent's inbox: id, kind, from, to, status, who spoke last, unread count, and subject.`,
			Group:    GroupWatch,
			Run:      cmdInbox,
		},
		{
			Name:     "tasks",
			Synopsis: "tasks <alias|label|proj:label>",
			Summary:  "Show an agent's task threads.",
			Help:     `Same as 'muster inbox' but filtered to kind=task threads only.`,
			Group:    GroupWatch,
			Run:      cmdTasks,
		},
		{
			Name:     "thread",
			Synopsis: "thread <id>",
			Summary:  "Show one thread's full conversation.",
			Help: `Prints the thread header (kind, participants, status, intent, subject)
then every entry oldest-first with author, timestamp, and the verbatim
body. The CLI half of the MCP get_thread tool. Side-effect-free: printing
a thread never marks it read — 'muster inbox' owns the read watermark.`,
			Group: GroupWatch,
			Run:   cmdThread,
		},
		{
			Name:     "events",
			Synopsis: "events [--agent <alias>] [--kind <kind>] [--thread <id>] [--limit <n>] [--aliases] [--full-time] [--width <cols>]",
			Summary:  "Print the bus journal (event log), oldest first.",
			Help: `The observability log behind every mailbox notify outcome (lit / cleared /
skipped / error), inbox read, send, task, and nudge event — "when was whose
mailbox actually lit." --aliases shows raw aliases instead of resolving
current labels; --full-time prints dates, not just times.`,
			Group:    GroupWatch,
			NewFlags: newEventsFlags,
			Run:      cmdEvents,
		},
		{
			Name:     "watch",
			Synopsis: "watch [--agent <alias>] [--kind <k>] [--thread <id>] [--interval <dur>] [--backlog <n>] [--aliases] [--full-time] [--width <cols>]",
			Summary:  "Tail the bus journal live.",
			Help: `Prints the last --backlog matching events, then polls for new ones every
--interval until interrupted (Ctrl-C). Side-effect-free: watching never
marks anything read.`,
			Group:    GroupWatch,
			NewFlags: newWatchFlags,
			Run:      func(args []string, out io.Writer) error { return cmdWatch(args, out, watchOpts{}) },
		},
		{
			Name:     "station",
			Synopsis: "station [--interval <dur>] [--aliases] [--width <cols>] [--alias <name>]",
			Summary:  "Full-screen operator TUI for the bus.",
			Help:     `A live, navigable projects → agents → threads view of the bus, built on the same event journal 'muster watch' tails.`,
			Group:    GroupWatch,
			NewFlags: station.FlagSet,
			Run: func(args []string, out io.Writer) error {
				// station.Run launches a full-screen bubbletea program and its
				// own flag.FlagSet doesn't discard -h output, so -h/--help is
				// intercepted here first, via a throwaway parse against the same
				// flag declarations (station.FlagSet) — see that function's doc
				// comment for why it's a second declaration rather than a shared
				// call.
				if err := station.FlagSet().Parse(args); errors.Is(err, flag.ErrHelp) {
					return HelpFor("station", out)
				}
				return station.Run(args)
			},
		},
		{
			Name:     "register",
			Synopsis: "register [<alias>] [--role <role>] [--model claude|codex]",
			Summary:  "Register the current tmux session as an agent.",
			Help: `Alias precedence: the explicit argument, then $MUSTER_ALIAS, then the tmux
session name. Captures the calling session's project/pane/socket identity
from tmux (internal/tmuxenv) so other commands can address it.`,
			Group:    GroupIdentity,
			NewFlags: newRegisterFlags,
			Run:      cmdRegister,
		},
		{
			Name:     "deregister",
			Synopsis: "deregister [<alias>]",
			Summary:  "Remove an agent's registration.",
			Help:     `Alias precedence mirrors register: the explicit argument, then $MUSTER_ALIAS, then the tmux session name. A soft delete (tombstone) — see 'muster gc'.`,
			Group:    GroupIdentity,
			Run:      cmdDeregister,
		},
		{
			Name:     "label",
			Synopsis: "label [<name>] [--clear]",
			Summary:  "Name or clear the current tmux session's label.",
			Help:     `Requires a tmux session ($TMUX set). Sets (or, with --clear or a bare 'muster label', clears) this session's addressable label in one command. When a live Claude Code agent is registered in this session, also types /rename <name> into its pane so the Claude session name follows.`,
			Group:    GroupIdentity,
			NewFlags: newLabelFlags,
			Run:      cmdLabel,
		},
		{
			Name:     "gc",
			Synopsis: "gc [--events-keep <dur>] [--purge-agents]",
			Summary:  "Reap dead agents and prune old journal events.",
			Help: `Tombstones every agent whose tmux session is no longer alive (a soft
delete: identity and read-state survive as history), then prunes journal
events older than --events-keep (default 720h = 30 days). --purge-agents
instead hard-deletes every departed or currently-dead agent row —
irreversible, off by default.`,
			Group:    GroupIdentity,
			NewFlags: newGCFlags,
			Run:      cmdGC,
		},
		{
			Name:     "serve",
			Synopsis: "serve",
			Summary:  "Run the daemon (lazy unix-socket API server).",
			Help: `The daemon speaks newline-delimited JSON over a unix socket
(internal/proto) and is the one process every other muster command and the
MCP server talk to. Logs one line to stderr on startup, then blocks until
SIGINT/SIGTERM.`,
			Group: GroupPlumbing,
		},
		{
			Name:     "mcp",
			Synopsis: "mcp",
			Summary:  "Run the MCP stdio server for coding-agent tool use.",
			Help: `Exposes the daemon's operations as MCP tools over stdio, for a coding
agent (Claude Code, Codex, ...) to call directly. stdout is the MCP
protocol channel — all diagnostics go to stderr.`,
			Group: GroupPlumbing,
		},
		{
			Name:     "hook",
			Synopsis: "hook <SessionStart|SessionEnd|Stop> [model]",
			Summary:  "Session-lifecycle hook entry point for agent harness configs.",
			Help: `The single entry point an agent harness's hook config (Claude Code,
Codex, ...) points at directly — not normally typed by hand. SessionStart
registers, SessionEnd deregisters, Stop checks the calling tmux session's
inbox and, if there's unread mail, prints decision:block JSON telling the
agent to drain it. model defaults to "claude" when omitted. Never blocks a
session: every internal error is swallowed.`,
			Group: GroupPlumbing,
			Run:   func(args []string, out io.Writer) error { return cmdHook(args, os.Stdin, out) },
		},
		{
			Name:     "debug",
			Synopsis: "debug <op> [key=value ...]",
			Summary:  "Send a raw op to the daemon (dev tool).",
			Help: `Bypasses muster's own subcommands and calls the daemon directly with an
arbitrary op and string key=value args, printing the raw JSON response. For
exploring or debugging the wire protocol — not part of the stable operator
surface.`,
			Group: GroupPlumbing,
		},
	}
}

// lookup finds a Registry command by name.
func lookup(name string) (Command, bool) {
	for _, c := range Registry {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// commandNames returns every registered command name, in Registry order.
func commandNames() []string {
	names := make([]string, 0, len(Registry))
	for _, c := range Registry {
		names = append(names, c.Name)
	}
	return names
}
