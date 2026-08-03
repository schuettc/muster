# Hosted backend — optional remote bus on AWS

**Date:** 2026-08-03
**Status:** proposed
**Depends on:** label-first identity (v0.7.5), durable alias identity (v0.8.0),
become / claim-your-name (v0.9.0), roster exposure (v0.9.1)

## Problem

muster's state lives in a SQLite file that exactly one daemon owns, on exactly
one machine. That is the right default and it is not changing. But it means an
operator working across two machines has two unrelated buses: agents on the
laptop cannot address agents on the desktop, and the roster, threads, and read
state on each are invisible to the other.

The goal is a bus that spans devices, self-hosted by whoever runs it. We are
explicitly not building a service anyone else's data flows through — the
deliverable is a deployable stack plus instructions, and the local-only path
must remain untouched and dependency-free for people who never want this.

Cost is a first-class constraint. A full RDS instance is out of proportion to a
personal coordination bus, and so is any always-on database tier.

## Decision

Add an **optional remote backend**. The local daemon stays exactly where it is
on every device — it keeps the unix socket, it keeps tmux wake — but in remote
mode it forwards each `proto.Request` upstream instead of dispatching locally.
Upstream is the *same* `dispatch()` running in a Lambda behind a Function URL,
over a DynamoDB implementation of the existing store interface.

Two properties of the current code make this far cheaper than it sounds:

1. `daemon.Serve` already takes a 22-method `storeAPI` interface rather than
   `*store.Store`, introduced so tests could wrap the real store. The pluggable
   seam exists; it needs a second implementation, not a refactor.
2. `dispatch()` is a pure `proto.Request` → `proto.Response` function with no
   knowledge of its transport. A Lambda handler is an adapter over it, not a
   reimplementation of it.

### Rejected alternatives

**A small always-on box (Lightsail, ~$3.50/mo)** running the existing binary
against SQLite. This is by far the least *engineering* work — the release
workflow already cross-compiles linux/arm64, so the server is an artifact we
already ship. It was rejected on running cost rather than on merit, and it
remains the obvious fallback if the DynamoDB implementation proves more
troublesome than estimated. Worth keeping in mind: it needs no store rewrite at
all.

**Local daemons sharing a remote SQL database** (Aurora DSQL, Postgres). This is
the literal reading of "host the database," and it costs a store rewrite for the
same reason DynamoDB does, while also putting business logic on every device and
requiring every cross-device race to be re-derived client-side. It buys nothing
the proxy shape does not.

**Direct device-to-DynamoDB with no Lambda tier.** Genuinely tempting on its
own terms: no server to deploy, no cold starts, infrastructure reduced to one
table and one IAM policy.

**Ruled out by the no-AWS-credentials requirement.** DynamoDB authenticates with
SigV4 and has no bearer-token equivalent, so every device would need AWS
credentials — the exact thing this design must avoid. A Lambda can authenticate
however it likes; a DynamoDB table cannot. This is decisive on its own, and it
is why the server tier is not optional here.

Secondary reasons it was already weak: business logic would ship to every
device, so a stale binary could write state a newer one would not, and there
would be no server-side place to add a push channel later.

**API Gateway in front of the Lambda.** Rejected on cost. At $1.00 per million
requests it would exceed every other line item combined on poll traffic alone.
Function URLs carry no per-request charge and support IAM auth natively.

**Reserved concurrency of 1 to preserve single-writer semantics.** Considered at
length and rejected. See "Concurrency" below — it does not actually solve the
case it was proposed for, and DynamoDB's own primitives solve the rest properly.

## Modes

One binary, three modes, selected by `MUSTER_BACKEND`:

- **`local`** (default) — today's behavior exactly. `store.Open(paths.DBPath())`,
  unix socket, `dispatch()` in process, `wake.NewTmuxNotifier`. No AWS code on
  any path, no new runtime dependencies.
- **`remote`** — `muster serve` still owns the unix socket and tmux wake, but
  forwards whole requests upstream and runs the poller.
- **`lambda`** — a new `cmd/muster` mode alongside `serve` / `mcp` / `debug`.
  Function URL event in, request body unmarshalled to `proto.Request`,
  `Dispatch()`, response marshalled back.

Every client above the daemon — the MCP server, the human CLI, station — is
untouched. They speak to the same unix socket regardless of mode.

### Enabling refactors

- Split `daemon.Serve` into `New(storeAPI, wake.Notifier) *Daemon` plus a thin
  listener wrapper, so lambda mode can construct a `Daemon` without binding a
  socket.
- Export `dispatch` as `Dispatch`.
- Promote the unexported `daemon.storeAPI` to an exported `store.API`, living in
  `internal/store` beside the types it already references (`store.Agent`,
  `store.Thread`, `store.Event`). `daemon` then depends on `store.API` rather
  than declaring its own copy, and `*store.Store` continues to satisfy it
  unchanged. Placing it in `store` rather than `daemon` keeps the DynamoDB
  implementation from importing `daemon` purely to name its own interface.

Lambda mode constructs its `Daemon` with a nil notifier. `Serve`'s existing doc
comment already declares nil supported ("no notifications are delivered"); the
implementation of `notifyForThread` and `pushSessionAgents` under a nil notifier
must be verified rather than assumed during implementation.

## Authentication

The Function URL uses auth type `NONE` and the Lambda authenticates requests
itself against a shared bearer token. **Devices need no AWS credentials, no AWS
configuration, and no AWS SDK** — `internal/remote` is a plain HTTPS client that
POSTs JSON.

This is a hard requirement, not a preference: the operator does not want AWS
credentials provisioned on every machine they code from.

### Mechanics

- The token is generated at deploy time and passed to the stack as a parameter.
- On the device it lives in a file at `<MUSTER_HOME>/remote-token`, mode 0600 —
  **not** an environment variable. muster runs alongside coding agents that read
  their own environment, so a token in `MUSTER_REMOTE_TOKEN` can leak into an
  agent's context or a session transcript. A file the daemon reads at startup
  does not.
- The daemon sends it as `Authorization: Bearer <token>`.
- The handler compares with `crypto/subtle.ConstantTimeCompare` and rejects with
  HTTP 401 **before** touching DynamoDB, and caps request body size before
  parsing.
- The Lambda accepts **two** valid tokens (`MUSTER_TOKEN` and
  `MUSTER_TOKEN_PREVIOUS`), so a new token can be rolled out across devices
  before the old one is retired. With a single token every device breaks the
  instant it rotates, which in practice means it never rotates.

### Accepted risk

With `AWS_IAM`, unsigned requests are rejected at the AWS edge before any code
runs. With `NONE`, the URL is publicly reachable and security rests entirely on
the token. A leaked token grants full bus access — read every message, send as
any agent — which is comparable to what leaked IAM credentials would grant; the
difference is that AWS would otherwise manage rotation and expiry.

Two things bound this. Function URL hostnames are random 32-character
subdomains on `lambda-url.<region>.on.aws` and are not enumerable in practice.
And the reserved concurrency cap (see "Concurrency") bounds what an attacker who
does find the URL can cost.

The URL should therefore be treated as a secret in its own right, as defense in
depth, and documented that way.

### This is a first step, not the destination

A single shared bearer token is deliberately the *simplest thing that meets the
no-AWS-credentials requirement*, and it is expected to be replaced. Its
weaknesses are known and accepted for v1: one secret shared by every device
means no per-device attribution and no way to revoke a single device without
rotating all of them, and the token is long-lived.

The planned upgrade, in order of cost:

1. **Per-device tokens held in DynamoDB** rather than one shared token in the
   function's environment. Each device gets its own, stored **hashed** (the
   table already exists, so this is one item type and one lookup), giving
   per-device attribution and single-device revocation via a
   `muster device revoke` command. This is the cheapest meaningful improvement
   and the intended next step.
2. **OIDC with short-lived tokens** — a browser device-code login on the daemon,
   a signed JWT verified by the handler, no long-lived shared secret anywhere.
   This is the real destination if the bus outlives a single operator.

`AWS_IAM` remains available as a third path and is worth reconsidering
specifically under AWS SSO, which issues short-lived credentials without
long-lived keys on disk — that combination would satisfy the spirit of the
no-credentials requirement while restoring edge-level rejection.

**To keep this cheap later, authentication must sit behind a seam from day
one.** The handler resolves credentials through a small interface rather than
reading environment variables inline, so swapping the env-token implementation
for a table lookup is a one-file change rather than a rewrite of the request
path.

## Wake

All wake unifies behind a single local operation: **reconcile this device's
session badges**. For each `(socket_path, session_id)` with live local agents,
take the local `sessionLock`, fetch `session_unread` upstream, write the
`@muster_inbox` tmux option.

`sessionLock` moves from a server-side concern to a purely local-daemon one.
This is where it belonged regardless: `tmuxenv` confirms `socket_path` is the
tmux server socket, so the lock's key is device-scoped by construction and two
devices can never contend on it. Today's code only gets away with holding it
server-side because server and client are the same process.

Reconcile has two triggers:

- **Inline after a local write.** Same-device messaging keeps today's latency
  characteristics; nothing waits for a poll.
- **The poller,** for traffic originating on other devices.

### The poller

A new op, `device_poll`, takes a device id and a last-seen entry id and returns
the sessions needing reconciliation plus a new watermark. The server does the
filtering, because it holds both the roster and the entries; the daemon does not
scan entries client-side. One round trip per tick.

The poller only runs when the device has live local agents registered — a
device with an idle daemon and an empty local roster polls nothing and costs
nothing. Interval is a knob (`MUSTER_POLL_INTERVAL`, default 10s) that backs off
while quiet and tightens after recent cross-device traffic, per the repo's
"knobs, not constants" rule.

Long-polling is not used. It was attractive under reserved-concurrency-1 and is
unnecessary without it, but it would hold a Lambda execution environment open
for the duration and is a poor fit for the cost model. If poll latency ever
becomes a real complaint, the escalation is DynamoDB Streams driving a push
channel, not a longer-held request.

### Device identity

`store.Agent` gains a `DeviceID`. `SocketPath` cannot serve this purpose, since
two machines can both have `/tmp/tmux-501/default`. A UUID is generated on first
run and persisted at `<MUSTER_HOME>/device-id`, overridable via
`MUSTER_DEVICE_ID`, with hostname recorded alongside for display.

This is an additive column in the SQLite schema too — local mode gets a device
id it simply never varies.

## DynamoDB model

Single table, on-demand capacity.

Thread metadata and entries share partition `THREAD#<id>` with a **numeric** sort
key: entry ids as numbers, with zero reserved for the thread metadata row. This
gives correct ordering without zero-padding, and makes `GetThread` a single
query.

Ids come from counter items updated with an atomic `ADD`. These must be
**globally** monotonic, not per-thread, because `Agent.LastReadEntryID` is a
global watermark — per-thread sequences would silently corrupt unread math.

Two secondary indexes:

- **GSI1** partitions by recipient — `RCPT#agent#<alias>`, `RCPT#role#<role>`,
  `RCPT#broadcast` — sorted by entry id. "Unread for me" becomes three queries
  with a sort-key lower bound, no joins and no scans. This requires
  denormalizing the thread's recipient onto every entry. The roster lives in a
  disjoint `ROSTER` partition of the same index, which `ListAgents` queries.
- **GSI2** is a single `ENTRIES` partition ordered by entry id — the global log
  `device_poll` reads.

Events carry a native TTL attribute, which removes `PruneEvents` as a concept on
this backend.

The single-partition indexes (`ENTRIES`, `ROSTER`) are acceptable because
DynamoDB permits 1000 WCU and 3000 RCU per partition and this bus writes at
single-digit-per-minute rates. If that ever stops being true, sharding the
partition key is the remedy.

Schema evolution has no `ALTER` equivalent and needs none: the implementation
tolerates missing attributes, which is simpler than the additive-migration
pattern `store.migrate()` maintains for SQLite.

## Concurrency

Handled with DynamoDB primitives rather than process-local mutexes:

- **`ClaimTask`** is already a compare-and-swap — `UPDATE ... WHERE id=? AND
  status='open'` with a `RowsAffected` check. It becomes that same condition as
  a `ConditionExpression`, bundled with the entry insert in a
  `TransactWriteItems`.
- **`RegisterAgent`** is likewise a CAS, as `aliasLock`'s own comment states. It
  becomes a conditional write. This is strictly better than the mutex: the
  guarantee is durable and in the database rather than contingent on all writers
  sharing one process.
- **`sessionLock`** moves to the local daemon (see "Wake"), where its key is
  device-scoped and contention is impossible.

Reserved concurrency is capped at 10 as a **cost guard only** — blast radius
against a bug that spins the poller. It is not a correctness mechanism and must
not be documented as one.

Note for implementers: a concurrency cap of 1 was seriously considered as a way
to preserve today's single-writer property, and rejected. It fails on
`sessionLock`, whose critical section spans the recompute (server) and the tmux
write (client) and is therefore split across processes no matter what the
server's concurrency is. It also converts contention into 429 throttles rather
than queueing, adds head-of-line blocking, and buys nothing the conditional
writes above do not already provide.

## Idempotency

Write retries must be safe, because a blind retry of `send_message` would
duplicate a message on a bus whose entire job is not losing or duplicating them.

`proto.Request` gains an idempotency key field:

```go
IdemKey string `json:"idem,omitempty"`
```

It is `omitempty` and ignored by local dispatch, so the wire format stays
backward compatible and local mode is unaffected.

The local daemon generates one key per logical write and **reuses it across
retries of that same request**. Only the daemon→Lambda hop carries keys; the
client→daemon unix socket hop does not need them.

Server side, before executing any write op:

1. Conditional `PutItem` of an idempotency record at `IDEM#<key>` with
   `attribute_not_exists`, state `pending`, and a 24-hour TTL.
2. **Put succeeds** — first delivery. Execute the op, then write the serialized
   response into the record and mark it `done`.
3. **Put fails on the condition** — duplicate. If the record is `done`, return
   the stored response verbatim. If it is still `pending`, a concurrent
   identical request is in flight; return a retryable error and let the client
   back off.

This applies **uniformly to every write op**, not to a classified subset.
Several writes are naturally idempotent (`kv_set` is last-write-wins,
`register_agent` is an upsert) and would not strictly need it, but a uniform
rule cannot be got wrong, and the cost is roughly one extra write unit per
mutation — cents per month at this volume. Note that CAS ops genuinely need it
despite looking idempotent: if a `task_claim` succeeds but its response is lost,
a naive replay returns `ErrNotClaimable` and the original caller wrongly
concludes it failed.

With keys in place, the transport may retry writes on connection failures, 5xx,
and throttles, in addition to reads.

## Configuration and deployment

Environment variables, matching `MUSTER_HOME`'s existing style:

| Variable | Default | Meaning |
|---|---|---|
| `MUSTER_BACKEND` | `local` | `local` \| `remote` \| `lambda` |
| `MUSTER_REMOTE_URL` | — | Function URL (required when `remote`) |
| `MUSTER_POLL_INTERVAL` | `10s` | poller base cadence |
| `MUSTER_DEVICE_ID` | persisted file | device identity override |

The bearer token is **not** an environment variable — see "Authentication". It
is read from `<MUSTER_HOME>/remote-token` (mode 0600). A device in remote mode
needs nothing else: no region, no profile, no AWS credentials.

Deployment is a CloudFormation template in `contrib/` creating the table, the
function, the Function URL, and the execution role. It takes the bearer token as
a `NoEcho` parameter and outputs the Function URL. The release workflow gains one
artifact: a `bootstrap` zip for the `provided.al2023` runtime, built from the
linux/arm64 binary it already cross-compiles.

## Costing

Modelled on two devices, a 10-second poll, daemons up twelve hours a day, plus a
few hundred real operations a day — roughly 275,000 invocations a month. List
rates, us-east-1. **These rates must be re-verified before they are published in
user-facing docs.**

| Line item | Volume/month | Rate | Cost |
|---|---|---|---|
| Lambda requests | 275k | 1M/mo always-free | $0 |
| Lambda duration | ~700 GB-s | 400k GB-s/mo always-free | $0 |
| Function URL | — | no charge | $0 |
| DynamoDB reads | ~138k RRU | $0.25/M | ~$0.03 |
| DynamoDB writes | ~180k WRU | $1.25/M | ~$0.23 |
| DynamoDB storage | megabytes | 25GB always-free | $0 |
| **Total** | | | **~$0.26/mo** |

The Lambda free tiers used here are the always-free ones, not the twelve-month
introductory tier. Write volume includes the idempotency record per mutation.

Poll cadence is not a meaningful cost driver: polling every two seconds around
the clock instead — the aggressive case — lands near $0.65/mo, still dominated
by DynamoDB rather than Lambda.

### Cold starts

Reserved concurrency is capped but not pinned, and the poll stream keeps
execution environments warm during any period a device has live agents.
Interactive calls therefore land warm at roughly 30–50ms — network round trip
plus a DynamoDB query. A cold start of 200–400ms is expected on the first call
after an idle gap, such as the start of a working day, plus occasional blips
when Lambda recycles environments. It is not a per-call tax.

## Error handling

- **Reads** retry with backoff on 5xx and throttles.
- **Writes** retry under the same conditions, made safe by idempotency keys.
- **Poller failures** log to stderr and back off. They never take down the
  daemon, and they never block the unix socket.
- **Unreachable upstream** surfaces a clear error to the CLI rather than
  hanging. All upstream calls carry timeouts.

Diagnostics go to stderr unconditionally, including in lambda mode, per the
repo's rule that stdout is the MCP channel.

## Testing

The existing store test suite runs against both backends through the store
interface, parameterized by implementation.

The DynamoDB half requires **DynamoDB Local** rather than a hand-written
in-memory fake. A fake would not reproduce conditional-expression semantics, and
conditional expressions are precisely where this design's correctness lives —
`ClaimTask`, `RegisterAgent`, and every idempotency check.

That is a container dependency, so it stays out of `just verify`, which must
remain fast and dependency-free per CLAUDE.md. It lives in a separate
`just verify-dynamo` recipe run as its own CI job. `just verify` remains the
gate for the default path.

Additional coverage needed:

- Idempotency: replayed writes return the original response; `pending` collisions
  return retryable errors; keys expire.
- The poller: watermark advancement, backoff, and the empty-local-roster case
  that must not poll.
- Nil-notifier behavior in lambda mode.

## Out of scope

- **Migration from an existing local bus.** A remote bus starts empty. Export
  and import of an existing SQLite bus is deferred.
- **A push channel.** Polling is the wake mechanism for v1. DynamoDB Streams is
  the documented escalation.
- **Offline queueing.** No network means no remote bus, which matches today's
  behavior where no daemon means no bus.
- **Multi-tenancy.** One deployment serves one operator. There is no account
  model, and none is planned.
- **Anything beyond a shared bearer token for authentication.** Per-device
  tokens and OIDC are planned successors, not v1 scope — see "This is a first
  step, not the destination". The v1 requirement on them is only that the
  `Authenticator` seam exists so they are a one-file change later.
