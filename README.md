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
- **tmux-native wake** — a `send-keys` knock on the target pane, socket-aware so
  it works across per-project tmux servers.

Landing page: **muster.tools**

## Status

Pre-implementation. The approved v1 design lives in
[`docs/design.md`](docs/design.md).

## License

TBD
