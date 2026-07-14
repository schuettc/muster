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
   scoped to their **project**, so nobody has to remember that `muster-2` is the
   frontend — and `send frontend` never silently hits the wrong project's frontend.

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
  `ProjectFromSocket` (strip `proj-` from the socket basename), `IsSessionAlive`,
  and `SessionLabel` (returns the `@claude_task` value and whether it's manually
  pinned via `@claude_task_manual`). Both the MCP `register_agent` tool and the
  new CLI use it — one capture path, no drift. `tmuxQuery`/the command runner
  stays injectable for tests.
- **`internal/store`:** `Agent` gains `Project` / `Label` / `LabelManual` fields +
  matching columns; a small idempotent migration adds the columns to existing
  databases. New `DeleteAgent(alias)`.
- **`internal/daemon`:** one new op, `deregister_agent`. The daemon core remains
  tmux-agnostic — tmux is only ever touched through the CLI layer or the injected
  `wake.Notifier`.
- **`internal/humancli`:** new `register`, `deregister`, `gc` commands; `agents`
  enriched with `LABEL` + `LIVE`; one canonical **target resolver** (alias-or-label)
  reused by `send`, `nudge`, `inbox`, `tasks`.

## Data model

`agents` gains three columns:

```sql
project      TEXT    NOT NULL DEFAULT ''   -- derived from the tmux socket (proj-<project>)
label        TEXT    NOT NULL DEFAULT ''   -- snapshot of @claude_task at register time
label_manual INTEGER NOT NULL DEFAULT 0    -- 1 if @claude_task_manual (deliberately pinned)
```

`entries.from_agent` and `threads.*` store the alias as **text**, not a foreign
key to `agents`. Therefore deleting an agent row (deregister / gc) never breaks
message or task history — threads addressed to a since-removed alias remain
intact and readable. `label`/`label_manual`/`project` stored on the agent are a
**fallback snapshot** (for display/addressing when the session is dead); the live
values are read from tmux whenever the session is alive.

## Identity model

Three fields make up an agent's identity — one stable key, one scope, one human
handle:

- **`alias`** — the stable, **globally unique** routing key. It never changes
  under the operator, so it is NOT derived from the mutable label. Resolution
  precedence in `muster register`: explicit positional arg → `$MUSTER_ALIAS` →
  tmux session name (`#{session_name}`). Session names are already unique across
  projects (`muster-2`, `timewalk-2`), so an alias always addresses exactly one
  agent, from anywhere.
- **`project`** — the scope. Derived from the tmux **socket**: your setup runs
  one server per project on socket `proj-<project>`, so
  `project = basename(socketPath)` with the `proj-` prefix stripped. Empty when
  the session isn't proj-managed (ad-hoc `tat`/legacy server, or no tmux). This
  costs nothing — muster already captures `socket_path` at register.
- **`label`** — the human name (`frontend`/`backend`), read **live** from the tmux
  session option `@claude_task` (falling back to the stored snapshot when the
  session is dead). Option name defaults to `@claude_task`, overridable via
  `$MUSTER_LABEL_OPTION`. A label is **addressable only when manually pinned**
  (`@claude_task_manual = 1`, i.e. set via `prefix T`). This matters because
  `@claude_task` auto-populates with Claude's rolling conversation topic every
  turn unless pinned — an auto-topic is shown for display but is never a routing
  target.

### Canonical target resolution

A single `ResolveTarget(agents, given, callerProject) → alias` function, used by
every command that takes an agent target. `callerProject` is derived from the
CLI's own `$TMUX` socket at call time.

1. **Exact alias** — if `given` matches an agent `alias`, use it (alias wins;
   works cross-project since aliases are globally unique).
2. **Qualified label** — if `given` is `proj:label`, match agents where
   `project == proj` and addressable-label `== label`. One → its alias; several →
   error listing candidates; none → error.
3. **Bare label** — candidates = agents whose addressable-label `== given`:
   - Restrict to `callerProject` first: exactly one there → use it; several there
     → error.
   - None in `callerProject` but some elsewhere → error telling the operator to
     qualify it (list the candidates as `proj:label`). **Never silently cross
     project boundaries.**
   - None anywhere → "unknown agent" error.

So from within the `muster` project, `muster send frontend "…"` routes to
muster's frontend; to reach another project you write `muster send
timewalk:frontend`. (`:` is the qualifier, not `/`, because `/` already denotes a
worktree session like `timewalk/feature-x`.) The underlying alias never shifts if
you relabel with `prefix T`.

## Liveness

`tmuxenv.IsSessionAlive(socketPath, sessionID) bool` runs
`tmux -S <socket> has-session -t <session_id>` (exit 0 = alive). Performed in the
**CLI layer**, once per agent, so the daemon stays pure. An agent with an empty
socket/session (registered outside tmux) is reported dead/unknown, not alive.

- `muster agents` → list from daemon, enrich each row with live label + liveness,
  grouped by project:

  ```
  PROJECT    ALIAS      LABEL       MODEL   LIVE
  muster     muster     backend     claude  ●
  muster     muster-2   frontend    codex   ●
  timewalk   timewalk   frontend    claude  ●
  (none)     stale      —           codex   ✗
  ```

  A non-addressable auto-topic is dimmed/parenthesized so it's clearly not a
  routing handle.

- `muster gc` → list, and for every dead agent call `deregister_agent`. Prints
  what it reaped (never silently). Fixes the stale-agent problem (leftover
  registrations pointing at panes that no longer exist).

## CLI surface

```bash
muster register [alias] --role <r> --model <claude|codex>  # capture pane, upsert
muster deregister [alias]                                  # remove registration
muster gc                                                  # reap dead agents
muster agents                                              # now shows LABEL + LIVE
muster send <alias|label|proj:label> "msg" --from me       # target resolver
muster nudge <alias|label|proj:label>                      # target resolver
muster inbox <alias|label|proj:label>                      # target resolver
muster tasks <alias|label|proj:label>                      # target resolver
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
- `register` with no tmux env → registers with empty socket/pane/session/label
  and empty `project` (still addressable by alias).
- Ambiguous or cross-project bare label in `ResolveTarget` → explicit error naming
  the candidates as `proj:label`; never a silent guess or a silent cross-project hop.
- Migration: each `ALTER TABLE agents ADD COLUMN …` (project, label, label_manual)
  guarded so re-running (column already present) is a no-op — the live `bus.db`
  upgrades in place, no wipe.

## Testing

- **tmuxenv:** injected command runner — capture assembly; `ProjectFromSocket`
  (`proj-x` → `x`, non-proj socket → ""); `IsSessionAlive` alive/dead/error;
  `SessionLabel` present/empty/manual-vs-auto/custom option name.
- **store:** migration adds columns idempotently; `RegisterAgent` upsert
  sets/updates `project`/`label`/`label_manual`; `DeleteAgent` removes the row and
  leaves `entries`/`threads` intact.
- **daemon:** `deregister_agent` op removes the agent; unknown alias no-ops.
- **humancli:** against the existing `startTestDaemon` harness with injected tmux —
  `register` alias precedence + captured project/label; `deregister`; `gc` removes
  only dead agents; `agents` grouped with live label + liveness; `ResolveTarget`
  by exact alias, bare label in caller project, qualified `proj:label`,
  cross-project-without-qualifier error, ambiguous error, unknown error,
  auto-topic-label-not-addressable.
- All green under `just verify` (`go test -race`, macOS + Linux).
