# Teammate identity refusal: fleet teammates never touch the bus's identity

Status: approved in discussion 2026-08-06
Scope: muster only (hook layer + one nudge message). Companion to the
2026-08-05 conversation-identity spec.

## 1. Problem

Caught live twice on 2026-08-06, same day: a fleet teammate's hooks claimed
a primary session's bus row. `bettor-help-workspace-2`'s row carried the
pane (%16) and harness id of teammate `l5-mlb-measure`; `muster-3`'s row
carried a dead teammate pane (%34) and a teammate conversation id — the
second one produced by a perfectly normal subagent-driven workday. Damage:
`muster nudge` (the only pane-exact op) types into a ghost and fails; the
stolen harness id would strand the primary's mailbox behind a fresh alias
at its next resume in a differently-named tmux session.

Which exact door the theft came through is unpinned (the daemon does not
journal register ops): a fresh-register claim during the post-restart
window when the row's stored pane pointed at a dead server; a
resume-reclaim (reclaimRow moves rows without checking pane ownership —
the residual the 2026-08-05 Task 6 re-review logged); or Stop-hook harness
stamping during an unprovable-ownership window. It does not matter. All
three are one structural flaw: **muster has no concept of a teammate.**
Every Claude conversation's hooks run identical identity logic, so a
teammate is a full peer in every race for the session's identity, and the
pane-ownership guard only protects a primary while its pane claim is
provable — every tmux restart, pane recycle, or reap opens a window, and
teammates outnumber primaries in fleet workflows. Racing more carefully
cannot fix this; only teammates not racing at all can.

## 2. Verified detection signal (2026-08-06)

A teammate's transcript carries a top-level `teamName` (with `agentName`)
field on its message records from the first few lines. Verified
empirically:

- 46 transcripts scanned in the muster project dir: every teammate (the
  day's impl-*/review-* fleet plus older become-* agents) shows `teamName`
  within the first 30 lines; zero primaries do.
- Team LEADS are clean: two sessions that each spawned many teammates
  (muster-3, bettor-help-workspace-2) have no top-level `teamName` in
  head-30. The field marks members only.
- Plain `agent-name` records do NOT discriminate (a /rename'd primary has
  one); it is specifically the `teamName` field.
- Task-tool sidechain subagents live in separate `agent-*.jsonl` files
  with `isSidechain: true` and never fire session hooks — out of scope.
- `~/.claude/teams/<team>/config.json` (leadSessionId + members[]) exists
  as a cross-check but is not needed for v1; the transcript signal is
  self-contained and travels with the conversation.

**Predicate:** any record within the first 30 lines of the transcript
whose top-level `teamName` is a non-empty string ⇒ teammate.

## 3. Design

**Rule: a teammate session's hooks are no-ops.** At `muster hook` entry,
if the payload's `transcript_path` identifies a teammate, return silently
— no register, no resume-reclaim, no name projection, no SessionEnd
tombstone sweep, no Stop-hook badge drain or harness stamping. The
teammate is invisible to the bus's automatic identity machinery.

Deliberately unaffected:
- **Explicit registration stays possible.** MCP `register_agent` and CLI
  `muster register` are untouched — a teammate that *wants* a bus identity
  (a standing reviewer, a coordinator) can still be given one on purpose.
  What is barred is *automatic* identity capture.
- **Codex/Cursor hooks**: no `transcript_path` in their payloads → the
  predicate is false → behavior unchanged.
- **Failure posture**: unreadable/missing transcript → not a teammate
  (fail-open to today's behavior; hooks must never block a session).

**Nudge fails usefully on a dead pane.** When the stored pane is gone but
the row's session is alive, `muster nudge` refuses with a message naming
the dead pane and the remedy (the row heals at the session's next
start/resume) instead of a bare failure. No auto-retargeting: with
teammate panes around, typing into a guessed pane is worse than failing.

**Rejected: Stop-hook pane adoption** (operator's call, 2026-08-06).
Primaries re-register at every SessionStart, so stale-pane damage heals at
the session's next start; with refusal in place no new damage can be
created. A one-time audit of live rows (dead pane or teammate-owned
harness id) found exactly two casualties — both repaired by hand the same
day. Standing repair for any straggler: re-register from the live pane
(`TMUX=<socket>,x,x TMUX_PANE=<pane> muster register <alias> --model
claude --harness-session <uuid>`).

## 3a. Addendum (2026-08-06, post-v0.10.1 live acceptance FAILURE)

The v0.10.1 acceptance test — spawn a teammate seconds after the installer
restarted the daemon — reproduced the theft THROUGH the shipped gate. Two
gaps, both confirmed by evidence:

1. **The transcript does not exist when SessionStart fires.** The
   teammate's transcript records the SessionStart hook_success in its FIRST
   flush — the file is written after the hooks run. `IsTeammate` fail-opens
   on the missing file, so the transcript predicate covers Stop/SessionEnd
   but never SessionStart, the most damaging event. §2's timing probe
   measured file-appearance, not hook-fire, and drew the wrong conclusion.
2. **`hookMayClaimIdentity` fails open when the daemon is unreachable.**
   `hookGetAgent` treats a dial error as not-found → claimable. The
   install's LaunchAgent restart put the daemon mid-restart exactly when
   the probe's SessionStart ran its ownership check.

**The durable signal is process argv.** Teammate Claude processes launch as
`claude --agent-id … --agent-name … --team-name … --parent-session-id …`
(verified live on the probe; a primary's process carries none of these).
Argv exists from process birth — no fail-open window — and the hook is a
descendant of that process, reachable by the same ancestry walk the capture
already does.

Revised design:
- **Detection = argv first, transcript second.** `IsTeammate` becomes an
  OR: an ancestor process whose argv carries `--team-name` (authoritative,
  covers SessionStart), or the transcript teamName scan (belt, covers any
  spawn shape where argv is unavailable).
- **Ownership fails closed on daemon errors.** `hookGetAgent` distinguishes
  "row absent" (claimable) from "daemon unreachable" (NOT claimable this
  event — skip registration; the next hook event heals once the daemon is
  back). A hook still never blocks the session; it just declines to write.

## 4. Testing

The regression scenario is not hypothetical — it is the day's incident:
a primary registered on pane A of a tmux session; a teammate SessionStart
fires from pane B of the same session with a teamName-bearing transcript;
the row must be byte-for-byte untouched and the roster must gain no alias.
Plus: teammate SessionEnd tombstones nothing; teammate Stop emits no wake
text; predicate unit tests (member true, lead/primary false, missing/
unreadable/beyond-line-30 false); nudge dead-pane message test.
