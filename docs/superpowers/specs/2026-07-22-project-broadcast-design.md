# Project-scoped broadcast

**Date:** 2026-07-22
**Status:** Approved

## Problem

Global broadcast already exists end-to-end (`to_kind='broadcast'`, CLI
`muster send --broadcast`, MCP `send_message`): it reaches every registered
agent on the bus. On a busy machine (30+ agents across a dozen projects)
that is too blunt — an announcement that matters to one project's agents
badges every live session everywhere. The common case is "tell everyone in
project X," and there is no way to express it.

A secondary finding: the operator didn't know global broadcast existed, so
the MCP tool description should advertise both forms more loudly.

## Decision

Reuse `to_kind='broadcast'` and carry the scope in `to_target`, which is
empty for every existing broadcast row:

- `to_target = ''` — global broadcast, everyone (unchanged semantics; all
  historical rows already mean this).
- `to_target = '<project>'` — reaches only agents whose registered
  `project` equals that string exactly.

No schema change, no new `to_kind`, backward compatible with old rows and
old clients. A new `to_kind='project'` was considered and rejected: it
touches every `to_kind` switch across daemon/store/station/render/MCP for
no added capability at this scope.

## Semantics

- Scope matches the registered `project` string **exactly** — no prefix,
  glob, or wildcard matching.
- Concern is evaluated at read time (same as global broadcast today): an
  agent that registers into the project *after* the send still sees the
  thread in its inbox.
- **Project validation (daemon-side, hard reject):** a project-scoped
  broadcast is rejected unless at least one **non-departed** agent is
  registered with that exact project. This mirrors the existing
  "black-hole fix" for mistyped aliases (see `internal/daemon/resolve.go`)
  — the daemon is the only backstop before a thread gets created that
  concerns nobody, and both surfaces (CLI and MCP) pass `to_target`
  through unresolved. Departed-only projects are rejected too. The error
  lists the known (non-departed) projects so the sender can correct in one
  round trip, e.g.:
  `no registered agents in project "bettor-hlep" (known projects: bettor-help, muster, …)`.
  Global broadcast (empty target) is never validated — there is nothing to
  typo. No fuzzy or prefix matching of project names: project strings come
  from tmux capture and are stable, so it is exact-match-or-clear-error.
- An agent with an empty `project` never matches a scoped broadcast; it
  still receives global broadcasts.
- The sender's own session is excluded from wake/notify, as with all sends
  today.

## Changes by component

### Store (`internal/store/threads.go`)

The one canonical concern predicate gains the scope check. In
`threadConcerns`:

```sql
OR (threads.to_kind='broadcast' AND (threads.to_target=''
     OR threads.to_target=(SELECT project FROM agents WHERE alias=?)))
```

`threadConcernsJoin` changes identically (with `sess.alias`). The existing
equivalence test (`TestThreadConcernsSessionJoinEquivalence`) must keep
passing; extend its fixture matrix with scoped-broadcast thread shapes.
Note the alias bind count in `threadConcerns` increases; update callers'
bound-argument lists accordingly.

### Daemon (`internal/daemon/daemon.go`)

- **Send-time validation:** in the send_message and task_create op
  handlers, when `to_kind=='broadcast'` and `to_target != ''`, reject
  unless some non-departed agent's `project` equals `to_target` exactly
  (roster read via the store, consistent with `resolveAgentTarget`'s
  pattern). Error format:
  `no registered agents in project "<target>" (known projects: <sorted, deduped, non-departed>)`.
- `notifyForThread`: the `case "broadcast"` fan-out filters recipients to
  `a.Project == th.ToTarget` when `th.ToTarget != ""`.
- `targetOf`: journal target renders `broadcast` for global,
  `broadcast:<project>` for scoped.

### Render / station

`internal/render/renderer.go` and `internal/station` treat the literal
string `broadcast` specially; teach them the `broadcast:<project>` form
(display as-is or as `broadcast:<project>` — no resolution needed).

### MCP (`internal/mcpserver`)

No new tool. `send_message` / `task_create` descriptions and the
`to_target` jsonschema hints are rewritten to advertise both forms:
"to_kind=broadcast with empty to_target reaches every agent; set to_target
to a project name to reach only that project's agents." This doubles as
the discoverability fix.

### CLI (`internal/humancli`)

- `muster send --broadcast "body"` — global (unchanged).
- `muster send --broadcast <project> "body"` — project-scoped. The
  positional slot is currently a usage error when combined with
  `--broadcast`, so it is free to claim: two positional args mean
  project + body, one means body. Help text and `registry.go` synopsis
  updated.

## Testing

- **Store:** scoped broadcast concerns same-project agents, not
  other-project or empty-project agents; global broadcast still concerns
  everyone; both predicate forms (direct + join) agree via the extended
  equivalence fixture; departed agents' preserved `project` behaves
  consistently.
- **Daemon:** wake fan-out badges only same-project sessions for a scoped
  broadcast; journal rows carry `broadcast:<project>`; scoped broadcast to
  an unknown project is rejected with the known-projects error; scoped
  broadcast to a departed-only project is rejected; global broadcast is
  never rejected.
- **CLI:** arg parsing for one- and two-positional `--broadcast` forms.

## Out of scope

Prefix/wildcard project matching, fuzzy project-name correction,
multi-project targets, broadcast to a named set of agents, any new
`to_kind`, persistence/announcement semantics beyond the existing thread
model.
