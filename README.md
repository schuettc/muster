# muster

**Coordinate your coding agents — no copy/paste, subscription-only.**

`muster` is a local coordination bus that lets independent coding-agent
sessions (Claude Code and OpenAI Codex, each in its own tmux tab) hand tasks to
each other and talk back and forth — without you copy/pasting between terminals,
and without leaving your standard subscriptions. The bus never calls a model; it
only routes between agents already running on their own plans.

- **Task-board orchestration, multi-model.** Claude posts a "review this branch"
  task → a standing Codex session claims it, reviews, and replies. A consumer
  session files a request to its producer. All async, all local.
- **One static Go binary**, multi-mode: a lazy-started daemon, a stdio MCP server
  each agent registers, and a human CLI.
- **tmux-native wake** — the daemon lights a status-bar banner on the recipient's
  session (socket-aware, across per-project tmux servers) without ever typing into
  a pane; an operator `muster nudge` is the only send-keys path.

Landing page: **muster.tools**

## Status

**v0.1.0** — Milestones A–E shipped: SQLite store + lazy unix-socket daemon, the
`muster mcp` server (11 tools), the human CLI, and the notify/nudge wake. The
approved v1 design lives in [`docs/design.md`](docs/design.md).

## MCP mode

`muster mcp` runs muster as an MCP server over stdio, exposing the bus as tools
any MCP client (Claude Code, Codex) can call. Register it once per tool:

```bash
# Claude Code
claude mcp add muster -s user -- muster mcp
# Codex
codex mcp add muster -- muster mcp
```

Then, inside a session, the agent calls `register_agent` once, and can
`send_message` / `task_create` / `task_claim` / `task_transition` / `reply` /
`get_inbox` / `get_thread` / `list_agents` / `kv_set` / `kv_get`. The server
talks to the local `muster` daemon (auto-started); nothing is sent to any model
provider — muster only routes between agents already running on their own
subscriptions.

> Note: stdout is the MCP channel in this mode; muster writes all diagnostics to
> stderr.

## CLI

Beyond `muster mcp` (for agents), muster has operator commands you can run from
any shell to observe and drive the bus (they auto-start the daemon):

```bash
muster agents                              # who's registered
muster inbox <alias>                       # threads addressed to an agent
muster tasks <alias>                       # just the tasks for an agent
muster send <alias> "message"  --from me   # send a directed message
muster send --role reviewer "please look"  --from me   # to a role
muster send --broadcast "heads up"         --from me   # to everyone
```

### Notifications & nudging

When bus activity is addressed to an agent, muster **notifies** its tmux session
(sets `@claude_attn`, which lights the status-bar banner for tabs you're not
looking at) — it never types into a pane. The flag clears when that agent next
reads its inbox (`get_inbox`).

To actively poke an agent to act now:

```bash
muster nudge <alias>              # types "check your inbox" into the agent's pane
muster nudge <alias> --no-submit  # type only; don't press Enter
```

Nudge auto-submits for Claude Code; Codex holds the text in its composer, so
you press Enter there (muster tells you). Autonomous Codex wake (via its
app-server) is possible but requires launching Codex differently — deferred.

## License

TBD
