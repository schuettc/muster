# muster roadmap

Post-v0.1.0 directions. The organizing principle: **daemon = API, tmux =
substrate, CLI = universal client, hooks = automation glue.** The MCP server and
the human CLI are *peer clients* of the daemon — neither goes through the other —
so any daemon op is reachable from a plain CLI command with no MCP involved. And
because tmux is always present, `$TMUX_PANE` + the stored `session_id` are
reliable and we can ask tmux questions (liveness, labels) at any time. Every idea
below slots into one of those four roles.

## 1. Real liveness, zero heartbeats

Ask tmux `has-session -t <session_id>` to know who is actually alive. `muster
agents` gains a live/dead column; `muster gc` reaps dead registrations. No
heartbeats, no reaping timers. *(In progress — see
`docs/superpowers/specs/2026-07-14-session-identity-design.md`.)*

## 2. Lifecycle hooks beyond register

- **SessionStart → `muster register`**, **SessionEnd → `muster deregister`** — the
  registry maintains itself as tabs open and close. *(In progress — same spec.)*
- **Stop (turn end) → idle agent auto-drains its inbox** — the missing zero-touch
  half of the wake loop. Today an idle agent needs an operator `nudge`; a Stop
  hook lets it check its own inbox the moment it goes idle.

## 3. tmux status-bar integration

The daemon already sets `@muster_inbox=<unread count>` on the recipient's session
(persists until `get_inbox`), rendered as a `📬<count>` mailbox. Remaining: a
`muster inbox <self> --count` query the tmux status-right can poll (same pattern
as the dotfiles git/context segments) for an ambient indicator without waiting on
inbound activity.

## 4. CLI as the full operator / observability surface

All daemon clients: `muster watch` (live bus tail — precursor to the TUI),
`register`/`deregister`/`gc`, richer `reply`. The v3 `muster tui` is then "just
another daemon client" that also overlays tmux liveness.

## Further out (from the v1 design)

- **v2 Contracts** — a producer publishes its API surface; consumers subscribe and
  get diffs on change (the bettor-help producer/consumer use case).
- **v3 Dashboard TUI** — `muster tui`: live agents/activity/tasks.
- Advisory file leases; append-only event log; cross-machine.
