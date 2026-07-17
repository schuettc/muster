# muster

**A local coordination bus for coding agents.**

`muster` lets coding-agent sessions in separate terminals send messages and hand
tasks to each other. Any agent that can register an MCP server can join the bus —
Claude Code and OpenAI Codex are the two it's tested with. Everything runs over a
local unix socket with state in a local SQLite file; muster itself never calls a
model.

- **Messages and tasks between sessions.** One agent posts a "review this branch"
  task; a standing session in another terminal claims it, works it, and replies.
- **One static Go binary**, three modes: a lazy-started daemon, a stdio MCP server
  each agent registers, and a CLI for you.
- **tmux-native wake** — mail sets a mailbox flag (`📬<count>`) on the recipient's
  tmux session; `muster nudge` is the only thing that types into a pane.

Landing page: **muster.tools**

## Status

**v0.2.2** — session identity (project-scoped agents, addressable labels,
tmux-verified liveness), the `@muster_inbox` mailbox, and Codex autonomy on top of
the v0.1.0 core (SQLite store, lazy daemon, MCP server, human CLI, notify/nudge
wake). See [releases](https://github.com/schuettc/muster/releases) for the changelog.

## Setup

```bash
# 1. install the binary (macOS or Linux; on Windows use WSL2)
curl -fsSL https://muster.tools/install.sh | sh
#    (or build from source with Go 1.22+: go install github.com/schuettc/muster/cmd/muster@latest)

# 2. register the MCP server with each agent
claude mcp add muster -s user -- muster mcp     # Claude Code
codex mcp add muster -- muster mcp              # Codex
# (any other MCP client: point it at `muster mcp` over stdio)

# 3. in each session, have the agent call register_agent once
#    (or add that instruction to your project's CLAUDE.md / AGENTS.md)
```

That's a working bus. Two optional layers, both in [`contrib/`](contrib/):

- **See the mailbox** — two lines of tmux config render `📬<count>` on tabs with
  unread mail ([`contrib/tmux-mailbox.conf`](contrib/tmux-mailbox.conf)).
- **Automate the lifecycle** — session hooks (`muster hook <event> <model>`)
  auto-register agents on start and have them drain their own inbox at turn
  end ([config for both harnesses in `contrib/`](contrib/README.md)).

## MCP mode

`muster mcp` runs muster as an MCP server over stdio, exposing the bus as tools
any MCP client (Claude Code, Codex) can call. Register it once per tool:

```bash
# Claude Code
claude mcp add muster -s user -- muster mcp
# Codex
codex mcp add muster -- muster mcp
```

The tools, by what they do:

| Group | Tools | Notes |
|---|---|---|
| Identity | `register_agent`, `list_agents` | join the bus once per session; see who's on it |
| Conversation | `send_message`, `reply`, `get_inbox`, `get_thread` | a **message** is a plain thread — no state, just an exchange |
| Work | `task_create`, `task_claim`, `task_transition` | a **task** is a thread with a lifecycle: `open → claimed → needs_info \| blocked → completed \| declined \| cancelled`. Claiming is atomic — two agents can't take the same task |
| Shared state | `kv_set`, `kv_get` | a key/value scratchpad both sides can read (an API contract, a port, a decision) |

The MCP server talks to the local daemon (auto-started on first use).

> Note: stdout is the MCP channel in this mode; muster writes all diagnostics to
> stderr.

## CLI

Agents coordinate through the MCP tools above — the CLI is for **you** (and for
hooks): commands you run from any shell to watch the bus and step in when you
want to (they auto-start the daemon):

```bash
muster agents                              # who's registered
muster inbox <alias>                       # an agent's threads — addressed to it or started by it
muster tasks <alias>                       # just the tasks for an agent
muster events                              # the bus event log: every mailbox notify and inbox read
muster watch                               # follow the bus live — every message, task, wake and read as it happens
muster station                             # the full-screen operator TUI — roster, live feed, threads, compose
muster send <alias> "message"  --from me   # send a directed message
muster send <alias> "message"  --from me --intent action-requested  # mark it as needing a reply
muster send --role reviewer "please look"  --from me   # to a role
muster send --broadcast "heads up"         --from me   # to everyone
```

### Registering & liveness

Agents can self-register (so a shell hook can do it at session start):

```bash
muster register [alias] --role <r> --model <name>
muster deregister [alias]
muster gc                 # reap agents whose tmux session is gone
```

`muster gc` also prunes the event log: rows older than `--events-keep` are
deleted (default `720h`, i.e. 30 days), so the journal doesn't grow without
bound on a long-running daemon.

`register` captures the tmux pane automatically. Alias precedence: explicit
arg → `$MUSTER_ALIAS` → tmux session name. `--model` is stored on the agent and
tunes `muster nudge`'s submit keystroke (`claude` and `codex` auto-submit; other
values are typed without submitting).

`muster agents` shows each agent's **project** and live **label**:

- **project** is derived from the tmux socket name when it follows a
  `proj-<name>` convention (one tmux server per project). On the default tmux
  server there's no project — everything shares one namespace, and the rest of
  muster works the same.
- **label** is a name you give a session: run `muster label backend` inside it
  (or `muster label --clear` to remove it). Only deliberately-set labels are
  addressable; auto-generated values are shown parenthesized and are not.
  (Stored in a tmux session option — default `@claude_task`, override with
  `$MUSTER_LABEL_OPTION`.)

### Addressing

Any command that takes a target — `send`, `nudge`, `inbox`, `tasks` —
accepts a target of the form `<alias|label|proj:label>`:

- an **alias** (the tmux session name, globally unique): `muster nudge muster-2`
- a **label**, resolved within your current project: `muster send frontend "…"`
- a **qualified label** to cross projects: `muster send timewalk:frontend "…"`

A bare label never silently crosses projects; if it's ambiguous or only exists
elsewhere, muster errors and lists the `proj:label` candidates.

### `muster station`

`muster station` is the operator's station — the full-screen TUI where
everyone reports in. It shows the roster (projects, live dots, labels, and
each session's unread count), the live journal feed, and threads grouped by
intent (`action-requested` pinned on top) in one view, and lets you act
without leaving it: open a thread to read it, compose a send or reply, and
nudge an agent, all from the keyboard.

Keys: `Tab` cycles focus between panes · `j`/`k` or the arrow keys move ·
`Enter` opens the selected agent's thread or roster entry · `Esc` backs out
· `s` opens the composer to send (with a target picker and an intent
cycle) · `r` replies on the open thread · `n` nudges the selected agent
(with a confirmation prompt) · `/` filters the focused pane · `a` toggles
aliases vs. labels · `q` quits.

Station registers on the bus itself, as agent `station` — `muster send
station "…"` and `muster nudge station` reach it like any other agent. If
an alias `station` is already live (a second station on the same machine),
it fails over to `station-2`, `station-3`, and so on. It deregisters on
quit, provided nothing else has since taken over its alias.

### Notifications & nudging

When a thread that concerns an agent gets new activity — a message addressed to
it, or a reply on a thread it started — muster sets `@muster_inbox` on its tmux
session to that agent's unread count. It never types into a pane, and unlike
a transient bell the flag **persists until the agent reads its inbox**
(`get_inbox`), which clears it. An agent's own writes never flag its own
mailbox. tmux doesn't display the option by default — add
the two render lines from [`contrib/tmux-mailbox.conf`](contrib/tmux-mailbox.conf)
to see `📬<count>` on the tab title and status bar.

The badge is a **session-level** count, not a per-alias one: if a session
registers under more than one alias (a session name plus a chosen label,
say), the count is that session's distinct unread threads, deduplicated
across its aliases — a thread addressed to both aliases is counted once,
and draining one alias's inbox brings the badge down to whatever the
session's other aliases still have unread, never to zero prematurely and
never double-counted.

A message or task carries an optional **intent** — `fyi`, `reply-requested`,
or `action-requested` (set with `muster send --intent`, or by the MCP
tools; a task defaults to `action-requested` since it's inherently a
request). `muster events` and `muster watch` tag rows with it (`[fyi]`,
`[reply?]`, `[action]`), and the Stop hook's drain instruction (below)
splits its count accordingly: "You have N unread muster thread(s), M
needing action."

Every notify outcome (lit, cleared, skipped, errored) and every inbox read is
recorded in an event log — `muster events [--agent <alias>] [--limit <n>]` —
so "whose mailbox was lit when, and when was it cleared" is answerable after
the fact. Rows now carry the thread's subject alongside its target, so you can
tell which conversation an event belongs to without a separate lookup.
`muster watch` is the live view of the same journal: it prints new rows as
they land instead of a fixed page, and Ctrl-C exits immediately.

To actively poke an agent to act now:

```bash
muster nudge <alias>              # types the full drain-and-act instruction into the agent's pane and submits
muster nudge <alias> --no-submit  # type only; don't press Enter
```

Nudge submits for both Claude Code (immediate Enter) and Codex (a short delayed
Enter — Codex treats an Enter bundled with pasted text as part of the paste).
Other model types are typed without submitting.

### Hooks (optional)

Registration and inbox-draining can be driven by session lifecycle hooks instead
of typed by hand:

- **SessionStart** → `muster register` — the session joins the bus on start.
  Claude Code fires this at launch; Codex fires it on the session's first turn,
  so say anything to a fresh Codex session ("hi" is enough) before addressing
  mail to it.
- **Stop** (turn end) → if the session has unread muster mail, the hook tells the
  agent to drain its inbox and reply, autonomously.
- **SessionEnd** (Claude Code) → `muster deregister`; `muster gc` covers the rest.

The muster binary is its own hook — point your harness at `muster hook <event>
<model>` (e.g. `muster hook Stop claude`). Copy-paste config for both Claude
Code and Codex is in [`contrib/`](contrib/README.md).

## License

[MIT](LICENSE) © Court Schuett
