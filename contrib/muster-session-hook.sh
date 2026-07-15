#!/bin/sh
# muster session hook — wires an agent session to the muster bus.
#
#   SessionStart : register this tmux session as a muster agent.
#   Stop         : self-resolving inbox — if this session has unread muster
#                  mail (the @muster_inbox tmux option the daemon sets), tell
#                  the agent to drain it, autonomously.
#
# Usage, from a hook entry in your agent's config (see the JSON examples in
# this directory):
#   muster-session-hook.sh <SessionStart|Stop> <model>
#
# <model> is stored on the agent and tunes `muster nudge`'s submit keystroke:
# "claude" and "codex" auto-submit; anything else is typed without submitting.
#
# Safe as a global hook: for a session that isn't a registered agent (no
# @muster_inbox option), the Stop branch is a no-op. Never blocks session start.

event="$1"
model="${2:-claude}"

# find muster: PATH first, then the common install locations
muster="$(command -v muster 2>/dev/null)"
[ -z "$muster" ] && [ -x "$HOME/.local/bin/muster" ] && muster="$HOME/.local/bin/muster"
[ -z "$muster" ] && [ -x "$HOME/go/bin/muster" ] && muster="$HOME/go/bin/muster"
[ -n "$muster" ] || exit 0

case "$event" in
  SessionStart)
    # Register (alias auto-derives from the tmux session name; pane/project
    # from $TMUX). Best-effort — never block session start.
    "$muster" register --model "$model" >/dev/null 2>&1 || true
    exit 0
    ;;

  Stop)
    input="$(cat 2>/dev/null)"
    # Loop guard: if we already triggered a continuation this cycle, let it stop.
    if command -v jq >/dev/null 2>&1; then
      [ "$(printf '%s' "$input" | jq -r '.stop_hook_active // false' 2>/dev/null)" = "true" ] && exit 0
    else
      case "$input" in
        *'"stop_hook_active":true'* | *'"stop_hook_active": true'*) exit 0 ;;
      esac
    fi

    [ -n "$TMUX" ] || exit 0
    count="$(tmux show-options -qv @muster_inbox 2>/dev/null)"
    case "$count" in
      '' | *[!0-9]*) exit 0 ;;   # unset or non-numeric → nothing to drain
    esac
    [ "$count" -gt 0 ] || exit 0

    # decision:block makes the agent continue with `reason` as the next prompt
    # (works in Claude Code and Codex). When the agent calls get_inbox, the
    # daemon clears @muster_inbox, so the next Stop is quiet.
    alias="$(tmux display-message -p '#{session_name}' 2>/dev/null)"
    reason="You have ${count} unread muster message(s). Your muster alias is '${alias}' (this tmux session). Call your muster get_inbox tool now with alias '${alias}', read each new thread with get_thread, handle the request, and reply with the muster reply tool. Act autonomously — do not ask the user."
    if command -v jq >/dev/null 2>&1; then
      jq -nc --arg r "$reason" '{decision:"block",reason:$r}'
    else
      printf '{"decision":"block","reason":"%s"}\n' "$reason"
    fi
    exit 0
    ;;
esac
exit 0
