# tmux registered-agent badge (`@muster_agent`) — design

**Date:** 2026-07-18
**Status:** approved (brainstormed with Court in the dotfiles session)
**Scope:** muster (daemon push + wake surface) and, separately, a small dotfiles
`.tmux.conf` display change. This spec covers both; only the muster half lives
in this repo.

## Problem

When a coding agent registers on the bus — especially via the MCP
`register_agent` tool, where the agent picks its own alias — the operator has
no ambient way to know (a) that registration actually succeeded, and (b) the
exact alias to use when messaging it. Today you have to run `muster agents`
and cross-reference sessions by hand.

## Solution

A per-session tmux user option, `@muster_agent`, holding the comma-joined
alias list for that tmux session, pushed by the daemon on every registration
change. The operator's status bar renders it as a **dimmed** `📬 <alias>`
badge — visually the quiet sibling of the existing bright `📬N` unread badge,
occupying the same conceptual slot:

| status bar shows        | meaning                                   |
|-------------------------|-------------------------------------------|
| (nothing)               | no registered agent in this session       |
| dimmed `📬 backend`     | registered as `backend`, inbox drained    |
| dimmed `📬 backend,api` | split-identity session, two aliases       |
| bright `📬3`            | unread mail (existing behavior, unchanged)|

Because the daemon sets the option only **after** the store write commits, the
badge's presence is itself verification that the bus accepted the
registration. The alias is plain text in the status bar, so the operator can
mouse-select it to copy.

## Architecture (muster side)

Three touch points, honoring the tmux-agnostic daemon rule (all tmux contact
via the injected wake surface):

1. **`internal/wake` — new agent-badge surface.** Alongside the existing
   `Notifier` (inbox counts), add an alias setter with the same shape and
   semantics: option name `@muster_agent` (constant default; overridable at
   construction like the notifier's option), per-call subprocess bounded by
   the same default 500 ms timeout, best-effort throughout, `refresh-client`
   after set/unset so title bars repaint. Setting an empty alias list unsets
   the option. Injected into the daemon at construction in `cmd/muster`
   exactly as `wake.NewTmuxNotifier` is today.

2. **Register path.** After `handleRegisterAgent` commits, the daemon
   recomputes the full alias list for the registered `(socket_path,
   session_id)` tuple (the same grouping `session_aliases` uses), sorts it,
   and pushes it via the badge surface. Comma-joined, e.g. `backend,api`.
   Re-registration that *moves* an alias between sessions pushes to both the
   old and new tuples so a stale badge never lingers on the losing session.

3. **Deregister / purge / GC paths.** Same recompute-and-push after any op
   that removes an agent (`deregister_agent`, `purge_agent`, gc). When the
   tuple's alias list goes empty, the push unsets the option.

Agents with no tmux tuple (empty socket/session — registered outside tmux)
are skipped: nothing to push to, same as the inbox wake.

## Error handling / staleness

Identical contract to `@muster_inbox`: pushes are best-effort; a dead
session, missing tmux, or timeout is ignored — the store stays authoritative.
If the daemon restarts, existing tmux options persist and re-converge on the
next registration change for that session. `SessionEnd` deregistration
already clears on normal session exit; a killed session takes its options
with it.

## Display (dotfiles side, separate commit in that repo)

- `status-left`: unread badge wins the slot. When `@muster_inbox` is set,
  render the bright `📬N` exactly as today; otherwise, when `@muster_agent`
  is set, render dimmed `📬 #{@muster_agent}` (Catppuccin overlay-gray
  foreground, no background block).
- `set-titles-string`: same conditional so the terminal-tab title carries the
  alias too.

## Testing

- `internal/wake`: unit tests for the badge surface mirroring the existing
  notifier tests (set, unset-on-empty, timeout bound, best-effort error
  swallowing).
- `internal/daemon`: wiring tests following `wake_wiring_test.go` —
  register pushes the sorted comma-joined list; second alias on the same
  tuple pushes both; deregister of the last alias unsets; alias moving
  between tuples updates both; non-tmux registration pushes nothing.
- Gate: `just verify` (fmt, lint, `go test -race`, build).

## Out of scope

- No change to the bright unread badge, the Stop-hook drain loop, or nudge.
- No polling fallback; sessions on machines without the new daemon simply
  never show the badge.
- No new CLI subcommand; `muster agents` remains the detailed roster.
