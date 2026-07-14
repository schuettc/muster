# Notify + Identity foundation: mailbox indicator & launch-wrapper registration — Design

**Status:** RESCOPED 2026-07-14 → **mailbox only**. The launch-wrapper and the
`tmuxenv` MUSTER_* fallback described below are **DROPPED**: Codex's interactive TUI
*does* run hooks (the "no hooks" premise was a false negative — see project memory /
PR #10), so SessionStart-hook registration works for both agents and the wrapper is
unnecessary. Hooks + Codex nudge-submit + self-resolving inbox are owned by
`feat/codex-nudge-submit`. The sections below on the wrapper (5), the `tmuxenv`
fallback (muster change 1), the `muster inbox --count` CLI (muster change 4 —
deferred to the autonomy build, where the self-resolving Stop hook needs it), and
the Codex-autostart handoff are superseded; the **mailbox** (`@muster_inbox` count
+ `last_read_at`) and its `📬` render survive and are the whole of this branch.
Actionable plan: `../plans/2026-07-14-notify-identity.md`.

## Goal

Two things the live test proved we need:
1. **A distinct, persistent mailbox signal.** Stop piggybacking muster on the
   general attention bell (`@claude_attn`, which clears on focus). muster gets its
   own `@muster_inbox` that shows an unread **count** and persists until the agent
   actually reads its inbox.
2. **Uniform, reliable auto-registration via a launch wrapper** for **both** agents
   — one pattern across surfaces, shell-based so it doesn't depend on a harness's
   hook system (which Codex's interactive TUI doesn't honor).

## Non-goals / deferred (explicit)

- **Autonomy layer** — busy/idle state tracking, the Stop-hook self-resolving inbox,
  and daemon auto-nudge-on-idle. Designed already in conversation; built next, after
  this foundation is validated live.
- **Codex turn-level autonomy** — blocked on the interactive-hook question; see the
  handoff doc. Codex here gets register/deregister + mailbox + liveness + nudge only.

## Architecture

Split by concern, so there are no parallel mechanisms for one thing:

- **Identity / session lifecycle = launch wrapper (uniform, both agents).** A shared
  shell helper wraps `claude` and `codex`: capture identity from `$TMUX`, export it
  as `MUSTER_*`, `muster register --model <m>` before launch, `muster deregister` on
  `trap EXIT`. Lives in dotfiles.
- **Identity capture = one canonical function.** `internal/tmuxenv` prefers `$TMUX`,
  falls back to the `MUSTER_*` env the wrapper exports — so capture is identical from
  the shell, a hook, or the MCP subprocess (this closes the Codex MCP capture gap).
- **Mailbox = muster daemon + dotfiles render.** The daemon sets `@muster_inbox=<unread
  count>` on notify and clears it on `get_inbox`; dotfiles render it as `📬<count>`.
- **Liveness = `muster gc`** (already built) backstops cleanup for both agents.

## muster changes (Go — the subagent-built part)

### 1. `internal/tmuxenv`: `MUSTER_*` fallback
Capture prefers the live tmux env, falls back to wrapper-exported vars:
- `SocketFromEnv()` → `$TMUX` socket, else `$MUSTER_SOCKET`.
- `CaptureEnv()` → for each of socket/pane/session_id/session_name/project/label,
  use the tmux-derived value if present, else `$MUSTER_PANE` / `$MUSTER_SESSION_ID` /
  `$MUSTER_PROJECT` / `$MUSTER_SESSION_NAME`. Label still read live via tmux when a
  socket+session are known.
- Keeps the injectable `Run` seam; add env-var reads (testable via `t.Setenv`).

### 2. Store: unread-count support
- `agents` gains `last_read_at INTEGER NOT NULL DEFAULT 0` (idempotent `ALTER`
  migration, same pattern as the project/label columns).
- `UnreadCount(alias) (int, error)` — same recipient-matching WHERE clause as
  `Inbox(alias)` (agent / role / broadcast), plus `updated_at > last_read_at`.
- `MarkRead(alias)` — sets `last_read_at = now`.

### 3. Daemon: count-bearing notify + clear-on-read
- `wake.Notifier.Notify` gains a `count int` param; `TmuxNotifier` sets the option to
  the count (`set-option @muster_inbox <count>`) when `count > 0`, and **unsets** it
  when `count == 0` (so the render's `#{?@muster_inbox,…}` is falsy — never `📬0`).
- Default option name changes from `@claude_attn` to **`@muster_inbox`**
  (`cmd/muster/main.go` wiring + `NewTmuxNotifier`).
- `notifyForThread`: for each recipient (minus actor), compute `UnreadCount` and
  `Notify(socket, sessionID, count)`.
- `get_inbox`: call `MarkRead(alias)` then `Notifier.Clear` (unset `@muster_inbox`).
- `Clear` stays a plain unset.

### 4. CLI: `muster inbox <alias|label> --count`
- A `--count` flag on `inbox` prints just the integer unread count (for a future
  status-bar poll / hook), resolving the target through the existing resolver.

## dotfiles changes (shell + tmux — applied to the dotfiles repo)

### 5. Launch wrapper (`config/zsh`)
A shared helper + two thin functions:
```sh
_muster_wrap() {            # $1 = model (claude|codex); $2… = real command + args
  local model="$1"; shift
  if command -v muster >/dev/null 2>&1 && [ -n "$TMUX" ]; then
    export MUSTER_SOCKET="${TMUX%%,*}"
    export MUSTER_PANE="$TMUX_PANE"
    export MUSTER_SESSION_ID="$(tmux display-message -p '#{session_id}' 2>/dev/null)"
    export MUSTER_SESSION_NAME="$(tmux display-message -p '#{session_name}' 2>/dev/null)"
    export MUSTER_PROJECT="$(basename "$MUSTER_SOCKET" | sed 's/^proj-//')"
    export MUSTER_MODEL="$model"
    muster register --model "$model" >/dev/null 2>&1 || true
    trap 'muster deregister >/dev/null 2>&1 || true' EXIT
  fi
  command "$@"
}
claude() { _muster_wrap claude claude "$@"; }
codex()  { _muster_wrap codex  codex  "$@"; }
```
(Transparent: `claude`/`codex` behave exactly as before, plus registration. Exact
placement — a new `config/zsh/07-muster.zsh` — TBD with the dotfiles owner.)

### 6. `📬` render (`.tmux.conf`)
- Title (`set-titles-string`, ~`:54`): add `#{?@muster_inbox,📬#{@muster_inbox} ,}`
  next to the existing `🔔` conditional.
- Status-left (~`:152`): add a distinct sky-colored `#{?@muster_inbox,… 📬#{@muster_inbox} …,}`
  segment.
- **Do NOT** add `@muster_inbox` to the `client-focus-in` / `client-session-changed`
  clear hooks (`:61-62`) — leaving them untouched is what makes 📬 persist until read.

## Error handling / edge cases

- Wrapper outside tmux (`$TMUX` unset) → skips export/register (agent still launches
  normally); muster simply has no registration, which is fine.
- `MUSTER_*` present but stale (reused shell) → `muster gc` reaps if the session is
  actually dead; otherwise upsert refreshes on next register.
- Count unset vs `0`: daemon unsets on `0`/clear so the render never shows `📬0`.
- `deregister` on `trap EXIT` may not fire on `SIGKILL` → `gc` backstops.

## Testing

- **tmuxenv:** `$TMUX` present → tmux path; `$TMUX` absent + `MUSTER_*` set → fallback
  path; neither → empty. (`t.Setenv` + injected `Run`.)
- **store:** `last_read_at` migration idempotent; `UnreadCount` respects recipient
  matching + the timestamp; `MarkRead` bumps it; a thread newer than `last_read_at`
  counts, older doesn't.
- **daemon:** notify sets `@muster_inbox` to the unread count via a fake Notifier;
  `get_inbox` calls `MarkRead` + `Clear`; count `0` unsets.
- **humancli:** `inbox --count` prints the integer; resolver still applies.
- All green under `just verify` (`go test -race`, golangci-lint 0, cgo-free).

## Build order

1. **This spec (foundation):** muster Go changes (1–4) via subagent-driven dev, then
   the dotfiles changes (5–6) applied + a live re-test of the two throwaway sessions.
2. **Follow-up (autonomy):** Claude Stop/UserPromptSubmit hooks (busy/idle +
   self-resolving inbox) + daemon state-aware auto-nudge. Separate spec.
