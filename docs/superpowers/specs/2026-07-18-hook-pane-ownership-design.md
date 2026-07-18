# Hook pane ownership — subagents must not act as the session's agent

**Date:** 2026-07-18
**Status:** approved (designed live in the dotfiles session after field evidence)
**Scope:** `internal/humancli` (hook paths) + one `internal/tmuxenv` helper. No
daemon, store, or wire changes. No dotfiles changes.

## Problem (observed live, 2026-07-18)

Coding-agent harnesses spawn subagents as full Claude sessions in panes of the
SAME tmux session, and those subagents run the same session hooks. There is no
environment marker distinguishing a subagent's hook process from the primary
session's (probed empirically: child env is identical). Today every hook
event trusts the session, so within one tmux session:

- **SessionStart** (subagent spawns): re-registers the session-name alias and
  moves the roster's `pane_id` to the subagent's pane — nudges and any
  pane-targeted delivery now go to the wrong Claude (observed: `prefix m`
  nudged an idle reviewer subagent instead of the operator's conversation).
- **Stop** (subagent finishes a turn while the inbox badge is lit): the
  subagent receives the drain prompt and reads mail addressed to the primary
  conversation.
- **SessionEnd** (subagent exits): deregisters the alias — a dying subagent
  tombstones the primary session's registration.

## Rule

The roster's stored `pane_id` becomes meaningful: it is the session's
**primary agent pane** — first live claimant wins. `muster hook` events act
only when the calling pane owns (or can claim) that identity:

1. **SessionStart** — resolve alias (unchanged precedence: arg/$MUSTER_ALIAS/
   session name), `get_agent` it:
   - not found → register (claim).
   - found with a DIFFERENT (socket_path, session_id) tuple → register
     (cross-session takeover, exactly today's semantics — a renamed or
     recreated session reclaims its name).
   - found with the SAME tuple → register only if the stored pane is mine,
     empty, or **dead** (`tmuxenv.IsPaneAlive` false). A live different pane
     means a primary already owns this identity: **no-op**.
2. **Stop** — after the existing `@muster_inbox > 0` gate and alias-list
   fetch, emit the drain JSON only if at least one of the session's aliases
   has `pane_id == $TMUX_PANE` (via `get_agent` per alias — 1-2 cheap local
   calls). If the roster yields no pane information at all (no aliases
   resolvable / daemon unreachable), keep today's fallback behavior
   unchanged — the gate only engages when the roster names an owner and it
   isn't me.
3. **SessionEnd** — deregister only if `get_agent(alias)` exists and its
   `pane_id` is mine or empty. Otherwise no-op.

## Scoping decisions

- **Hook-only.** The explicit CLI commands `muster register` / `muster
  deregister` keep raw upsert/steal semantics — an operator typing a command
  overrides; only the ambient hook path is gated.
- **No daemon change.** Ownership is decided client-side in the hook using
  the existing `get_agent` op plus tmux liveness — the same client-side
  liveness pattern station's collision probes use; the daemon stays
  tmux-agnostic. The register itself remains a plain upsert once the hook
  decides to proceed (no new CAS semantics; the residual race between two
  simultaneously-starting Claudes is benign — one of them wins, both are
  live).
- **MCP `register_agent` untouched.** An agent that explicitly self-registers
  a custom alias is making a deliberate claim, like the CLI.
- **Failure posture unchanged:** hooks must never block a session — every new
  check degrades to today's behavior on error (daemon down, tmux unreachable
  → act as before).

## New helper

`tmuxenv.IsPaneAlive(socket, paneID string) bool` — true iff the pane still
exists on the socket (query `display-message -p -t <pane> '#{pane_id}'`
non-empty through the existing `query` seam). Sits beside `IsSessionAlive`.

## Testing

- `internal/tmuxenv`: `IsPaneAlive` unit tests through the `Run` override
  (alive, dead → error, empty socket/pane).
- `internal/humancli` (`hook_test.go` patterns, callData/tmuxenv seams):
  - SessionStart: no-op when same tuple + live foreign pane; registers when
    pane dead, pane empty, pane mine, alias absent, or foreign tuple.
  - Stop: silent when no session alias's pane is mine; drains when mine;
    today's behavior preserved when roster/pane info is unavailable.
  - SessionEnd: no-op on foreign live-owner pane; deregisters when owner.
- Gate: `just verify`.

## Out of scope

- Per-window/per-subagent aliases, `nudge --pane`, focus-based re-registration
  (all considered and rejected — they add identity complexity for what is a
  "don't let ghosts act" problem).
- Harness-side hook suppression (no reliable marker exists; probed).
