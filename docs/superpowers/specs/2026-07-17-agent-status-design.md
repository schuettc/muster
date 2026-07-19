# Self-published agent status (tier-2 enrichment) — design

Date: 2026-07-17 · Status: REVISED after muster-2 adversarial review (thread
37, 22 findings — all folded or explicitly decided). Build order: after the
station UX settles; hook slice LAST (finding 22).

## Problem

Station shows who exists and what they said, not what they're doing. Wanted,
per session: optional one-line status, last turn end, context/token pressure.
Enrichment is an optional convention (like labels): absent agents show less,
muster stays a lean bus, dotfiles and non-dotfiles identical.

## Store (additive; fresh CREATE + guarded ALTERs in one slice)

```sql
status_text   TEXT    NOT NULL DEFAULT ''   -- explicit, per-alias
status_at     INTEGER NOT NULL DEFAULT 0    -- stamps ONLY status_text changes
turn_ended_at INTEGER NOT NULL DEFAULT 0    -- vitals: stamped daemon-side
ctx_tokens    INTEGER NOT NULL DEFAULT 0
out_tokens    INTEGER NOT NULL DEFAULT 0
ctx_window    INTEGER NOT NULL DEFAULT 0    -- publisher-supplied; 0 = unknown
```
Wire Agent mirrors all six (station/MCP/humancli mirrors updated same slice).
RegisterAgent's upsert DOES NOT touch these (refresh preserves status —
regression-tested); deregister loses them deliberately (documented). Values
validated: 0 <= v <= 10^12; checked addition when summing usage components.

## Daemon ops

- `set_status {alias, status_text?}` — text only, per-alias. Key-PRESENCE
  semantics: absent = preserve; present (incl. "") = set + stamp status_at
  (clear = ''+stamp: auditable, invisible). Sanitized at the daemon
  (display.Sanitize, 120 cols). Unknown alias fails — never creates rows.
  One atomic UPDATE. No journal row, no notify.
- `set_session_vitals {socket_path, session_id, ctx_tokens?, out_tokens?,
  ctx_window?, stamp_turn?}` — metrics for ALL aliases of one exact tuple,
  atomically, one round trip (finding 14). `stamp_turn: true` sets
  turn_ended_at = daemon clock — clients never supply epochs (finding 7).
  Presence semantics as above; touches ONLY metric columns, never
  status_text/status_at. Empty tuple fields fail.
- Vitals/status are NOT journaled (current-state, not bus actions); their
  absence from forensics is deliberate and documented. Observability = the
  fields themselves on list_agents/get_agent (staleness self-describing).

## Publishers

- CLI `muster status "text" | --clear` — mutually exclusive, extra args
  rejected, flag-like text after `--` supported; alias resolution as
  register; in both usage lines.
- MCP `set_status {status_text}` — NO alias input. The mcp server binds
  identity ONCE at startup exactly as register does (MUSTER_ALIAS → tmux
  session name via captured env); unbound identity → tool errors with
  guidance. Local-socket trust model unchanged (documented: any local
  client could call the daemon op directly; the MCP surface simply doesn't
  invite cross-agent writes).
- Hook (automatic; LAST slice; opt-out MUSTER_NO_VITALS=1):
  1. **Decision first.** hookStop computes and WRITES the drain decision
     (or nothing) before any enrichment. Enrichment runs after, with every
     diagnostic discarded (no stdout/stderr writes anywhere in the path,
     incl. panic recovery); stdin read via io.LimitReader (1MB).
  2. Skipped entirely when stop_hook_active=true (finding 13; the original
     invocation publishes even when it emits decision:block — tested both
     halves).
  3. **No-spawn, deadline-bounded client**: a dial-only connection (never
     spawns the daemon) with a 250ms total deadline; daemon down → skip.
  4. Transcript read hardening: Lstat (reject symlinks — product decision,
     documented; harnesses write real files), open, fstat the FD (regular
     file, snapshot size), ReadAt within snapshot, discard first partial
     line, ignore incomplete final line, shrink/rotation → fail closed.
  5. Parser: versioned, evidence-based, Claude-first — exact supported
     record shape (assistant message.usage), backward scan over COMPLETE
     newline-delimited records with an expanding window (64KB → 1MB hard
     cap; a final record bigger than the cap → skip, finding 1). JSON
     numbers decoded without float conversion; negative/fractional/
     overflow rejected; fixtures captured per supported harness version;
     unknown shapes unsupported → skip. Codex parser ships only when its
     format is verified.
  6. ctx = checked-sum of input + cache_read (+ cache_creation ONLY where
     the provider doesn't fold it in — decided per fixture evidence, not
     assumed); ctx_window from the payload's concrete model id via a
     publisher-side table (hook knows the real model; daemon/station never
     guess — finding 5).
  7. Publishes via set_session_vitals (tuple from its own captured env;
     session_aliases not needed). Failure silent.

## Station display (separate UI slice)

Three independent freshness clocks (finding 17): tmux liveness ("offline"
overrides everything, metrics grayed); status_text aged by status_at
("working on X · 3h ago"); ctx aged by turn_ended_at (suppressed > 1h or
when 0). Percentage rendered ONLY when ctx_window > 0: `~85k (42%)`;
otherwise raw tokens. Future timestamps render as "clock skew?" not fresh.

## Privacy scope (stated, not absolute)

Numbers are behavioral metadata: they reveal activity timing, workload
scale, and (with window) model tier to any local bus client — never
transcript content. transcript_path and parse errors are never persisted,
journaled, or copied into status/errors. Auto-publication is opt-out.

## Testing (highlights beyond the per-item tests above)

Fuzz the tail parser on arbitrary bytes/nesting; wall-time test: hookStop
returns within budget under FIFO path, missing daemon, huge stdin, and a
concurrently-appending writer, with stdout byte-identical in all cases;
absent-vs-0-vs-null-vs-negative-vs-string matrix for both ops; pre-status
hand-built DB reopened twice.
