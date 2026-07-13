# Local agent coordination bus — design

**Date:** 2026-07-13
**Status:** Approved (v1 scope)
**Name:** `muster` · landing page **muster.tools** (registered)

## Goal / north star

A **local coordination fabric** that lets independent coding-agent sessions —
Claude Code and OpenAI Codex, each in its own tmux tab — hand work to each
other and talk back and forth, **without copy/paste and without leaving standard
subscriptions.**

The north star is **task-board orchestration with multi-model support.** Two
scenarios define "done":

1. **Cross-model review** — Claude (backend) posts a "review this branch" task;
   a standing Codex session claims it, reviews, and posts the verdict back;
   Claude sees it and acts. All async, no copy/paste.
2. **Consumer → producer feedback** — a session working in the *consumer*
   (`~/bettor-help`) files a request/bug to the *producer*
   (`bettor-help-workspace`); the producer session picks it up, ships it, and
   replies. The consumer gets used the way it's meant to be, and its feedback
   reaches the producer automatically.

## Non-negotiable constraints

- **Standard subscriptions only.** Claude Code runs on the Claude subscription;
  Codex runs on the ChatGPT subscription (`codex login`). The bus itself is a
  free local binary and **never calls a model** — it only routes between agents
  already running on their own plans. No API keys, nothing metered.
- **Multi-model / agent-agnostic.** Anything that speaks MCP and lives in a tmux
  pane can participate. Claude and Codex are the v1 clients.
- **Per-project tmux servers.** Verified: this machine runs **one tmux server
  per project**, each on its own socket (`__proj_srv() → proj-<project>`), e.g.
  `proj-bettor-help` and `proj-bettor-help-workspace` are *different servers*. A
  naive `tmux send-keys -t <session>` on the default socket silently misses
  them. The wake mechanism must be socket-aware.

## Architecture

A **single static Go binary**, multi-mode, with a **lazy-started daemon** over a
**unix domain socket**. Go is chosen for easy publishing/sharing (`go install` /
GitHub release / brew tap, no runtime deps) and because it makes a long-lived
daemon cheap.

Modes of the one binary:

| Mode | Role |
|---|---|
| `muster serve` | The daemon (the hub). Owns SQLite, does the tmux wakes. |
| `muster mcp` | Thin **stdio MCP server** each agent registers (`claude mcp add` / `codex mcp add`). Talks to the daemon over the socket. |
| `muster send \| inbox \| tasks \| agents \| tail` | **Human CLI** — drive the mesh from any shell, no agent needed. |
| `muster tui` | Live dashboard (deferred to v3). |

**Lazy daemon:** the `mcp`/CLI modes auto-spawn the daemon if the socket is
dead (the `docker`/`tmux` pattern). This gives a central hub (single state
owner, a home for future push/dashboard/contract-watchers) with **zero lifecycle
management** — no launchd, auto-heals. State lives in SQLite (WAL mode) at
`~/.local/share/muster/bus.db`; the socket at `~/.local/share/muster/sock`.

**Delivery model (settled best practice):** poll for content, push for the wake.
Content is pulled via MCP tools; a **`tmux send-keys` knock** wakes the target
pane. We do **not** build on MCP's native `--channels` server-push — it's
unreliable for idle Claude Code sessions today (known upstream bugs), and the
tmux knock is deterministic here.

## Relationship to the Codex MCP bridge

Two different axes; both kept:

- **Bridge (already built)** = *vertical*. Claude invokes an **ephemeral** Codex
  inline within one terminal for a quick one-shot second opinion. No standing
  session, no coordination.
- **Bus (this design)** = *horizontal*. **Standing** peer sessions hand tasks to
  each other async, with back-and-forth. Covers Claude↔Codex *and*
  consumer↔producer (which the bridge cannot do).

**Decision:** keep the bridge as an *optional convenience* — it's already built,
costs nothing, and covers the "no Codex tab open" case. It is **not load-bearing**
for the bus; removable anytime (`claude mcp remove codex` + drop dotfiles bits)
if a single coordination paradigm is preferred later.

## Data model — one object: the thread

Message and task are the same object at different fidelity. The core object is a
**thread** (a conversation between agents); message vs task is just how much
lifecycle it carries.

```
thread {
  id, kind: message|task,
  from_agent, to: {role|agent},
  subject, ref,               # ref = a POINTER (repo/branch/endpoint/file), not payload
  status,                     # null for messages; state machine for tasks
  created_at, updated_at
}
entry {                       # append-only; the back-and-forth lives here
  id, thread_id, from_agent, body, at,
  status_change               # optional: records a task transition
}
```

- **Message** = `kind=message`, no status. Post → wake. A reply is another entry
  → wakes the other side. Back-and-forth for free.
- **Task** = `kind=task` + a status machine. Same `entry` mechanism carries the
  discussion; the final entry carries the result.

**Principle: the bus carries pointers, not payloads.** Agents share a
filesystem, so a review task says "branch `feat/wagers` in repo X" and the
reviewer reads it / runs `codex review` directly. Diffs never go through SQLite.

## Task state machine (a collaboration model, not a rigid workflow)

| State | Meaning | Wakes |
|---|---|---|
| `open` | created, unclaimed | assignee(s) |
| `claimed` | someone took it (folds "in progress") | requester |
| `needs_info` | bounced back — assignee needs an answer to proceed | requester |
| `blocked` | waiting on an external thing | requester |
| `completed` | done; result in the final entry | requester |
| `declined` | assignee can't/won't | requester |
| `cancelled` | requester withdrew | assignee |

Transitions are mostly free: either party can move it; we record **who** moved
it and wake the **other** party. `needs_info ↔ claimed` is the loop that captures
a few rounds of back-and-forth. Every state change is an `entry`, so the thread
holds full history.

**Role-addressed delivery:** a task sent `to: {role: reviewer}` when several
agents hold that role **wakes all of them; first to `task_claim` wins** (claim is
atomic — a second claim on an already-claimed task fails). A task sent to a
specific `agent` wakes only that agent. Same rule for messages: a role/broadcast
message fans out; a directed message hits one.

## Registry, roles, passive liveness

`register_agent` is called on agent startup and **self-reports from its own env**
— no convention-guessing:

```
agent {
  alias,                      # you choose, e.g. "backend" / "reviewer" (addressable)
  role,                       # producer | consumer | reviewer | ... (addressable)
  model_type,                 # claude | codex
  socket_path,                # from $TMUX field 1  → e.g. /private/tmp/tmux-501/proj-bettor-help
  pane_id,                    # from $TMUX_PANE     → e.g. %6
  session_name,               # #S, display + secondary lookup
  registered_at, last_seen
}
```

Addressing = **option C**: a chosen `alias` (and/or `role`) maps to the stable
`(socket_path, pane_id, session_name)` tuple. Because the wake targets
socket+pane, delivery is **rename-proof** — renaming the tmux session doesn't
break it.

**Passive liveness only — no heartbeats, no reaping** (detached ≠ dead in a tmux
world; the mailbox persists):

- `register_agent` on startup **upserts** — a restarted session self-heals a
  stale tuple.
- `last_seen` bumped on any tool call. `agents` list shows "last seen 2m ago".
- Liveness checked **at wake time**: cheap `tmux -S <socket> has-session` before
  `send-keys`. If gone, the item just waits in the inbox.

## Wake mechanism

```
tmux -S <socket_path> send-keys -t <pane_id> -l "<message>" ; send Enter
```

- Socket-aware (`-S <absolute path>`) → crosses per-project servers correctly.
- `-l` (literal) + separate Enter → avoids shell/newline mangling.
- Existence check before send; skip + leave in inbox if the pane is gone.
- **Known consideration:** a knock while the target Claude is *mid-turn* queues
  the text and submits after the turn. v1 accepts this; a later refinement can
  `capture-pane` to confirm the pane is at a prompt before knocking.

## MCP tool surface (v1)

- **Registry:** `register_agent`, `list_agents`
- **Threads/messages:** `send_message` (directed or broadcast), `reply`,
  `get_inbox`, `get_thread`
- **Tasks:** `task_create` (to role/agent, subject, ref), `task_claim`,
  `task_update` (status transition + note), `task_complete` (result),
  `task_list`, `task_get`
- **KV (thin extra):** `kv_get`, `kv_set` (shared facts: API base URL, schema
  version, ports)

## v1 scope

- Single Go binary: `serve` (lazy daemon) + `mcp` shim + basic human CLI.
- Registry + roles + passive liveness.
- Unified thread model: messages and tasks over one schema, with entries/replies.
- Richer task state machine (above).
- tmux socket-aware wake.
- Claude **and** Codex both register and participate.
- Broadcast + KV as thin extras.
- Dotfiles integration: `brew`/`go install` the binary; register the `mcp` mode
  in Claude (`claude mcp add`) and Codex (`codex mcp add`); document in README +
  a `docs/` cheat sheet, matching the codex-bridge pattern.

## Deferred (the staged future — same binary, same schema)

- **v2 — Contracts:** producer publishes its API surface; consumers subscribe
  and get diffs on change (`register_contract` / `subscribe_contract` /
  `diff_contracts`). The bettor-help killer feature.
- **v3 — Control plane:** `muster tui` dashboard (agents, live activity from
  pane titles, inbox depths, task board) + richer human CLI.
- **Later:** advisory file leases, append-only event/timeline log, cross-machine
  sync.

## Testing

1. **Unit:** thread/task state transitions; registry upsert; KV.
2. **Wake:** integration test that spawns two tmux sessions on *different*
   sockets, registers both, sends a task, asserts the knock lands in the right
   pane (assert via `capture-pane`).
3. **Cross-server:** explicitly exercise `proj-bettor-help` ↔
   `proj-bettor-help-workspace` addressing.
4. **Multi-model:** register a Claude `mcp` client and a Codex `mcp` client
   against the same daemon; round-trip a task both directions.
5. **End-to-end (scenario 1):** Claude `task_create` → Codex `task_claim` →
   `task_complete` → Claude sees result, no copy/paste.
6. **Lazy daemon:** kill the daemon; confirm next `mcp`/CLI call respawns it and
   state persists (SQLite).
7. **Liveness edge:** kill a registered session; confirm sender doesn't error and
   the item waits in inbox.

## Out of scope (v1)

- MCP `--channels` native push (unreliable; tmux knock instead).
- Heartbeats / liveness reaping (passive only).
- Contracts, dashboard TUI, file leases, event log, cross-machine (deferred).
- Payload transfer through the bus (pointers only).
- Mid-turn wake suppression (accepted; refine later).

## Open questions

1. ~~**Name.**~~ **Decided:** `muster` (command / repo / brew formula), landing
   page **muster.tools** (registered).
2. **Repo location.** This is a shareable standalone Go project — its own repo,
   not `dotfiles`. Decide the GitHub owner/org and whether it's public from day
   one before scaffolding.
3. **Go MCP library.** Pick during planning (candidates incl. `mark3labs/mcp-go`
   and the official Go SDK) — verify maturity for stdio server + our tool set.
4. **Codex wake path.** v1 uses the universal tmux knock for Codex too; a later
   option is Codex's app-server WebSocket (xats notes it's "cleaner than tmux
   paste"). Left as an upgrade, not v1.
