---
name: muster-coordination
description: Use when this session should coordinate with other coding-agent sessions over the muster bus — registering on the bus, checking your inbox, sending messages or handing tasks to peers, replying on threads, and acting on the notify/nudge wake. Fires when the muster MCP tools (register_agent, send_message, get_inbox, reply, task_create, …) are available and you need to hand work to, or receive it from, an agent in another terminal.
---

# Coordinating over the muster bus

muster lets independent agent sessions (each in its own tmux tab) message and hand
tasks to each other with no copy/paste. If your session has the muster MCP tools,
you're a potential peer on the bus. This skill is the etiquette.

## Register once, at the start

Call `register_agent(alias, role, model_type)` a single time when your session
begins. The bus captures your tmux pane automatically; your **alias** is how peers
address you (default: your tmux session name). If a launch hook already ran
`muster register`, you're already on the bus — don't double-register.

Codex peers register on their **first turn**, not at launch: a freshly opened
Codex session is not addressable until someone says something to it ("hi" is
enough). If a Codex peer you expect is missing from `list_agents`, that is the
usual reason.

## The core loop

- **`list_agents`** — who's on the bus (project, label, liveness).
- **`send_message(to, body, …)`** / **`reply(thread_id, body)`** — message a peer, or
  continue a thread you were addressed on.
- **`get_inbox()`** — your pending threads (metadata only). **`get_thread(id)`** —
  the full thread; always drill in with `get_thread` to read message bodies before
  acting.
- **`task_create` / `task_claim` / `task_transition`** — for work with a lifecycle
  (open → claimed → completed / needs_info / blocked / …). Use a **task** when someone
  must *do* something; a **message** for FYI or discussion.
- **`kv_set` / `kv_get`** — a shared scratchpad for state both sides pull on demand
  (an API contract, an agreed decision, a running port).

## Addressing

- **alias** — a peer's tmux session name, globally unique: `send to "backend-2"`.
- **label** — a peer's manually-pinned tmux label, resolved **within your project**:
  `send to "frontend"`. A bare label never silently crosses a project boundary.
- **proj:label** — cross projects explicitly: `send to "timewalk:frontend"`.

## The wake model (how peers notice each other)

- Sending sets a persistent **mailbox** on the recipient — an unread count (`📬N`)
  that survives focus until they read their inbox. It never types into their pane.
- An idle peer can be poked with `muster nudge <alias>` (operator-run) — the only
  path that types into a pane.
- If **your** session has a self-resolving Stop hook, you'll be told at turn-end when
  you have unread muster mail. **When that happens: call `get_inbox`, read each new
  thread with `get_thread`, handle the request, and `reply` — autonomously.** Don't
  ask the human to relay; acting on your own is the entire point of the bus.

## Etiquette

- Reply on the thread you were addressed on — keeps the exchange in one place.
- Be concise: the bus carries pointers and short asks, not essays.
- When you finish handling a request, `reply` so the sender knows it's done.
