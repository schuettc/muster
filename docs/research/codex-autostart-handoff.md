# Handoff: deeper research into Codex auto-start / hook mechanism

_Written 2026-07-14. Portable handoff for investigating **elsewhere** how to get autonomous
muster integration working with the interactive Codex CLI. muster itself does not
need this to ship — the launch-wrapper (below) covers registration — but Codex's
turn-level autonomy (self-resolving inbox, busy/idle state) is blocked on it._

## Goal

Get the same autonomous muster experience for Codex that Claude Code already has:
1. Auto-register on session start / deregister on end.
2. Track busy/idle state (turn start/end) so wake can be state-aware.
3. Self-resolving inbox: when the agent finishes a turn, a hook checks muster for
   pending messages and, if any, makes Codex continue and handle them.

For Claude, all three work via native hooks (verified live). For Codex, only #1 is
solved (via a shell wrapper); #2 and #3 are blocked by the finding below.

## The blocking finding (live-tested 2026-07-14, Codex `codex-cli 0.144.3`)

**The interactive Codex TUI did not run a `SessionStart` hook; `codex exec` did.**

Evidence:
- A `~/.codex/hooks.json` with `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/tmp/…sh"}]}]}}`
  was installed. Restarting the interactive `codex` TUI produced the **trust prompt**
  (so the hook was *read and recognized*) and recorded a `trusted_hash` under
  `[hooks.state."~/.codex/hooks.json:session_start:0:0"]` in `config.toml`.
- After trusting, **two** interactive restarts produced **no** hook output file at all.
- The **same** hook, invoked via `codex exec "…"`, fired immediately (`hook: SessionStart`
  / `hook: SessionStart Completed`), wrote its output file, and — importantly — its
  shell **had `$TMUX`/`$TMUX_PANE`**, so `muster register --model codex` captured the
  project/pane/session/label fully.

So: the hook plumbing, trust, and `$TMUX` inheritance all work — but **the
interactive TUI does not execute the hook** (or executes it in a way that produced
nothing), whereas the non-interactive `exec` path does.

## Hypotheses to test elsewhere

1. **Interactive TUI runs no hooks in this version.** Only `exec` does. → Test by
   installing a `Stop` hook that appends to a file, then in an interactive session
   send one prompt and check whether the file appears on turn-end. If nothing → the
   interactive TUI genuinely skips hooks; check changelog/newer versions.
2. **Interactive hooks run sandboxed.** The hook fires but under a sandbox
   (read-only / workspace-write / no-network) that blocks writing `/tmp` and reaching
   the muster unix socket, so it silently fails. → Test with a hook that writes only
   inside the workspace cwd (allowed by workspace-write) vs `/tmp`; compare. Check
   `sandbox_mode` / `-s` settings and `[projects].trust_level` interactions.
3. **Layer/scope mismatch.** The one *verified-working* example was a **project-level**
   `<repo>/.codex/hooks.json` run under `codex exec`; the interactive test used a
   **user-level** `~/.codex/hooks.json`. → Test a project-level `.codex/hooks.json`
   in an interactive session.
4. **Enable step beyond trust.** `[hooks.state]` recorded only `trusted_hash` (no
   `enabled` field). Check whether interactive requires an explicit enable (e.g. the
   `/hooks` slash command in the TUI) distinct from trust.

## Alternative autonomy path (independent of hooks) — worth evaluating

Codex has an **app-server + remote-control** surface (`codex app-server`,
`codex remote-control`, `codex --remote <addr>`, and a `turn/start` RPC). This was
scoped earlier in the project as the way to *programmatically inject a turn* into a
running Codex — i.e., a true push/wake that does not rely on hooks or send-keys.
It was deferred because it requires launching Codex differently (`--remote`). If
interactive hooks can't be made to work, this is the most promising route to
autonomous Codex wake/inbox. Investigate:
- Can a standing interactive Codex be driven via `remote-control` / `turn/start`
  from an external process (muster) to inject "check your inbox"?
- What launch change does it require, and is it acceptable?

## Known-good facts (don't re-derive)

- Codex hook config: `~/.codex/hooks.json`, `<repo>/.codex/hooks.json`, or `[hooks]`
  in `config.toml`; layers merge; trust is by content-hash in `config.toml
  [hooks.state]`; `--dangerously-bypass-hook-trust` skips the trust check for one
  invocation.
- Hook events present in the binary: SessionStart, UserPromptSubmit, PreToolUse,
  PostToolUse, PermissionRequest, PreCompact, PostCompact, SubagentStart/Stop, Stop.
  **No SessionEnd.**
- Stop hook supports `{"decision":"block","reason":"…"}` → Codex creates a
  continuation prompt from `reason` (verified in `exec`). Same shape as Claude's Stop.
- Hook stdin JSON fields: `session_id`, `transcript_path`, `cwd`, `hook_event_name`,
  `model`, `permission_mode`; turn-scoped adds `turn_id`; `SessionStart` adds
  `source`; `Stop` adds `stop_hook_active`, `last_assistant_message`.
- `codex exec` hook shell inherits `$TMUX`/`$TMUX_PANE` (so shell-based capture works).

## What muster ships in the meantime (so this is not blocking)

A transparent **launch wrapper** (`codex` shell function): `muster register
--model codex` before `command codex "$@"`, `muster deregister` on `trap EXIT`.
Runs in the shell (has `$TMUX`), so identity capture works regardless of the hook
question. Claude uses the same wrapper pattern (plus native hooks for the autonomy
layer). Liveness/`muster gc` backstops cleanup for both. Result: Codex gets
auto-register + mailbox + liveness + nudge (types-only); the self-resolving inbox
and busy/idle state remain pending this research.
