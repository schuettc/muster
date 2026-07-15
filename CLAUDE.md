# CLAUDE.md — muster

Conventions for working in this repo (humans and agents).

## What muster is

A local multi-agent coordination bus: independent coding-agent sessions (Claude
Code + OpenAI Codex, each in its own tmux tab) hand tasks/messages to each other —
no copy/paste, subscription-only (the bus never calls a model; it routes between
agents already running on their own plans). One static Go binary, multi-mode
(`serve` daemon · `mcp` stdio server · human CLI).

## Build / test / run

- **`just verify`** — the gate: `gofmt`, `golangci-lint`, `go test -race`, build.
  Run it before every commit; CI runs the same recipe, so local and CI can't drift.
- **cgo-free** — the binary builds under `CGO_ENABLED=0` (pure-Go SQLite via
  `modernc.org/sqlite`). Don't add cgo dependencies.
- **macOS tests** use `internal/mustertest.ShortHome()` for unix-socket paths (the
  `sun_path` ~104-char limit; `t.TempDir()` is too long and breaks the socket).
- Build + run: `go build -o ~/.local/bin/muster ./cmd/muster`, then
  `muster serve | mcp | agents | send | inbox | tasks | nudge | register | …`.

## Branch model

`feat/* → dev → main`. CI (`just verify`) is required on `dev` and `main`; `main` is
the release line (tags + GitHub releases). Never develop on `main` — do feature work
in a git worktree off `dev`, merged via PR.

## Architecture (the mental model)

- **daemon = API.** A lazy unix-socket daemon speaking newline-delimited JSON
  (`internal/proto`). The MCP server (`internal/mcpserver`) and the human CLI
  (`internal/humancli`) are **peer clients** of the daemon — neither goes through the
  other. Any daemon op is reachable from a plain CLI subcommand.
- **tmux = substrate.** Liveness, wake, and identity lean on tmux — but only through
  `internal/tmuxenv` (the one canonical capture path) or the injected
  `wake.Notifier`. Keep `internal/daemon` and `internal/store` tmux-agnostic.
- **Wake is split.** `internal/wake` *notifies* (sets the `@muster_inbox` tmux
  option; never types into a pane). `internal/nudge` is the **only** send-keys path.
  The daemon never types.

## Hard rules

- **stdout is sacred in `mcp` mode** — it is the MCP channel. All diagnostics go to
  **stderr**. A stray `fmt.Println` on an mcp-mode path corrupts the protocol.
- **One canonical module per concern** — identity capture lives in `internal/tmuxenv`,
  not copied around. Extend the owner; don't fork it.
- **Knobs, not constants** — operator-tunable defaults over hardcoded numbers.

## Package map

`cmd/muster` entrypoint · `internal/proto` wire protocol · `internal/client` daemon
client · `internal/daemon` the daemon · `internal/store` SQLite persistence ·
`internal/mcpserver` MCP tools · `internal/humancli` operator CLI · `internal/wake`
notify · `internal/nudge` send-keys · `internal/tmuxenv` tmux capture/liveness/label
· `internal/paths` socket+db paths · `internal/clock` injectable time ·
`internal/mustertest` shared test helpers.

## Using the bus itself

If you're an agent working here and want to coordinate with sessions in other
terminals, the `.claude/skills/muster-coordination` skill is the etiquette
(register, inbox, send/reply, addressing, the wake model).
