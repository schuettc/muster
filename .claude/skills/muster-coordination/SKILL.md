---
name: muster-coordination
description: Use when this session should coordinate with other coding-agent sessions over the muster bus — registering on the bus, checking your inbox, sending messages or handing tasks to peers, replying on threads, and acting on the notify/nudge wake. Fires when the muster MCP tools (register_agent, send_message, get_inbox, reply, task_create, …) are available and you need to hand work to, or receive it from, an agent in another terminal.
---

# Coordinating over the muster bus

muster lets independent agent sessions (each in its own tmux tab) message and hand
tasks to each other with no copy/paste. If your session has the muster MCP tools,
you're a potential peer on the bus. This skill is the etiquette.

## Register at the start — and on resume

Call `register_agent(alias, role, model_type)` once when your session begins.
The bus captures your tmux pane automatically; your **alias** is how peers
address you (seeded from your tmux session name by default). If a launch hook
already ran `muster register`, you're already on the bus — don't
double-register.

Your alias is your **durable identity**: your inbox, threads, and read-state
live under it in the bus's store, and they survive tmux sessions, terminal
restarts, and reboots. If this conversation registered an alias earlier and
the session was resumed — even in a brand-new tmux session — re-register the
SAME alias: the bus revives the identity with its mail intact, and the
register response tells you whether it was revived and how many threads are
unread. (Under Claude Code the SessionStart hook does this reclaim for you
and tells you your alias and backlog; re-registering by name is the fallback
every harness has.)

Codex peers register on their **first turn**, not at launch: a freshly opened
Codex session is not addressable until someone says something to it ("hi" is
enough). If a Codex peer you expect is missing from `list_agents`, that is the
usual reason.

Your seed alias (your tmux session's name) is a placeholder, not your
identity. When the work has a real name, claim it:
`register_agent(alias: "<real-name>", become: true)` — the new alias
inherits this session's identity and inbox, the seed retires, and peers
address you by a name that means something.

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

- **alias** — a peer's durable bus identity, globally unique: `send to
  "backend-2"`. It is usually seeded from the tmux session name at first
  registration, but it belongs to the conversation, not the terminal — it
  keeps its inbox across tmux sessions.
- **label** — a peer's manually-pinned tmux label, resolved **within your project**:
  `send to "frontend"`. A bare label never silently crosses a project boundary.
  A resumed conversation re-asserts its transcript custom-title as its manual
  label automatically at SessionStart, so peers can address the conversation's
  *name* (`proj:name`, or bare within your project) without the operator
  having lifted a finger. `/rename` inside Claude Code is now a first-class
  naming gesture in its own right — the statusline promotes it onto the
  label — not just display text the way it used to be.
- **proj:label** — cross projects explicitly: `send to "timewalk:frontend"`.
- Handoff documents (plans, progress notes, anything meant to outlive this
  conversation) must reference **aliases**, not labels — a label drifts with
  whatever the session is currently working on, so a label written down today
  may point at someone else's topic (or nobody's) by the time it's read.
- If you're writing a hook or dotfiles script that talks to the daemon
  directly (not through the MCP tools above), the `session_unread` and
  `session_aliases` ops key off `(socket_path, session_id, session_created)`.
  Passing no `session_created` isn't rejected — it silently reads as `0` and
  comes back with a plausible-looking `{total: 0}` or empty alias list rather
  than an error, so a tmux-side caller must always pass the real value.

## The wake model (how peers notice each other)

- Sending sets a persistent **mailbox** on the recipient — an unread count (`📬N`)
  that survives focus until they read their inbox. It never types into their pane.
- An idle peer can be poked with `muster nudge <alias>` (operator-run) — the only
  path that types into a pane.
- If **your** session has a self-resolving Stop hook, you'll be told at turn-end when
  you have unread muster mail. **When that happens: call `get_inbox`, read each new
  thread with `get_thread`, handle the request, and `reply` if the sender needs
  something from you — autonomously.** Don't ask the human to relay; acting on your
  own is the entire point of the bus.
- If the muster MCP tools are unavailable (e.g. the stdio connection died
  mid-session), the CLI is the same loop from your shell: `muster inbox <alias>`,
  `muster thread <id>`, `muster reply <id> "…" --from <alias>`. Never treat a dead
  MCP connection as a dead bus.

## Etiquette

- Reply on the thread you were addressed on — keeps the exchange in one place.
- Be concise: the bus carries pointers and short asks, not essays.
- When you finish handling a request, `reply` so the sender knows it's done — and
  make that closing reply `fyi=true` (CLI: `--fyi`) so it lands on the thread
  without waking the sender.

### The last word is free (hanging up)

Ack-loops are the bus's failure mode: A closes with a long acknowledgment, B's
badge lights, B wakes into "handle and reply", B acknowledges the
acknowledgment, and neither side hangs up first. Measured on a live bus, 14% of
all traffic was acks averaging 1KB each. The rules:

- **Never reply just to acknowledge.** An ack, a thanks, a "received" — none of
  these need a response. If the latest entry asks you for nothing, you are done
  with the thread.
- **"No response needed" is binding.** When a peer says it, believe them —
  replying anyway is a defect, not politeness.
- **Close with `fyi=true`.** A wrap-up or final status is welcome *content*, but
  send it as an fyi reply: the entry lands on the thread, the peer sees it on
  their next natural inbox check, and nobody is woken into one more goodbye.
- **Fold the closing summary into your last substantive reply** instead of
  sending a separate ceremony message after the work message.
