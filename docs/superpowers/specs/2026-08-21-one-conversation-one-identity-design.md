# One conversation, one identity — transcript-keyed rows, owned reads, reclaimable names

**Date:** 2026-08-21
**Status:** approved (operator, 2026-08-21)
**Supersedes in part:** `2026-08-01-durable-alias-identity-design.md` (harness session ID as the reclaim key), `2026-08-05-conversation-identity-naming-design.md` §3 (extends "the transcript is the durable name" to "the transcript is the durable identity").
**Origin:** bus thread #281 (reporter `personal-nfl-coordinator`, 2026-08-21) and the data-lake incident diagnosed the same night.

## 1. Problem

The roster keys a conversation's identity on the harness `session_id`. Claude Code can change that ID for a running conversation (observed: `/login` — transcript `43bc7a70…jsonl` carries 240 records stamped `session_id: 7676d357…`). The SessionStart hook sees the new ID in its payload; the MCP server keeps the old one from `CLAUDE_CODE_SESSION_ID` in its environment. Each registers "its" conversation, and because the MCP's pane check skips departed rows while the hook's reclaim is keyed on harness ID, the two leapfrog: one tombstones the seed, the other revives it, and the conversation ends up with two live rows on one pane, two read watermarks, and two names.

Everything reported in #281 follows from that one defect:

- `session_aliases` is tuple-keyed and includes departed rows on purpose, so a conversation resumed into a tmux session that hosted a different conversation is told the other conversation's aliases are its own, and can read that inbox.
- `get_inbox` / `MarkRead` / `muster inbox <alias>` accept any alias with no ownership proof, so a sweep across aliases clears other sessions' badges silently.
- Two watermarks per conversation manufacture phantom badges: a reply sent from one alias leaves the other's watermark behind.
- `become` refuses any existing row, departed or not, so a conversation whose custom-title drifted (data-lake's `git-branch-handoff-flags/data-lake`, minted while the session lived in a worktree) cannot take its correct name back without `gc --purge-agents`, which destroys every departed row.
- A stored socket path of `/tmp/tmux-501/…` never matches the live `/private/tmp/tmux-501/…`, so `DepartStaleSiblings` missed a pre-reboot ghost (`personal-bettor-help-workspace/coordinator`) that stayed "live" with 12 unread for nine days.

## 2. Decision

A conversation has exactly one live roster row. The keys that find it, strongest first:

1. **`transcript_path`** — the harness transcript file. Claude Code never changes it for a conversation; `/login`, resume, restart, and pane moves all keep it. New column on `agents`.
2. **The live pane tuple** `(device_id, socket_path, session_id, session_created, pane_id)` — a pane hosts one harness process, so a live row on the caller's pane *is* the caller's conversation.
3. **`harness_session_id`** — fallback only, for rows that carry neither (Codex, paneless sessions without a transcript). It is re-stamped whenever a stronger key finds the row; it never keys a lookup when a stronger key is present.

Extra aliases for one conversation exist only as a **become-chain**: the seed is `departed=1, superseded_by=<successor>`. Lineage (`SessionAliasLineage`, `SessionUnread`) walks chains; it no longer picks up tombstones that are not part of a chain.

Reads are **owned**: the watermark moves only when the caller proves it owns the alias. Without proof the read is a peek.

`become` may **reclaim a departed name**.

Socket paths are **canonical** (`filepath.EvalSymlinks`) at capture and backfilled once in the store.

## 3. Mechanism

### 3.1 Store (`store.API`, both backends)

- `Agent.TranscriptPath string`. SQLite: `ALTER TABLE agents ADD COLUMN transcript_path TEXT NOT NULL DEFAULT ''`. DynamoDB: attribute `transcript_path`.
- **New op** `FindConversation(deviceID, transcriptPath, socketPath, sessionID string, sessionCreated int64, paneID string) (Agent, bool, error)`: the live (`departed=0`) row whose `transcript_path` equals the argument (non-empty) on that device; else the live row on the full pane tuple (all of socket, session, created≠0, pane non-empty); else not found. Never matches on harness ID — that is the hook's own fallback (§3.3).
- `RegisterAgent` no longer resets `superseded_by` to `''` on conflict. A become-retired seed that is re-registered by alias stays marked as superseded; that reset is what let a retired seed return as a live sibling. (`departed` is still reset to 0 — a revival is still a revival.)
- `Become(from, to)`: if `to` exists and is departed, the tombstone is deleted inside the same transaction before the clone is inserted. `ErrBecomeToExists` remains for a live `to`. Mail addressed to `to` in existing threads is addressed by alias string, so the reclaimer inherits it; the deleted tombstone's watermark is not carried — the clone brings `from`'s read-state, as today.
- `SessionAliasLineage` / `SessionUnread` base case gains `AND (departed = 0 OR superseded_by != '')`. A tombstone with no successor on the caller's tuple is a previous conversation's row, not the caller's.
- `SetHarnessSessionID` is extended to `StampHarness(alias, harnessSessionID, transcriptPath string)`: stamps both; an empty argument leaves that field alone.
- `DepartStaleSiblings`: unchanged.
- `NormalizeSocketPaths()` — one-time backfill in SQLite `migrate`: for every row whose `socket_path` resolves via `filepath.EvalSymlinks` to a different string, rewrite it. Idempotent; rows whose path no longer exists are left alone. DynamoDB gets no backfill — hosted rows are written by devices, which normalize at capture from this release on.

Conformance tests (`internal/storetest`) for every item above, so the two backends cannot drift.

### 3.2 Daemon

- `register_agent` accepts `transcript_path`. Before the upsert it calls `FindConversation` with the incoming row's keys. Outcomes:
  - *not found* → today's path (`new` / `revived`, then `DepartStaleSiblings`).
  - *found, same alias* → today's `refreshed` / `revived`.
  - *found, different alias* → **`adopted`**: no insert. `StampHarness` + tuple refresh on the existing row; response `{outcome: "adopted", alias: <existing>, unread: N}`. The caller learns the name it actually has. Renaming stays `become`.
- `stamp_harness_session` accepts `transcript_path` too.
- `get_inbox` accepts an optional **caller proof**: `caller_socket_path`, `caller_session_id`, `caller_session_created`, `caller_pane_id`, `caller_device_id`, `caller_harness_session_id`. The alias is *owned* when it is in `SessionAliasLineage(caller tuple)` or (paneless) its row's `harness_session_id` equals the caller's. Owned → `Inbox` + `MarkRead` + badge + `read` event, as today. Not owned (or no proof) → `Inbox` only; response carries `marked_read: false`; a `peek` event is journaled so a sweep leaves an artifact. `get_thread` is unchanged (already side-effect-free).
- `session_aliases` / `session_unread`: unchanged signatures; the lineage change in §3.1 is what narrows them.

### 3.3 Hooks (`internal/humancli/hook.go`, `paneless.go`)

- `harnessOwnedRows(h)` becomes `conversationRows(h)`: rows whose `transcript_path == h.TranscriptPath` (when non-empty), else rows whose `harness_session_id == h.SessionID` or paneless tuple matches. A row found by transcript under a different harness ID is the `/login` case and is reclaimed normally; the daemon re-stamps.
- `reclaimRow` / `reviveRow` / the seed register all pass `transcript_path`.
- The seed-from-tmux-name fallback runs only when `conversationRows` is empty; the daemon's adopt is the backstop.
- `stampHarnessLinks` stamps transcript path alongside the harness ID, same pane-ownership predicate.
- The Stop hook's "your aliases are …" string is produced by `session_aliases` and therefore no longer lists other conversations' tombstones.

### 3.4 MCP server (`internal/mcpserver`)

- `paneRegistration` follows `superseded_by` chains: a departed row on the pane with a successor resolves to the successor; a departed row with no successor is ignored as today.
- `register_agent` passes `transcript_path` when the harness env provides one (`harnessenv.FromEnv` gains it if Claude Code exposes it; otherwise empty and the pane tuple carries identity). On `outcome: "adopted"` the tool reports "you are already '<alias>' on this pane — use that alias, or pass become:true to claim '<requested>'".
- `get_inbox` passes the caller proof from `tmuxenv.CaptureEnv()` + `harnessenv.FromEnv()`. The tool result says when a read was a peek: "peek only — '<alias>' is not this session's; its unread state is unchanged".

### 3.5 Human CLI

- `muster inbox <alias>` / `muster tasks <alias>` pass the caller proof from the pane the command runs in. When the daemon answers `marked_read: false` the table is printed followed by one line: `peek only: '<alias>' is not this pane's alias — unread state unchanged`. No flag.
- `muster become <name>` over a departed name succeeds; the output says `reclaimed departed name '<name>'`.
- `muster help inbox` / `help become` updated.

### 3.6 tmuxenv

- `CaptureEnv` canonicalizes `SocketPath` with `filepath.EvalSymlinks` (falls back to the raw value if resolution fails). This is the one canonical capture path, so every client gets it.

### 3.7 Dotfiles (cross-repo, not in this change)

`~/dotfiles/bin/tmux-session-rename.sh` re-attaches the current `#S` project half to a bare work name. When the session lives in a worktree-named tmux session, the minted name carries the worktree as project (`git-branch-handoff-flags/data-lake`). That is the dotfiles' contract to fix (derive the project half from `proj`'s project, not from `#S`); muster only needs to let the operator take the right name back (§3.1 `Become`). Recorded here so the cause is not re-diagnosed.

## 4. Flows

**`/login` mid-conversation.** Hook SessionStart fires with new `session_id`, same `transcript_path`. `conversationRows` finds the row by transcript → `reclaimRow` → daemon `FindConversation` hits by transcript → `refreshed`, harness ID re-stamped. MCP server (still holding the old env ID) calls `register_agent` → `FindConversation` hits by pane → `adopted`. One row.

**Resume into a fresh tmux session after reboot.** Hook finds the row by transcript, `IsSessionAlive` on the old tuple is false → reclaim onto the new tuple. MCP register → adopted. `DepartStaleSiblings` reaps any recycled-`$N` ghost.

**Second conversation started in a pane that hosted another (new `claude` in the same pane).** Different transcript; `FindConversation` by transcript misses, by pane hits the *old* conversation's live row. The old conversation's SessionEnd normally tombstoned it first (then `FindConversation` misses and a new row is born). If SessionEnd never ran (crash), the pane match adopts the dead conversation's row — same outcome as today's revive-by-tuple, and the next hook with the old transcript cannot reclaim it while the pane is alive (`IsSessionAlive` guard). Accepted.

**Operator sweeps `muster inbox` across seven aliases.** Each call from a pane that does not own the alias is a peek; seven `peek` events land in the journal; no watermark moves.

**data-lake takes its name back.** `muster gc` tombstoned nothing it needed; operator runs `/rename bettor-help-workspace/data-lake` (or `muster become`); `Become` deletes the departed seed row of that name and clones the live identity onto it; old mail addressed to the name is visible under it.

## 5. Edge cases

- **No transcript and no pane** (paneless Codex): identity is the paneless tuple `("", harness UUID)`, unchanged.
- **Transcript path differs across devices** for the same conversation: impossible — a transcript is a local file; the device dimension is in every key.
- **Two agent panes in one tmux session** (teammates): pane_id is in the tuple; separate conversations stay separate. Teammate refusal (2026-08-06 spec) is untouched.
- **Caller proof with `session_created = 0`**: lineage matches nothing → peek. Same "zero is absence of proof" rule as every other tuple surface.
- **A chain seed whose successor is itself departed and unsuperseded** (conversation ended): lineage still returns the chain for the tuple; `gc` tombstones nothing new; behaviour as today.

## 6. Non-goals

- Changing the alias primary key or the device-relative display layer (2026-08-10 spec).
- A targeted `muster purge <alias>`; `Become` over a departed name covers the need that surfaced.
- Fixing the dotfiles rename script (§3.7) — separate repo, separate change.
- A `--json` flag on `muster inbox` (requested in #281 §5); worthwhile, separate.

## 7. Testing

- `storetest`: `FindConversation` (by transcript, by pane, precedence, device scoping, departed excluded); `RegisterAgent` preserves `superseded_by`; `Become` over departed `to` succeeds and over live `to` still fails; lineage excludes unsuperseded tombstones; `StampHarness` partial stamps; `NormalizeSocketPaths` idempotence (SQLite only).
- `daemon`: `register_agent` adopt path (same transcript/different alias, same pane/different alias); `get_inbox` owned vs peek (watermark moves only when owned; `peek` event journaled; `marked_read` in response).
- `humancli`: SessionStart reclaim by transcript under a changed harness ID; `muster inbox` prints the peek line; `become` reclaim message.
- `mcpserver`: `paneRegistration` follows a chain; `get_inbox` passes proof; adopt message.
- `tmuxenv`: `EvalSymlinks` canonicalization with a symlinked temp dir.
- Gates: `just verify` and `just verify-dynamo`.
