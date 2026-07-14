# Session Identity: auto-register, tmux liveness, addressable labels — Design

**Status:** approved 2026-07-14. Post-v0.1.0 (Milestone F).

## Goal

Make muster's agent registry **self-maintaining** and **human-legible** by leaning
on the two facts that define muster's architecture: the **daemon is the API**
(the CLI is a peer client of it, not a client of the MCP server) and **tmux is
the substrate** (always present; queryable at any time). Concretely:

1. A plain CLI command registers an agent (so a shell hook can auto-register it),
   capturing the tmux pane reliably from the shell environment.
2. The agent list tells the truth about who is alive, by asking tmux — no
   heartbeats, no reaping timers.
3. Agents are surfaced and addressed by their **tmux label** (the `@claude_task`
   session option the operator sets with `prefix T`, e.g. `frontend`/`backend`),
   so nobody has to remember that `muster-2` is the frontend.

## Non-goals / deferred

- **Hook wiring lives in dotfiles** — the user asked not to touch dotfiles yet.
  This spec delivers the muster-side surface (`muster register`/`deregister`) and
  *documents* the SessionStart/SessionEnd hook snippets; actual `settings.json` /
  Codex-config wiring is a separate dotfiles change. Everything here is testable
  by running `muster register` manually (simulating the hook).
- Idle-agent auto-inbox on the `Stop` hook, `muster watch`, the status-bar unread
  segment, and the v3 TUI remain future roadmap items.

## Architecture

The core stays as-is; changes are additive.

- **`internal/tmuxenv` (new, extracted):** the single canonical place for tmux
  interaction from outside the daemon. Moves `tmuxSocketPath` / `tmuxQuery` and
  the pane-capture assembly out of `internal/mcpserver/tools_registry.go`; adds
  `IsSessionAlive` and `SessionLabel`. Both the MCP `register_agent` tool and the
  new CLI use it — one capture path, no drift. `tmuxQuery`/the command runner
  stays injectable for tests.
- **`internal/store`:** `Agent` gains a `Label` field + `label` column; a small
  idempotent migration adds the column to existing databases. New
  `DeleteAgent(alias)`.
- **`internal/daemon`:** one new op, `deregister_agent`. The daemon core remains
  tmux-agnostic — tmux is only ever touched through the CLI layer or the injected
  `wake.Notifier`.
- **`internal/humancli`:** new `register`, `deregister`, `gc` commands; `agents`
  enriched with `LABEL` + `LIVE`; one canonical **target resolver** (alias-or-label)
  reused by `send`, `nudge`, `inbox`, `tasks`.

## Data model

`agents` gains one column:

```sql
label TEXT NOT NULL DEFAULT ''    -- snapshot of @claude_task at register time
```

`entries.from_agent` and `threads.*` store the alias as **text**, not a foreign
key to `agents`. Therefore deleting an agent row (deregister / gc) never breaks
message or task history — threads addressed to a since-removed alias remain
intact and readable. The `label` stored on the agent is only a **fallback
snapshot**; the live value is read from tmux whenever the session is alive.

## Identity model

- **`alias`** — the stable routing key. It must not change under the operator, so
  it is NOT derived from the mutable label. Resolution precedence in
  `muster register`: explicit positional arg → `$MUSTER_ALIAS` → tmux session
  name (`#{session_name}`). For two tabs in one repo this yields stable unique
  keys like `muster` / `muster-2`.
- **`label`** — the human name. Read **live** from the tmux session option
  `@claude_task` at list/resolve time (falling back to the stored snapshot when
  the session is dead). The option name defaults to `@claude_task` and is
  overridable via `$MUSTER_LABEL_OPTION` (knob, not hard-weld to the dotfiles
  convention).

### Canonical target resolution

A single `ResolveTarget(agents, given) → alias` function, used by every command
that takes an agent target:

1. If `given` exactly matches an agent `alias` → that alias (alias always wins).
2. Else if exactly one live agent has `label == given` → that agent's alias.
3. Else if multiple agents share that label → error listing the candidate aliases.
4. Else → "unknown agent" error.

So the operator types `muster send frontend "…"` and it routes to `muster-2`,
while the underlying alias never shifts if they relabel with `prefix T`.

## Liveness

`tmuxenv.IsSessionAlive(socketPath, sessionID) bool` runs
`tmux -S <socket> has-session -t <session_id>` (exit 0 = alive). Performed in the
**CLI layer**, once per agent, so the daemon stays pure. An agent with an empty
socket/session (registered outside tmux) is reported dead/unknown, not alive.

- `muster agents` → list from daemon, enrich each row with live label + liveness:

  ```
  ALIAS      LABEL       MODEL   LIVE
  muster     backend     claude  ●
  muster-2   frontend    codex   ●
  stale      —           codex   ✗
  ```

- `muster gc` → list, and for every dead agent call `deregister_agent`. Prints
  what it reaped (never silently). Fixes the stale-agent problem (leftover
  registrations pointing at panes that no longer exist).

## CLI surface

```bash
muster register [alias] --role <r> --model <claude|codex>  # capture pane, upsert
muster deregister [alias]                                  # remove registration
muster gc                                                  # reap dead agents
muster agents                                              # now shows LABEL + LIVE
muster send <alias|label> "msg" --from me                  # target resolver
muster nudge <alias|label>                                 # target resolver
muster inbox <alias|label>                                 # target resolver
muster tasks <alias|label>                                 # target resolver
```

`register` run outside tmux still succeeds (addressable, empty pane fields → no
wake target). `deregister` of an unknown alias is a no-op success. Both are safe
to wrap `|| true` in a hook so they never block session start/end.

## Hook integration (documented; dotfiles wiring deferred)

- **Claude Code** — `settings.json`:
  `SessionStart` (matcher `startup|resume`) → `muster register --model claude || true`;
  `SessionEnd` → `muster deregister || true`.
- **Codex** — equivalent SessionStart/SessionEnd hook entries → same commands
  with `--model codex`.

Because the hook runs in the pane shell, `$TMUX`/`$TMUX_PANE` are present, so the
capture is more reliable than the MCP subprocess ever was.

## Error handling

- Missing tmux binary / dead socket → `IsSessionAlive` false, `SessionLabel` "".
- `register` with no tmux env → registers with empty socket/pane/session/label.
- Ambiguous label in `ResolveTarget` → explicit error naming the candidates.
- Migration: `ALTER TABLE agents ADD COLUMN label …` guarded so re-running (column
  already present) is a no-op — the live `bus.db` upgrades in place, no wipe.

## Testing

- **tmuxenv:** injected command runner — capture assembly; `IsSessionAlive`
  alive/dead/error; `SessionLabel` present/empty/custom option name.
- **store:** migration adds column idempotently; `RegisterAgent` upsert sets/updates
  `label`; `DeleteAgent` removes the row and leaves `entries`/`threads` intact.
- **daemon:** `deregister_agent` op removes the agent; unknown alias no-ops.
- **humancli:** against the existing `startTestDaemon` harness with injected tmux —
  `register` alias precedence + captured fields; `deregister`; `gc` removes only
  dead agents; `agents` shows live label + liveness; `ResolveTarget` by alias, by
  label, ambiguous, unknown.
- All green under `just verify` (`go test -race`, macOS + Linux).
