# Device-relative aliases — a machine's name never shows on its own machine

**Date:** 2026-08-10
**Status:** proposed
**Depends on:** device-seeded aliases (`internal/humancli/aliasseed.go`), hosted backend (0.11.0+)

## Problem

Device-name seeding exists for a real failure. Derived aliases come from a tmux session name or a directory basename, both of which are identical on two machines with the same repos checked out. Registration is an upsert on the alias primary key, so the second machine to register takes the row — and because the roster row *is* the identity, it takes the inbox and read-state with it. Seeding puts the machine's name in front of derived aliases so that cannot happen.

It solves that, and then charges for it in a place where nothing was wrong. The prefix appears on the machine that minted it, which is the one context that already knows its own name. The operator's verdict: the machine name is only useful *between* machines.

The symptom that surfaced it is worth recording, because it is exactly the cost in miniature. The operator's dotfiles render muster's `@muster_agent` tmux option into the terminal title, but only when it differs from the tmux session name — a deliberate dedupe, so a title never says the same thing twice. Before seeding, the alias equalled the session name and the title stayed clean. After seeding, `h-dotfiles/h-prefix` no longer equalled `dotfiles/h-prefix`, the dedupe stopped matching, and a machine name the operator never needed appeared in every window title on the machine it names.

Two further gaps come from the same root:

- Seeding fires on a purely local bus, where the roster is per-machine and no alias can collide with anything. The cost is paid; the protection is vacuous.
- Seeding applies only to *derived* aliases. A typed name — `muster become galley/design`, or `$MUSTER_ALIAS` — is left literal, so the names an operator chooses deliberately are precisely the ones left globally unqualified and open to the collision seeding exists to prevent.

## Decision

The **stored** alias stays globally unique and device-seeded. The **displayed** alias is device-relative: on the machine that minted it the prefix is not shown, and a bare name typed there resolves to it.

Alias remains the primary key of the agents table. Nothing in the store changes.

Six decisions carry this, each with the reason it was chosen over its alternative.

**1. Presentation layer, not data model.**

Rejected: making identity a composite of (device, alias), so `dotfiles/main` could exist on both machines and cross-device addressing qualified explicitly. That is the more honest model — it makes "on this machine we know our name" true at the data layer rather than papered over at render — but it is a uniqueness-key change in both the SQLite and DynamoDB stores plus device scoping on every op that takes an alias, landing in the middle of the hosted-backend rollout. The presentation-layer change buys the same daily experience and is reversible.

Rejected: gating seeding on `MUSTER_BACKEND=remote`, so a local bus never seeds. The operator expects remote to be the usual mode, so this would prefix nearly everything anyway — it addresses the vacuous-protection gap while leaving the actual complaint untouched.

**2. Every alias this machine mints is seeded — derived and typed alike.**

The current carve-out for typed names reasons that an explicit choice should not be silently rewritten. Hiding the prefix inverts that reasoning. Once the roster shows `dotfiles/main` and `galley/design` side by side, nothing distinguishes the alias that is device-scoped from the one that will collide, so an operator would reasonably type `become galley/design` on a second machine believing it behaves like every other name on screen. One rule with no exceptions is the only rule that survives the prefix being invisible.

**3. Stripping applies to human surfaces only. Model-facing surfaces keep the full form.**

Models write aliases into message bodies and task descriptions that are read on the *other* machine, where a bare name re-resolves against that device and lands on a different, real agent. The failure modes are asymmetric: a human who types a short name that does not resolve gets an error and retries, while a model that writes a short name into a durable thread produces a silent misroute that nothing reports. Consistency is not worth buying at that price, and it costs the operator nothing they asked for — the prefix that prompted this work is in a title bar, which is a human surface.

**4. Resolution expands local-first**: `<device>-<given>` is tried before the literal `given`.

Rejected: exact-match first, which is strictly additive and cannot change the meaning of anything that resolves today. It was rejected because it reintroduces the action-at-a-distance this design removes — under exact-first, whether your own short name reaches your own agent depends on whether some other machine happens to hold that bare string, and when it does not, you silently get theirs.

**5. A device name is required, auto-adopted from the sanitized hostname at first registration, and pinned to a file.**

Rejected: hard-failing registration until the operator names the machine. It blocks a fresh install at its most fragile moment — the SessionStart hook, on first run.

Rejected: seeding from `device.Name()` with its live hostname fallback and no file. A hostname is not stable: it changes when a machine joins a network that already has that name (`Courts-MacBook-Pro-2.local` carries its `-2` for exactly this reason), when it joins a domain, or when it is restored from a backup. The prefix is stamped into the roster at registration, so a drifting hostname means a machine silently begins registering under a new prefix and orphans every alias and inbox behind the old one. Pinning once makes the seed immutable regardless of what the OS later does.

The long-name objection to auto-adoption largely evaporates under this design: an unrenamed `courts-macbook-pro-2-dotfiles/main` renders as `dotfiles/main` on its own machine, and the long form appears only on the other machine, where the disambiguation is the point.

**6. No `--global` escape hatch.**

Rejected: a flag preserving today's ability to mint an unprefixed alias shared across machines. A shared alias does not produce a roaming identity; it produces a contested one. Registration is an upsert on a primary key, so two machines claiming `court` do not share an inbox — they take turns owning it, and mail follows whoever registered last with nothing reporting the handoff. That is the failure this design exists to prevent. If reaching "whichever machine I am at" is wanted later, the honest mechanism is an alias resolving to a device rather than a row two machines fight over, and it deserves its own design.

## Mechanism

### 1. Minting — `seedAlias` loses its carve-out

`internal/humancli/aliasseed.go:32` currently seeds only derived aliases and only when `device.NameConfigured() != ""`. Both conditions go.

Seeding reads the configured name, and auto-adoption (§6) guarantees one exists before any alias is minted: adoption runs at the head of registration, ahead of the seed. The ordering is the whole reason the `NameConfigured()` gate can be dropped rather than merely widened — without it, dropping the gate would seed against an empty string.

Seeding applies at every site that mints an alias: `register` (derived, positional arg, and `$MUSTER_ALIAS` — `internal/humancli/identity.go:46,64`), `become` (`internal/humancli/become.go:36`), and paneless allocation (`internal/humancli/paneless.go:39`).

The existing idempotence guard stays and becomes load-bearing: an input already carrying the prefix, or equal to the device name, is returned unchanged. `become personal-galley/design` and `become galley/design` must produce the same stored alias, or an operator who reads a full name off the other machine and pastes it back creates a second identity.

### 2. Display — one helper, no plumbing

A single function strips a leading `<device-name>-` from an alias for display. It needs no per-row device data, which is what makes this change cheap: only this machine mints this prefix, so the prefix alone identifies a local row. This matters because `internal/station` never decodes `device_id` at all (`internal/station/poll.go:77`) and the MCP roster row omits it (`internal/mcpserver/call.go:16`); neither needs to change.

Applied at the human surfaces:

- `muster agents` ALIAS column — `internal/humancli/humancli.go:236`
- `inbox` / `tasks` FROM/TO/LAST-FROM — `internal/humancli/humancli.go:514`
- `thread` header and per-entry author — `internal/humancli/thread.go:70,89`
- `register` / `become` / `deregister` / `gc` / `nudge` confirmations — `identity.go:152,160,206,322,351`, `become.go:96`, `humancli.go:620`, `register_ack.go:29`
- station's `dispLabel` and `dispToTarget` — `internal/station/model.go:1503,1519`
- `events` / `watch` in `--aliases` mode — `internal/render/renderer.go:113,124,139`

Explicitly **not** applied at the model surfaces: `AgentView` / `ThreadView` / `EntryView` (`internal/mcpserver/call.go:95,116,134`), the `register_agent` and `requireRegisteredFrom` detail strings (`tools_registry.go:52,60`, `validate.go:41`), and the hook text injected into agent context (`internal/humancli/hook.go:270,285,635,688`).

### 3. Resolution — one helper, applied at every input site

A single function expands a given alias to `<device-name>-<given>`, tried before the literal string.

`internal/resolve` is device-blind by design — `resolve.Candidate` carries no device field, and the package doc states that an alias means the same agent from every device. Rather than teaching the resolver about devices, expansion happens on the **input** before it reaches `resolve.Target`, which keeps the resolver's contract intact and confines the change to callers.

Applied at the two resolver wrappers — `humancli.resolveVia` (`internal/humancli/resolve.go:59`) and `daemon.resolveAgentTarget` (`internal/daemon/resolve.go:26`) — and at the sites that bypass the resolver and exact-match: `deregister` (`identity.go:177`), `become --from` (`become.go:105`), `reply --from`, `events --agent` / `watch --agent`, `task_claim` / `task_transition` `by` (`daemon.go:792,799`), and the MCP `from` / `alias` fields (`validate.go:26`).

Daemon-side expansion is **required, not optional**: MCP hands `to_target` straight to the daemon with no client-side resolution (`internal/mcpserver/tools_messages.go:66`), so the daemon is the only place a model-supplied target is checked.

### 4. The daemon needs its device name in local mode

`daemon.Serve` takes no device identity — `d.deviceID` and `d.deviceName` are the remote-mode half (`internal/daemon/daemon.go:41-48`), populated only by `ServeRemote` (`cmd/muster/main.go:205`). Local mode (`main.go:154`) passes neither, so daemon-side expansion has nothing to expand with.

The daemon reads the device name once at startup, matching `ServeRemote`'s existing snapshot semantics. A name change mid-run is not tracked, which is consistent with how the device name already behaves and acceptable because auto-adoption makes changes rare and deliberate.

### 5. Collision-aware rendering

Stripping can make two visible rows render the same string: a legacy unprefixed row and a new seeded one, or a foreign machine's bare alias that matches one of ours. When two rows in the same view would display identically, both render in full form.

This is not migration-only code — the cross-device case is permanent. It mirrors an existing pattern: station already falls back from `label` to `label (alias)` when `computeLabelCollisions` (`internal/station/nav.go:399`) flags an ambiguity.

### 6. Auto-adoption of the device name

At the first registration on a machine with no `device-name` file, muster writes the sanitized hostname via the existing `device.SetName` (`internal/device/device.go:113`) and reports it once: the name it took, and that `muster device <name>` changes it.

`device.NameConfigured()` keeps its meaning — the operator's file or `$MUSTER_DEVICE_NAME` — and after auto-adoption it is always populated, so the distinction between a configured and a merely-derived name stops driving behavior.

### 7. The `@muster_agent` tmux option carries the stripped form

`internal/wake/wake.go:86` writes the live alias list to a per-session tmux option, consumed by the operator's `.tmux.conf`. This is a human surface and gets the stripped form. It is unambiguously local: remote mode already narrows the badge to `ag.DeviceID == d.deviceID` (`internal/daemon/remotemode.go:275`).

No change is needed in the dotfiles repo. Its existing title dedupe starts working again on its own once the alias matches the session name.

## What does not change

The store schema, the alias primary key, `resolve.Target`'s precedence rules and its device-blind contract, the label mechanism (`@claude_task`), `become`'s claim-and-retire semantics, and the `if_absent` CAS suffixing that produces `-2` / `-3` on genuine collisions.

## Migration

Existing unprefixed rows are left alone. They stay addressable by their literal aliases, since expansion falls back to exact match, and collision-aware rendering keeps them legible next to seeded rows. Stale rows are reaped by `muster gc` in the normal way.

The doc comment at `internal/humancli/humancli.go:75-81` asserts that an alias means the same agent from every device on the bus. It is revised: the *stored* alias is globally unique; the *displayed* alias is device-relative.

## Testing

- **Seeding:** derived, positional, `$MUSTER_ALIAS`, and `become` inputs all seed; an already-prefixed input is unchanged; an input equal to the device name is unchanged.
- **Round-trip:** `become <short>` and `become <full>` produce the same stored alias and the same displayed alias.
- **Display split:** a given roster renders stripped through the CLI and station, and full through `AgentView` and the hook text — asserted on the same fixture so the two cannot drift apart.
- **Resolution:** a bare name resolves to the local seeded row; a full foreign name resolves exactly; a bare name with no local match falls back to exact; local-first precedence is asserted against a fixture holding both a local seeded row and a foreign bare row of the same short name.
- **Daemon-side:** a model-supplied bare `to_target` reaches the local agent through `send_message` with no client-side resolution in the path.
- **Collision rendering:** two rows that strip to the same string both render full.
- **Auto-adoption:** first registration with no `device-name` file writes the sanitized hostname and reports it; a subsequent registration does not rewrite it; a hostname change afterwards does not change the seed.
- **Unnamed-device compatibility:** the conformance suite in `internal/storetest` continues to pass against rows carrying no device name.

## Open risks

**A bare alias on a named machine can no longer be minted.** Anything that depended on registering an unqualified name must now qualify it or accept the prefix. Nothing on the operator's bus does.

**Local-first shadows a bare alias, and for a legacy row that means it stops being addressable at all.** Any agent whose alias strips to one of ours cannot be reached by that bare string from this machine. Under decision 5 a bare alias can only be a row minted before auto-adoption. Collision-aware rendering keeps such a row visible next to its seeded twin — but visible is all it stays.

An earlier draft of this section claimed "the full alias still resolves exactly." That holds for a *seeded foreign* alias, whose full form is not the bare string. It is false for exactly the case the sentence was about: a legacy **unprefixed** row's full alias IS the bare string, so once a seeded twin exists there is no string left that reaches it. Expansion rewrites the bare form to the twin at both input layers, ahead of any resolution — `humancli.expandAlias` (`inbox`, `tasks`, `nudge`, `send`, `reply --from`, `deregister`, `events`/`watch --agent`, and any label that resolves to the legacy row) and the daemon's own `resolveAgentTarget`/`requireKnownAlias` (station's and MCP's sends, claims and inbox reads, which have no client-side resolution to shadow first). `become --from <bare>` is shadowed too, and worse than merely refused: it retires the *twin* while its confirmation, stripped for display, reads as though it retired the legacy row.

What still works is everything that never goes through an alias: the row renders in `muster agents` and station's roster, `muster thread <id>` reads its threads by ID, and mail already addressed to it still notifies, since a stored `to_target` is never expanded. What does not work is sending to it, draining it, or carrying it forward with `become`.

No escape hatch is added, for decision 6's reason: a flag letting a caller opt out of the addressing rule is permanent, while a legacy row is a migration artifact with a bounded life. The remedy is `muster gc --purge-agents` once the row is departed. This is called out in the upgrade notes rather than left to be discovered.

**Human and model vocabularies differ by design.** An agent reports itself as `personal-dotfiles/main` while the operator's terminal shows `dotfiles/main`. This is deliberate — see decision 3 — but it will read as a bug at least once, so the divergence is worth a line in the user-facing docs.
