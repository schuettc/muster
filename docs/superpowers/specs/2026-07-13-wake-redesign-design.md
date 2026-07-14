# Milestone E — Wake Redesign (notify + operator-nudge) — design

**Date:** 2026-07-13
**Status:** Approved (pending user review of this spec)
**Peer-reviewed:** by a Codex/gpt-5.6 agent over muster itself (thread 3) — its refinements are folded in.

## Goal

Replace muster's current wake — which blindly types into an agent's pane via `tmux send-keys` — with two cleanly-separated behaviors that (a) never corrupt in-progress typing, (b) don't depend on `send-keys` auto-submitting, and (c) require **no change to how Claude Code or Codex are launched**:

1. **Notify (automatic, default):** on bus activity, the daemon flags the recipient's tmux *session* (socket-aware) so the operator's existing status-bar banner lights up. No pane injection.
2. **Nudge (manual, operator-triggered):** `muster nudge <alias>` types a "check your inbox" prompt into an agent's pane — only when the operator explicitly asks (human judgment is the guard).

## Context / why (validated by live prototyping, 2026-07-13)

Against live Claude Code + Codex sessions we confirmed:
- **`send-keys` auto-submits for Claude Code but NOT for Codex** (Codex holds the text in its composer; the human must press Enter), and it **corrupts in-progress typing** — muster's current `wake.go` sends blind, unconditionally. This is a real bug.
- **Auto-detecting "composer empty" to guard send-keys is unreliable** across TUIs → so send-keys must be operator-triggered, not automatic.
- **The notify path works:** `tmux -S <socket> set-option -t <session> @claude_attn 1` + a client refresh lights the operator's existing banner, and correctly only "sticks" on **unfocused** tabs (attention auto-clears on focus — the right semantics).
- Codex's app-server (`--remote`) offers a clean, submitting, non-corrupting injection, but requires a two-process launch — **explicitly out of scope** here (opt-in future work), because the user requires no launch changes.

Conclusion: **notify is the automatic workhorse; nudge is a manual convenience; pull (agents call `get_inbox`) is the baseline.** Human-in-the-loop by design — accepted as the right trade-off under the no-launch-change constraint.

## Design

### 1. Registration captures tmux targets (socket-aware, derived — never user-supplied)

`register_agent` already runs inside the agent's process, so it can resolve the agent's tmux coordinates from its environment. Extend the captured tuple:

- `socket_path` — from `$TMUX` (field 1). *(already captured)*
- `session_id` — **NEW.** The stable tmux session id (e.g. `$1`), resolved socket-aware from `$TMUX_PANE`: `tmux -S <socket> display-message -p -t <pane> '#{session_id}'`. Used as the **notify target** — stable across `rename-session`, unambiguous within a socket. Do **not** target by session *name* (stale on rename, unreliable if user-supplied).
- `session_name` — display only (also from `display-message`).
- `pane_id` — from `$TMUX_PANE`. *(already captured)* Used **only** for `nudge`.

All four interpreted under `socket_path`. If `$TMUX` is unset (agent not in tmux), these are empty and both notify and nudge are no-ops (inbox still authoritative).

Store adds `session_id` to the `agents` table + `store.Agent`. `register_agent` MCP handler resolves `session_id`/`session_name` via a socket-aware `display-message` (it already reads `$TMUX`/`$TMUX_PANE`).

### 2. Daemon = notify-only

- Rename `wakeForThread` → **`notifyForThread`**; keep its existing recipient resolution (originator + agent/role/broadcast recipients, minus the actor) unchanged.
- Replace the `wake.Waker` dependency with a **`wake.Notifier`**:
  ```go
  type Notifier interface { Notify(socketPath, sessionID string) error }
  ```
- `TmuxNotifier` sets a configurable tmux user-option on the target session, socket-aware, then repaints that session's client so the title-based banner updates:
  ```
  tmux -S <socket> set-option -t <session-id> <option> 1     # default option: @claude_attn
  # repaint each client attached to that session (a bare refresh-client from the
  # daemon has no attached client of its own to target):
  tmux -S <socket> list-clients -t <session-id> -F '#{client_name}' | while read c; do
      tmux -S <socket> refresh-client -t "$c"
  done
  ```
  (The prototype confirmed the set-titles 🔔 only repaints after refreshing the session's specific client; a plain `refresh-client` is a no-op from the daemon. If no client is attached, the option is still set and paints on next attach/status tick — best-effort.)
- **Best-effort + bounded:** each `Notify` is a subprocess; wrap with a short `exec.CommandContext` timeout. Recipient fan-out runs after the store mutation and must not block/hang the actor's op — dispatch notifications so a slow/hung tmux can't wedge the request (goroutine the fan-out or bound it). Failures are swallowed (inbox is authoritative).
- **The daemon NEVER calls `send-keys`.** Automated bus activity can only ever *notify*. `send-keys` lives solely in the CLI nudge (§3).
- Injectable command runner on `TmuxNotifier` for tests.

### 3. Flag lifecycle (who clears `@claude_attn`, and when)

The notify option is a boolean that intentionally coalesces multiple events, so it must be cleared on **acknowledgement**, not per-notification, or the banner sticks forever:

- **Clear on inbox read (unconditional):** any `get_inbox` call for an alias clears the notify option for that agent's session (`tmux -S <socket> set-option -t <session-id> -u <option>`) — no unread-cursor tracking; reading = acknowledged. Ties the banner to "hasn't looked since last activity." (Mild failure mode: a routine `get_inbox` clears the 🔔 even if you hadn't noticed it — accepted for simplicity; a read-cursor is deferred.)
- Focus-clear (the operator switching to the tab) is handled by the operator's existing banner semantics and is complementary.
- Note: the option is session-scoped, so multiple agents sharing one tmux session share attention state — acceptable given the banner is per-session; documented.

### 4. CLI nudge = operator-only send-keys (`muster nudge`)

`muster nudge <alias> [--submit]`:
- Resolves `<alias>` to its `socket_path` + `pane_id`. **Exact alias only** — reject role/broadcast fan-out (a nudge is a deliberate single poke).
- **Prints the resolved target** (`alias → session/pane on <socket>`) so the operator sees what it'll hit.
- Types the literal "📬 check your muster inbox (get_inbox)" into the pane via `tmux -S <socket> send-keys -t <pane> -l <text>`.
- **Auto-submits by default, agent-aware** (`--no-submit` forces type-only): muster knows the target's `model_type`, so it submits wherever that genuinely works and falls back honestly where it doesn't.
  - `model_type=claude` → send Enter after the text (confirmed to submit).
  - `model_type=codex` → auto-submit does **not** work today (`send-keys` Enter is a no-op in Codex's composer). Type the text and print "Codex won't auto-submit — press Enter in that pane." **Build-time investigation:** test whether typing the keys (not `-l` bracketed-paste) or `load-buffer`+`paste-buffer`+Enter makes Codex actually submit; if reliable, enable auto-submit for Codex too and drop the fallback.
  - unknown/other `model_type` → attempt Enter, best-effort.
- If the pane is gone/stale, return a clear best-effort error (don't silently claim success).
- Implemented by a separate `TmuxNudger` (in the CLI layer, not the daemon).

### 5. Config

- `notify.tmux_option` — the tmux user-option muster sets/clears (default `@claude_attn` to reuse the operator's existing banner out of the box; configurable so muster stays generic).
- `notify.timeout_ms` — per-notify subprocess timeout (default e.g. 500ms).
- Config lives in muster's config (extend existing config handling; if none, a small `internal/config`).

## Components / files

```
internal/store/models.go        # +SessionID on Agent
internal/store/schema.sql       # +session_id column on agents
internal/store/agents.go        # persist/read session_id
internal/mcpserver/tools_registry.go  # register_agent resolves session_id/name via socket-aware display-message
internal/mcpserver/tools_messages.go  # get_inbox → clear notify option for the agent's session
internal/wake/wake.go           # Waker → Notifier + TmuxNotifier (set/clear option, timeout, injectable runner)
internal/daemon/daemon.go       # wakeForThread → notifyForThread (Notifier, best-effort/bounded, no send-keys); Serve takes Notifier
internal/nudge/nudge.go         # NEW: TmuxNudger (send-keys, --submit) — CLI-only
internal/humancli/humancli.go   # NEW `nudge` subcommand
cmd/muster/main.go              # runServe wires TmuxNotifier; nudge routed via humancli
```
Existing `Serve(sock, store, waker)` callers (daemon_test, mcpserver startTestDaemon) update to the Notifier signature (nil = no-op).

## Testing

- **Notifier:** injectable runner asserts `Notify` issues `set-option -t <session-id> @claude_attn 1` (socket-aware) + refresh, and **never** `send-keys`; timeout path; nil-notifier no-op.
- **notifyForThread:** fake Notifier asserts the right recipients' *session-ids* are notified (agent/role/broadcast, actor excluded); no PaneID required.
- **Flag clear:** `get_inbox` for an alias issues `set-option -u @claude_attn` on that agent's session.
- **Nudge:** exact-alias only (role/broadcast rejected); prints resolved target; type-only by default, Enter only with `--submit`; stale-pane returns an error.
- **Registration:** `register_agent` populates `session_id` (mock the `display-message` runner).
- `just verify` green (fmt, lint, `go test -race`, build), `TMPDIR=/tmp` on macOS.
- Live check (manual, recorded): notify lights the banner on an unfocused agent tab; `muster nudge` delivers to a pane.

## Out of scope (deferred)

- **Autonomous idle-wake via Codex app-server / `--remote`** — the only path to true no-human-in-loop Codex wake, but needs a launch change. Revisit as opt-in later.
- Claude Code Stop-hook auto-pull — a separate optional bolt-on, not this milestone.
- Fully-async notification queue with drop/coalesce — start with bounded/timeout; upgrade only if fan-out latency bites.
- Auto composer-empty detection for nudge — rejected as unreliable; operator judgment is the guard.

## Resolved decisions

1. **`nudge` default = auto-submit, agent-aware** (`--no-submit` forces type-only). Submits for Claude; Codex falls back to type-only + operator prompt, with a build-time attempt to make Codex submit. (See §4.)
2. **`get_inbox` clears the flag unconditionally** — any read acknowledges; no read-cursor. (See §3.)
