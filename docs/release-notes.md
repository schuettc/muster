# Release notes

There is no CHANGELOG.md in this repository yet. This file holds the notes for releases where the change is operator-visible enough to need explaining rather than just listing. Newest first.

## 0.19.0 — `muster setup`

**`muster setup`** configures your coding agents for you. It detects the agents installed on the machine — **Claude Code, Codex, and Cursor** — and, for each, registers muster's MCP server and installs the session hooks (SessionStart/Stop/SessionEnd) via idempotent JSON/TOML deep-merge, so there is no config file to hand-edit. Flags: `--tmux` also renders the 📬 mailbox badge in `~/.tmux.conf`; `--dry-run` previews every change; `--no-hooks` registers the MCP server only; `--agent` limits to specific agents; `--force` applies even when kempt is present. Re-running is safe — every write is a merge that preserves existing content.

When **kempt** is installed, `muster setup` defers to it: it prints the declarative `[packages.muster]` block to add to your `kempt.toml` rather than editing files. **`muster setup --print kempt`** emits that same canonical package on demand — it is the single source both the imperative and declarative paths share, so they never drift.

## 0.18.0 — Business Source License

muster adopts the **Business Source License 1.1**. It is **free for any organization with fewer than 25 employees** — every feature, no key, no gating. At 25+ employees an honor-based **one-time** commercial purchase applies, priced by company size; there is no feature gating, no license key, and no phone-home, so your receipt is the proof. **Each release converts to Apache-2.0 three years after it ships.** Versions **before 0.18.0 stay MIT in perpetuity** — BUSL applies from 0.18.0 onward. Deploying the hosted backend into your own AWS account is permitted self-hosting, not a commercial hosted service.

**`muster commands [--json]`** lists the family CLI surface for discovery. Internally, `internal/humancli` was renamed to `internal/cli`.

## 0.17.0 — broadcast storm guard

**Broadcasts now reply-to-originator and pass a confirm gate.** A first broadcast returns its blast radius and sends nothing; you re-send with `confirm` to actually deliver it, and replies route back to the originator rather than fanning out.

## 0.16.0 — coordination discipline

**Standing orders replace the live broadcast backlog.** A standing order is a keyed, retractable per-project convention; new sessions no longer inherit the live broadcast backlog and instead pick up the standing orders that still apply. **Wake discipline** codifies the etiquette: polite broadcasts, fast direct sends, explicit break-glass.

**`muster update`** self-updates the binary in place, so the deploy machine and devices no longer re-run the installer. **`muster status --json`** reports side-effect-free per-alias inbox counts (the project attention column) without marking anything read. **`MUSTER_HOOK_DISABLE`** guards nested harness subprocesses from firing muster's hooks.

## 0.15.2

**Binaries are now published to muster.tools/dl**, the family download standard. GitHub releases become the durable backing store behind it.

## 0.15.0 — channel mode

**New `muster channel` MCP carrier** (Claude Code `claude/channel`, pi `pi-channels`) pushes a compact envelope — intent, sender, thread, subject, **never the body** — into an idle session, a third wake path alongside the mailbox badge and `muster nudge`. Tunable via **`MUSTER_CHANNEL_INTERVAL`** (default 1s) and **`MUSTER_CHANNEL_MAX_LISTED`** (default 5). Event-scoped guidance rides after a `---` separator; pi rename propagation flows through the native `/name`; and the carrier accepts a harness-neutral **`AGENT_SESSION_ID`**.

## 0.14.0 — one conversation, one identity

This release makes a conversation own exactly one live roster row. Identity resolves by transcript path first, and falls back to the live tmux pane tuple only when no row already carries that transcript path. A harness session id (Claude Code's own session identifier, distinct from muster's alias) is just an attribute on the row that gets re-stamped whenever it changes — a `/login` or any other harness-internal event can change it mid-conversation without starting a new conversation as far as muster is concerned.

**Upgrade order matters.** If you run the optional hosted backend, redeploy it first. Then install the new `muster` binary on each device. Then kill any already-running `muster serve` daemon so it restarts on the new version — the daemon is a lazy unix-socket process, so it does not pick up a new binary on its own; the next command that needs it will start a fresh instance. A 0.14.0 client talking to an old daemon (or an old hosted lambda you haven't redeployed yet) still works during the gap: `muster inbox` and the MCP `get_inbox` tool both decode the old response shape as well as the new one, so you won't see decode errors, but the ownership behavior described below only takes effect once the daemon itself is upgraded.

**`muster inbox <alias>` now marks read only from the owning session.** Reading an alias's inbox moves that alias's unread watermark (and clears its tmux badge) only when the call proves it comes from the session that alias actually belongs to — a live pane in that tmux session, or the same harness conversation. Any other read, including an operator running `muster inbox` on another agent's alias just to look, still returns the threads, but is a peek: nothing is marked read, and the unread count and badge are untouched. This is the behavior change most likely to surprise someone: if you were in the habit of running `muster inbox <alias>` from a different pane to clear an agent's badge on its behalf, that no longer works. The agent itself (or a genuine resume of its conversation) has to read its own mail.

**Mail addressed to a deregistered alias without `become` now drains for nobody.** If an agent's alias is deregistered directly — not retired through `become` into a new name — any mail still addressed to that old alias has no owning session left to prove ownership, so `muster inbox <that-alias>` from any pane comes back as a peek. The mail is still there and still readable, but nothing will mark it read on its own. An operator has to run `muster inbox <alias>` deliberately to look at it, understanding it stays unread until someone with real ownership proof reads it, or until it's cleaned up with `muster deregister`/`gc --purge-agents`. This is a real behavior change from prior releases, where any reader draining that alias would clear it.

**`become` can now reclaim a departed name.** If an alias was retired (departed) and nothing has taken its place, `muster become <that-alias>` (or the MCP `register_agent` tool with `become:true`) can claim it again, carrying over its identity and unread state — where before a departed name was permanently unavailable.
