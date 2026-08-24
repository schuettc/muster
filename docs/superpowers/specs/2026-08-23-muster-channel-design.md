# `muster channel` — push delivery into the session

Status: DRAFT, pending Court's review. Date: 2026-08-23. Raised by: the pi harness program (schuettc/pi, spec `docs/superpowers/specs/2026-08-23-pi-harness-design.md`), Phase 1.

## The problem

muster's delivery is pull-shaped because MCP is pull-shaped. Mail lands in SQLite, the daemon lights `@muster_inbox` on the recipient's tmux session, and the agent finds out at its next turn end (the Stop-hook drain) or when the operator presses `prefix m` and `muster nudge` types into the pane. An idle agent at its prompt never sees new mail on its own. `muster nudge` — the only thing in muster that types into a pane — exists to paper over that gap.

Claude Code has **channels**: an MCP server declaring `capabilities.experimental["claude/channel"]` may emit `notifications/claude/channel`, and the event arrives inside a running session as a `<channel source="…">` message — starting a turn if the session is idle. galley proved (`galley channel`, spec `2026-08-10-the-channel-design.md`) that a dependency-free Go stdio server is a valid channel and that a push wakes an idle session in seconds with nobody typing. pi will consume the same convention through its `pi-channels` extension (Phase 2 of the harness program), so a muster channel serves both harnesses.

## What this delivers

`muster channel` — a new subcommand: an MCP channel carrier that pushes a compact envelope into the recipient's session the moment mail lands for it. The 📬 badge, the Stop-hook drain, and `muster nudge` all keep working unchanged as fallbacks; the channel is purely additive. Nudge leaves the critical path.

## Decisions

### Sibling subcommand, hand-rolled server — not an extension of `muster mcp`

`muster mcp` is built on `github.com/modelcontextprotocol/go-sdk` v1.6.1. Its `ServerCapabilities` accepts an `Experimental` map, but `ServerSession` exposes no way to emit an arbitrary notification method — only `NotifyProgress`, `Log`, and list-changed notifications. The channel therefore cannot be bolted onto the existing server without forking the SDK.

`muster channel` is a separate process with a minimal newline-delimited JSON-RPC 2.0 server over stdio, modeled on galley's `internal/mcp` (~150 lines: `initialize`, `ping`, `tools/list`, `tools/call`, plus a mutex-guarded `Notify(content, meta)` drained by one emitter goroutine so notifications never interleave with responses). Stdlib only; `go.mod` gains nothing. It is registered once per harness exactly like `muster mcp`:

```bash
claude mcp add muster-channel -s user -- muster channel
```

It is a **peer client of the daemon** (CLAUDE.md: daemon = API; mcp and CLI are peer clients). It never touches the store or tmux options directly.

### Identity: the pane, through `tmuxenv`

At startup the channel captures its pane tuple through `internal/tmuxenv.CaptureEnv()` — the one canonical capture path, the same call `register_agent` makes — and asks the daemon which live aliases that pane holds (the existing agent listing filtered by socket/session/pane, the lookup `paneRegistration` in `internal/mcpserver/tools_registry.go` already performs). It re-resolves on every poll tick, so `become`, `label`, and rename track without restart.

The channel starts with the session, and the SessionStart hook that registers the agent runs concurrently. If no alias is registered yet, the channel keeps polling the registry and pushes nothing; it never fails, never registers on the agent's behalf, and never blocks session start. A paneless session (`harnessenv` only) is out of scope for v1: the channel logs why to stderr and idles.

### Subscription: tail the journal, change nothing in the daemon

The bus journal already answers "what happened to alias X since event N": `list_events` in follow mode (`store.EventQuery{Agent, AfterID}`) is what `muster watch` tails. The channel polls it with the agent-concern filter for kinds `send`, `task`, `reply`, starting its cursor at the current max event id. Poll interval is a knob, `MUSTER_CHANNEL_INTERVAL` (default 1s, floor 250ms), never a constant.

Consequences worth stating: **v1 adds no `store.API` operation and no daemon op**, so it owes nothing to the DynamoDB backend or the conformance suite. Mail that already sits unread when the channel starts is not replayed event-by-event; the channel asks the daemon for the unread count once at startup and, if it is non-zero, emits one summary push (`muster: N unread — call get_inbox`).

Open item, not a v1 blocker: whether mail arriving from another device via the hosted-bus poller lands in the local journal as `send`/`reply` events. If it does not, cross-device pushes are a follow-up that adds a journal write to the poller's reconcile path.

### The envelope: compact, and the body stays in the bus

A push carries what the agent needs to decide and act, never the message body. Read-state stays authoritative in SQLite; the existing tools stay the only content interface — no second interface to keep in step.

One notification per poll tick, coalescing everything that tick found:

- `content` (one line, agent-readable): `muster: action-requested from reviewer on thread #42 "review the channel branch" — call get_thread 42, act, then reply.` With several events in one tick: `muster: 3 new — action-requested from reviewer on #42 "…"; reply from lead on #40 "…"; fyi from ops on #41 "…" — call get_inbox.`
- `meta` (string values only, identifier keys, as the channel convention requires): `kind`, `from`, `thread_id`, `intent`, `count`. When coalesced, `kind`/`from`/`thread_id`/`intent` describe the first event and `count` carries the total; the content line lists the rest.

`intent` is the thread's effective intent, joined at query time exactly as `list_events` already returns it (`fyi` | `reply-requested` | `action-requested`).

### Instructions: the loop, taught once at handshake

The `initialize` response carries an `instructions` string. It says: a `<channel source="muster-channel">` message means mail arrived on the bus for this session; call `get_thread` with the thread id (or `get_inbox` when the push names several), act on the request, and answer with `reply`; `fyi` means read and continue with no reply; act autonomously and do not ask the user whether to check mail. It names the alias(es) the channel resolved at startup so the agent never guesses its own address.

### Delivery confirmation: the read event already is one

galley needed a dedicated ack tool because its channel notifications are fire-and-forget and status had no other return path. muster already has one: calling `get_inbox`/`get_thread` clears `@muster_inbox` and the daemon journals a `read` event. That is the confirmation. An agent that ignores a push leaves the badge lit, and the Stop-hook drain or a `prefix m` nudge catches it exactly as today. No new tool is needed for content or acknowledgment.

The one tool the channel exposes is diagnostic — `muster_channel_status` — reporting the resolved pane, the aliases it is pushing for, the journal cursor, the last push time, and any reason it is idle (no registration yet, no pane, daemon unreachable). It mirrors `galley_channel_status` and exists so "is anyone listening?" is answerable from inside the session.

### Fallbacks and the nudge table

Nothing existing changes behavior. `internal/wake` still lights the badge; the Stop hook still drains; `muster nudge` still types on `prefix m`. The single edit outside the new subcommand: `internal/nudge`'s harness table (`claude` immediate-Enter; `codex`, `cursor` delayed submit) gains `pi`, so the fallback keeps working for pi sessions once they exist.

## Package layout

- `internal/channelmcp` — the stdlib JSON-RPC server (`Handler`, `Server`, `Notify`), a port of galley's `internal/mcp` with its tests: handshake golden, tools/call error-as-isError, notification ordering under concurrent Notify.
- `internal/channel` — the carrier: identity resolution (injected `tmuxenv` capture + daemon client), the poll loop (injected `clock`, injected daemon client, interval knob), envelope formatting (pure function, table-tested), the status tool, the startup unread summary.
- `cmd/muster` — the `channel` subcommand wiring stdin/stdout to `channelmcp.Server` and the carrier; `MUSTER_CHANNEL_INTERVAL` parsed here beside `MUSTER_POLL_INTERVAL`.
- `contrib/README.md` and `README.md` — registration instructions for Claude Code (and, once Phase 2 lands, pi).

## Testing

- `channelmcp`: golden JSON for `initialize` (capability + instructions), `tools/list`, `tools/call` success and error paths; a test that interleaves `Notify` from goroutines with request answers and asserts one JSON object per line, all parseable, notifications in call order.
- `channel`: envelope formatting for single and coalesced events across all three intents; poll loop against a fake daemon client — cursor advances, events before the start cursor are never pushed, re-resolution picks up an alias change, no-registration idles without error; startup unread summary emitted once.
- Integration: an in-process daemon via `internal/mustertest` with two registered agents; a `send` from one produces exactly one notification line on the other's channel stdout within one interval; a `reply` on that thread produces a second; a `get_inbox` produces none.
- Live trial: register `muster channel` in Claude Code, send mail from a second session, observe the idle session wake and reply with nobody typing. That trial is the acceptance test.
- Gate: `just verify` before every commit.

## Non-goals (v1)

- No daemon-side "route to channel if connected, else badge" logic — the badge always lights; the channel is additive.
- No message body in the push.
- No paneless-session support.
- No new `store.API` operations.
- No changes to `muster mcp`.
