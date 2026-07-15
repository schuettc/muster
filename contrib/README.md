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

## 2. Hooks: auto-register + self-resolving inbox (`muster-session-hook.sh`)

Without hooks, you ask an agent to call `register_agent` once per session (or
put that instruction in your project's `CLAUDE.md`/`AGENTS.md`). With hooks,
sessions handle themselves:

- **SessionStart** → runs `muster register`, so every session joins the bus
  automatically.
- **Stop** (turn end) → if the session has unread muster mail, the hook tells
  the agent to drain its inbox and reply — autonomously, no human relay.

Setup:

1. Copy [`muster-session-hook.sh`](muster-session-hook.sh) somewhere stable and
   `chmod +x` it.
2. **Claude Code:** merge [`claude-settings-hooks.json`](claude-settings-hooks.json)
   into `~/.claude/settings.json`, fixing the script path. Claude expands `~` in
   hook commands.
3. **Codex:** copy [`codex-hooks.json`](codex-hooks.json) to `~/.codex/hooks.json`,
   fixing the script path — **use an absolute path** (Codex does not expand `~`).
   On the next `codex` launch you'll get a one-time "Hooks need review" prompt;
   choose Trust. Note Codex fires `SessionStart` lazily, on the session's first
   turn.

The hook is safe to install globally: for any session that isn't a registered
agent it does nothing, and it never blocks a session from starting.
