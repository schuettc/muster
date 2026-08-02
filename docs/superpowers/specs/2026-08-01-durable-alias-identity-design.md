# Durable alias identity — design

**Date:** 2026-08-01
**Status:** proposed
**Depends on:** label-first identity (v0.7.5), fyi replies / paneless agents (v0.7.7)

## Problem

An agent's inbox, threads, and read-state are keyed by **alias**, which is the
right durable identity: it belongs to the *conversation* (the Claude Code /
Codex / Cursor session), not to the terminal it happens to be running in. But
two conventions currently bind the alias to the tmux session instead:

1. The CLI's alias default is the tmux session name
   (`internal/humancli/identity.go`: explicit arg → `$MUSTER_ALIAS` → tmux
   session name), and
2. the coordination skill *defines* alias as "a peer's tmux session name."

The failure mode: an operator closes a tmux session, opens a new one, and
resumes the same agent conversation (`claude --resume`, Codex resume, Cursor
resume). The resumed session — the one that actually holds the context the
inbox belongs to — arrives in a tmux session with a different name. If
anything leans on the tmux-name default, the agent registers under a fresh
alias and its inbox, threads, and unread cursor are orphaned under the old
one.

## Decision

Make the **alias-first contract** explicit and self-reinforcing, with **no
per-harness glue**. The durable identity anchor is the alias itself, reclaimed
by re-registration; the mechanism every resumable harness already carries
across a tmux swap is the conversation transcript, which contains "I
registered as `backend-2`."

Explicitly rejected: anchoring identity on a harness session ID as the
*required* identity mechanism (Codex would require scraping
`~/.codex/sessions`; Cursor exposes nothing dependable). That path builds a
first-class experience for one harness and a permanently degraded one for the
other two, and adds an adapter per future harness.

What IS in scope (change 4): **hook-layer reclaim glue** where a harness hands
us its session ID for free in a hook payload — Claude Code does, in every
hook event. The glue is strictly additive: the contract (changes 1–3) is the
identity mechanism on every harness; the glue automates it where the hooks
allow. The daemon stays harness-agnostic — it stores the harness session ID
as an opaque string it never interprets; only the hook layer
(`internal/humancli`) reads or writes it.

## What already works (no changes)

- `store.RegisterAgent` upserts by alias and always resets `departed=0`, so
  re-registering a tombstoned alias revives it. Read-state
  (`last_read_entry_id`/`last_read_at`) is untouched by both branches, so the
  unread cursor survives the roundtrip (`internal/store/agents.go`).
- The tmux tuple (`socket_path`, `session_id`, `session_created`, `pane_id`,
  `session_name`) is re-captured wholesale on every register — it is liveness
  and wake plumbing, stamped per registration, never identity.
- The MCP register path's pane guard (`tools_registry.go`) checks the *current
  pane's* registration, so a resumed session in a brand-new pane re-registers
  its old alias without conflict.
- Ghost reaping and `muster gc` tombstone the old registration when the old
  tmux session dies. Tombstoning is a soft delete precisely so revival works;
  no change to those semantics.

## Changes

### 1. `register_agent` reports what happened (daemon + MCP + CLI)

Today the register response is a bare "registered `<alias>`" regardless of
whether the row was new, refreshed, or revived. `handleRegisterAgent` already
reads the pre-mutation row (`old`, `hadOld`) for reconciliation, so it can
classify the outcome without an extra query:

- **new** — no prior row: `registered <alias>`.
- **refreshed** — prior row, not departed: `re-registered <alias>` (tuple
  refreshed).
- **revived** — prior row with `departed=1`: `reconnected as <alias> — revived
  departed registration`.

On the *refreshed* and *revived* outcomes, the response also carries the
agent's pending-inbox count, computed by the same query `get_inbox` uses. This
closes the resume loop in one call: a resumed session re-registers and is told
"you have 3 unread thread(s)" — a direct prompt to run `get_inbox` — instead
of discovering its backlog only at the next Stop-hook wake. The daemon returns
`{outcome, unread}` structured fields; the MCP tool folds them into
`OKOut.Detail`; the CLI prints them after its existing "registered …" line.

A revival also logs a journal event (existing `logEvent` path, the register
kind with a "revived" detail) so `muster watch` and station surfaces show a
returning agent as *returned*, not as a ghost that got lucky.

### 2. The skill teaches the contract (`.claude/skills/muster-coordination`)

Three edits, no new sections beyond one:

- **"Register once, at the start" → "Register at the start — and on resume."**
  The alias is the session's durable identity on the bus: it survives tmux
  sessions, terminal restarts, and machine reboots, because the inbox and
  read-state live under it in the daemon's store. If this conversation
  previously registered an alias, re-register **the same alias** when the
  session resumes — even (especially) if the tmux session around it is new.
  The register response says whether the identity was revived and how much
  mail is waiting.
- **Addressing:** redefine the alias bullet from "a peer's tmux session name"
  to "a peer's durable bus identity, globally unique — often *seeded from* the
  tmux session name at first registration, but owned by the conversation
  thereafter."
- **Alias choice guidance:** pick an alias that names the *work*, not the
  terminal (the default tmux-name seed is fine when the session name is
  already meaningful).

### 3. CLI default: keep the seed, fix the vocabulary

The `muster register` fallback chain (explicit arg → `$MUSTER_ALIAS` → tmux
session name) **stays**. It is a *seed* for first registration — operators
name their tmux sessions meaningfully, and the human CLI's identity defaults
elsewhere (hooks resolve aliases by tuple, not name, so a divergent alias
already works). What changes is the documentation: README and help text stop
*defining* the alias as the tmux session name and instead describe the
seed-then-own model above. No behavior change, so nothing existing breaks.

### 4. Hook-layer reclaim: register-on-resume, automated where possible

Register-on-resume is the biggest driver of this design, so where the harness
gives the hook layer enough signal, the reclaim happens without the model
having to remember anything. Verified facts this leans on (Claude Code docs +
live verification by the dotfiles session, thread 149, 2026-08-01):

- Resume (`--resume`, `--continue`, `/resume`) reopens the session under the
  **same `session_id`**; only fork (`--fork-session`, `/branch`) mints a new
  one. So the harness session ID is a stable key for one conversation.
- Every hook payload (SessionStart, Stop, SessionEnd) carries `session_id`.
  SessionStart additionally carries `source`
  (`startup|resume|clear|compact|fork`); SessionEnd carries `reason`, which is
  `"resume"` when the session was resumed *somewhere else*.
- SessionStart hook **stdout is added to the session's context** — the hook
  can tell the resumed agent who it is directly.
- Hooks run in a **stripped environment** — `$TMUX`/`$TMUX_PANE` are unset —
  but the *session* process itself does see them on a pane launch (verified
  live on claude 2.1.220; the dotfiles wrapper's "daemon-forks sessions"
  comment is stale). A hook therefore cannot read its pane from env, but it
  CAN locate it by walking its own process ancestry (hook → claude → pane
  shell) until a PID matches a tmux pane's `#{pane_pid}` — the technique
  dotfiles ships today in `config/claude/claude-notify.sh`. The walk requires
  the hook to run synchronously (async reparents the hook and breaks the
  chain) and fails safe: no match → behave paneless, never guess.
- Rejected: a pane-side hint keyed by cwd (wrapper writes tuple to kv at
  resume launch, hook consumes it). Two sessions in one directory is normal
  on this machine (live counterexample: bettor-help-workspace-4 and -6 in the
  same checkout), so a cwd key mis-delivers. No cwd fallbacks anywhere in
  this design.

The pane-side wrapper (dotfiles `04-aliases.zsh`) stays exactly as it is: on
a fresh tmux launch it mints the UUID, pre-registers the tmux session's name
with `--harness-session`, and passes `--session-id` — so fresh launches have
the harness link from birth. It deliberately skips `--resume`/`--continue`:
on an interactive resume the UUID is unknowable before exec, which is exactly
why reclaim is the hook layer's job.

**Ancestry capture: one implementation per product, aligned by contract.**
Within muster, the walk joins `internal/tmuxenv` (the capture owner, per the
one-canonical-module rule) — but that rule is intra-repo, not cross-product.
Dotfiles keeps its own implementation (extracted from `claude-notify.sh`) so
the two products stay independently installable: neither may become a hard
runtime dependency of the other (settled on thread 149). What is shared is
the **contract**, four invariants both implementations must hold:

1. Walk the parent chain outward from the hook process.
2. Match PIDs against `list-panes -aF '#{pane_pid} …'` across the
   `proj-*` sockets — per-project tmux servers mean a single-socket lookup
   silently misses.
3. First match wins; stop there.
4. Fail safe: no match → empty result and paneless behavior — never a
   broadcast to all panes, never a cwd-derived guess (two sessions in one
   directory is normal).

Muster additionally exposes the walk as a CLI verb, working name
**`muster whereami`** (tuple on stdout, `--json` for structure, empty +
nonzero exit when no pane matches) — for muster's own hooks and as an
operator convenience, NOT as a dependency dotfiles consumes.

Mechanism, three parts, all in the hook layer:

**Stamping.** A new opaque `harness_session_id` column on the agent row,
written via `register_agent` (new optional field) and a small
`stamp_harness_session` op. Two writers: (a) `muster hook SessionStart` —
which today ignores stdin — now decodes the payload and passes the harness
session ID through its auto-register; (b) the Stop hook, which already
resolves the session's full alias list by tuple (`session_aliases`), stamps
any owned alias that lacks the ID. Path (b) is what covers custom aliases
registered through the MCP tool mid-session — the MCP server never sees the
harness session ID, and doesn't need to; the next Stop event repairs the
mapping. Stamp only when unset or changed: no write traffic in the steady
state.

**Release.** Already shipped, no change: SessionEnd (including
`reason:"resume"` — the old side of a resume) tombstones every alias the
dying pane owns via the existing ownership predicate (`hookSessionEnd`,
v0.7.4). The old registration frees the alias at the exact moment the new
side wants it.

**Reclaim.** `muster hook SessionStart` with `source:"resume"`: resolve the
CURRENT pane via env capture when present, else the tmuxenv ancestry walk
(hooks are env-stripped, above); look up aliases by the payload's
`session_id` (`harnessOwnedRows` — the client-side lookup shipped with
paneless agents; no new daemon op needed), and for each one re-register it
with the *current* tmux tuple. That's the existing revive
path; change 1's outcome/unread response feeds the last step: the hook prints
a plain-text summary — "reconnected as 'backend-2' (revived) — 3 unread
thread(s); call get_inbox" — which SessionStart injects into the resumed
conversation's context. The agent wakes up knowing who it is and what's
waiting, with zero reliance on transcript memory. Safety: before stealing an
alias, apply the same client-side liveness check the hooks already use — a
row that is live under a *different* tuple and a different harness session ID
is a real collision, so skip it and say so in the hook output rather than
clobber (`hookMayClaimIdentity`'s cross-session takeover arm already encodes
the precedent).

Harness matrix:

| Harness | Stamp | Release | Reclaim |
|---|---|---|---|
| Claude Code | SessionStart + Stop payloads | SessionEnd (incl. `reason:resume`) | SessionStart `source:resume`, output injected into context |
| Cursor | verify: hook payload fields undocumented — build only what its payload carries | ditto | ditto, else contract |
| Codex | none (no hook payload with a session ID) | first-turn register convention | contract only: re-register by transcript memory |

Codex losing the glue is acceptable by design — the contract still works
there, exactly as it does everywhere. The Cursor column is an empirical
question (its harness support landed in v0.7.7; its hook payload fields are
undocumented) — resolve it during implementation against the real
`cursor-agent`, and if its payloads carry no stable session ID, Cursor rides
the contract like Codex. Never scrape a harness's session store from a hook.

## Non-goals

- Harness session IDs as a *required* identity mechanism, or any scraping of
  a harness's session store (`~/.codex/sessions`, Cursor internals). The only
  sanctioned source is a hook payload that hands the ID over directly.
- Daemon-side guessing: the daemon never decides a new registration "is" an
  old agent. Reclaim is always an explicit re-register by alias — the hook
  layer merely automates issuing it.
- Changes to ghost reaping, gc, tombstone semantics, or the wake model.

## Test surface

- Daemon: register outcome classification (new / refreshed / revived) and the
  unread count on refresh/revive; revival journal event emitted.
- MCP: `OKOut.Detail` carries outcome + unread on revive.
- CLI: register output prints outcome + unread; existing default-alias
  precedence tests unchanged (behavior is unchanged).
- Store: (existing) revival preserves read-state — already covered; extend if
  the outcome classification moves any logic into the store. New:
  `harness_session_id` column round-trip + `harness_aliases` lookup.
- Hooks: SessionStart decodes payload (tolerant of empty/invalid stdin, as
  hookStop already is); `source:resume` reclaims stamped aliases and prints
  the context summary; `source:startup` stamps through register; Stop stamps
  owned aliases lacking the ID; collision (live different-tuple,
  different-harness-ID row) is skipped with a message, never clobbered.
- Live verification during implementation: real `claude --resume` into a
  fresh tmux session end-to-end (old side releases via SessionEnd
  reason:resume, new side reclaims, unread summary lands in context), and an
  empirical check of what `cursor-agent` hook payloads actually carry.

## Implementation note

Cut the feature worktree from **origin/dev** — the primary clone's dev lags
(known trap; v0.7.7's fyi/paneless/Cursor work is on origin, and this
document's file/line references were read against the stale local checkout;
re-verify against origin/dev when building, especially `hook.go`, which
gained a Cursor loop guard in the #78 back-merge).
