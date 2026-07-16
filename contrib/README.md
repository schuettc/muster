# contrib — optional setup pieces

muster works with nothing but the binary and an MCP registration (see the main
README). The pieces here are the two optional layers on top:

## 1. Show the mailbox in tmux (`tmux-mailbox.conf`)

When mail arrives for an agent, the daemon sets `@muster_inbox=<unread count>`
on its tmux session — but tmux won't *display* it until you tell it to. Add the
two lines in [`tmux-mailbox.conf`](tmux-mailbox.conf) to your `~/.tmux.conf`
(merge them into your own `set-titles-string` / `status-left` if you customize
those), reload tmux, and sessions with unread muster mail show `📬<count>` in
the tab title and status bar until the agent reads its inbox.

## 2. Hooks: auto-register + self-resolving inbox

The muster binary is its own hook — no script to copy. Point your agent's
session hooks at `muster hook <event> <model>`:

- **SessionStart** → registers the session on the bus.
- **Stop** (turn end) → if the session has unread muster mail, tells the agent
  to drain its inbox and reply — autonomously.
- **SessionEnd** (Claude Code only; Codex has no such event) → deregisters.

Setup:

- **Claude Code:** merge [`claude-settings-hooks.json`](claude-settings-hooks.json)
  into `~/.claude/settings.json`.
- **Codex:** copy [`codex-hooks.json`](codex-hooks.json) to `~/.codex/hooks.json`.
  On the next `codex` launch you'll get a one-time "Hooks need review" prompt —
  choose Trust. Codex fires `SessionStart` lazily, on the session's first turn —
  a freshly opened Codex session is not on the bus until you say something to
  it, so give it any first message ("hi" is enough) before addressing mail to it.

If `muster` isn't on the PATH your harness gives hook commands (e.g. it lives in
`~/go/bin`), use the absolute binary path in the `command` strings — Codex in
particular does not expand `~`.

The hook is safe to install globally: for any session that isn't a registered
agent it does nothing, and it never blocks a session from starting.

### `muster-session-hook.sh`

The same behavior as a standalone POSIX shell script, for anyone who wants to
customize the hook (change the drain instruction, add logging, gate it per
project). Functionally identical to `muster hook`; point your hook config at
the script instead if you use it.
