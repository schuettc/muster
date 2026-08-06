# Testing the hosted backend

A staged plan for validating the optional hosted backend, ordered cheapest and
highest-value first. Stages 1 and 2 need no AWS account and protect what you
already depend on. Stage 4 is the one that exercises what the feature was built
for.

Each stage says what it proves, what it does **not** prove, and what a failure
looks like — because most failures here are silent. The characteristic symptom
of a bug in this system is not an error message; it is a badge that does not
light.

---

## Stage 0 — what the automated suites already cover

Run these first. If either fails, stop; nothing below is meaningful.

```bash
just verify        # gofmt, golangci-lint, go test -race, build, cross (incl. -tags lambda)
just verify-dynamo # the same store contract against DynamoDB Local, via Docker
```

Without Docker, run the DynamoDB half directly against a local JVM instance:

```bash
java -jar /path/to/DynamoDBLocal.jar -inMemory -port 8000 &
MUSTER_DDB_ENDPOINT=http://localhost:8000 \
  go test -race ./internal/dynamostore/... ./internal/storetest/...
```

**Proves:** local mode is intact; the two store backends agree on 60+ shared
behavioural cases (`internal/storetest`), including the device-collision cases,
scoped broadcast, task claim under 8-way concurrency, and the idempotency
contract.

**Does not prove: anything about eventual consistency.** DynamoDB Local is
strongly consistent. Every bug in that class — a watermark advancing past an
entry that has not replicated, an index read that has not caught up — is
structurally invisible to these suites. Three such bugs were found on this
branch by reading, not by testing, and that is not a gap that more of these
tests would close.

**A note on skips.** These tests skip themselves when `MUSTER_DDB_ENDPOINT` is
unset, and for a long time nothing ran them and nothing said so. The `dynamo`
CI job fails on `--- SKIP` for that reason. If you run them by hand, confirm
you see `ok`, not `no test files` or a skip count.

---

## Before you start — install side by side

Every stage below invokes **`muster-rc`**, not `muster`. That is deliberate: the
release candidate installs alongside your working muster rather than replacing
it, so your live bus keeps running the released build throughout and a bad rc
costs you nothing but two deletions.

On **each** machine:

```bash
# 1. the binary, under a distinct name.
#    Swap darwin_arm64 for your platform: the four assets are
#    muster_{darwin,linux}_{arm64,amd64}.tar.gz — NOT version-stamped,
#    the tag in the URL path is what selects the build.
curl -fsSL -o /tmp/muster-rc.tar.gz \
  https://github.com/schuettc/muster/releases/download/<rc-tag>/muster_darwin_arm64.tar.gz
tar xzf /tmp/muster-rc.tar.gz -C /tmp
install -m755 /tmp/muster ~/.local/bin/muster-rc
muster-rc --version        # expect 0.11.0

# 2. a distinct home, so the test bus never touches your live one
export MUSTER_HOME=~/.muster-rc
mkdir -p "$MUSTER_HOME"
```

`muster` keeps meaning your released build with its own `~/.local/share/muster`.
`muster-rc` with `MUSTER_HOME` set is the test bus. They share nothing — not the
database, not the socket, not the roster.

**Keep `MUSTER_HOME` exported in every shell you test from.** A `muster-rc`
invocation without it talks to your *live* bus, which is both confusing and the
one way this test can disturb something you care about.

To undo everything afterwards:

```bash
rm ~/.local/bin/muster-rc
rm -rf ~/.muster-rc
```

## Stage 1 — local-mode regression

**Cost:** no AWS, ~10 minutes. **Run this before anything else.**

This is the highest-value test in the document, because local mode is what you
already use and this branch threaded a new `deviceID` parameter through four
session-scoped queries. Local mode passes `""` for it everywhere. If that is
wrong anywhere, unread math silently stops matching sessions.

With `MUSTER_BACKEND` unset (or `local`):

Register an agent from inside a tmux session (registration captures the calling
session's tmux identity, so it must run in one):

```bash
muster-rc register test-a --role worker
muster-rc agents           # test-a present, live dot, correct project/label
```

Then, from a second tmux session, exercise the loop that produces a wake:

```bash
# session 2
muster-rc register test-b --role worker

# session 1 — target is POSITIONAL, not a flag
muster-rc send test-b "does the badge light" --from test-a

# session 2 — the badge should light within a second
muster-rc inbox test-b     # message present
muster-rc reply <thread-id> "yes" --from test-b
```

And the task path. Note the human CLI has no task-creation command — tasks are
created by agents over MCP, so use the raw op sender:

```bash
muster-rc debug task_create from=test-a to_kind=role to_target=worker \
  subject="claim me" body="please take this"

muster-rc tasks test-b     # the task is visible
```

**Proves:** the merge did not break the existing bus — registration, addressing,
unread math, badge lighting and clearing, task claim.

**Failure looks like:** a message that arrives in `muster inbox` but never
lights the pane. That is the device-scoping symptom. An outright error is
comparatively good news; a dark badge is the one to watch for.

---

## Stage 2 — the live-rig identity test

**Cost:** no AWS, ~15 minutes. **Nothing below substitutes for this.**

Upstream's v0.8.0–v0.9.1 identity work (durable alias resume, `become`) was
validated by hand against a real harness. This branch merged that with
device-scoping changes touching the same code, and no automated test drives a
real resume.

1. Register an agent in a tmux session and give it some history — send it a
   couple of messages so its inbox is non-empty.
2. Note its alias (`muster whereami`).
3. Kill that tmux session entirely.
4. Start a new session and `claude --resume` the same conversation.
5. In the resumed session:

```bash
muster-rc whereami           # should resolve to the SAME alias as before the resume
muster-rc inbox <that-alias> # the pre-resume history must still be there
muster-rc agents             # ONE live row for that alias, not a ghost pair
```

Then the claim path:

```bash
muster-rc become <a-name-you-choose>
muster-rc agents           # the old alias retired, the new one live and carrying the history
muster-rc whereami
```

**Proves:** alias durability across a resume, and that `become` still clones
identity, read-state and lineage correctly after the merge.

**Failure looks like:** a resumed session registering under a *fresh* alias with
an empty inbox (the orphaned-identity failure the v0.8.0 work exists to
prevent), or `muster agents` showing both the seed and its successor live at
once.

---

## Stage 3 — deploy and single-device remote

**Cost:** real AWS, effectively $0 at this volume.

Follow `docs/hosted-backend.md` end to end — that document is the deploy
instruction set and this stage is partly a test *of it*. If you have to work
anything out that the doc did not tell you, that is a doc bug worth filing.

Then, on one machine:

```bash
export MUSTER_BACKEND=remote
export MUSTER_REMOTE_URL=https://<your-function-url>/
# token at $MUSTER_HOME/remote-token, mode 0600 — the daemon refuses looser modes
```

Restart the daemon so it picks the new mode up, **from a shell that has the
exports** (a shell that does not will silently start a *local* daemon — see the
doc's troubleshooting section), then repeat Stage 1's loop.

```bash
muster-rc agents               # roster now comes from DynamoDB
muster-rc send <alias> "over the wire" --from <your-alias>
muster-rc inbox <alias>
```

**Proves:** the transport, bearer auth, Lambda, and DynamoDB path work at all,
and that the same client commands behave identically against a remote store.

**Does not prove:** almost any of the interesting logic. With one device there
is no cross-device wake, so the poller, device-scoping and reconcile paths are
barely exercised. Do not stop here.

**Failure looks like:** `muster agents` returning an empty roster while the
command succeeds — that is the silent-local-daemon fallback, not an empty bus.
Check `muster --version` (remote mode needs ≥ v0.10.0) and confirm the daemon
you are talking to actually has the exports.

---

## Stage 4 — two devices

**Cost:** real AWS, ~30 minutes. **This is the test the feature exists for.**

You do not need a second laptop. Run two daemons on one machine with separate
`MUSTER_HOME` directories and distinct device ids, both pointed at the same
stack.

This rig is *better* than two machines for one specific reason: both daemons
share the same tmux socket path, which is exactly the `(socket_path,
session_id)` collision that motivated all of the device-scoping work. Two
laptops would also collide (`/private/tmp/tmux-501/default` is the default on
every macOS box), but here you get it guaranteed rather than by luck.

**Device A**, in one terminal:

```bash
export MUSTER_HOME=~/.muster-rc-dev-a
export MUSTER_DEVICE_ID=dev-a
export MUSTER_BACKEND=remote
export MUSTER_REMOTE_URL=https://<your-function-url>/
mkdir -p "$MUSTER_HOME"
cp ~/.local/share/muster/remote-token "$MUSTER_HOME/remote-token"
chmod 600 "$MUSTER_HOME/remote-token"
muster-rc register agent-a --role worker
```

**Device B**, in another terminal — identical but for the home and id:

```bash
export MUSTER_HOME=~/.muster-rc-dev-b
export MUSTER_DEVICE_ID=dev-b
export MUSTER_BACKEND=remote
export MUSTER_REMOTE_URL=https://<your-function-url>/
mkdir -p "$MUSTER_HOME"
cp ~/.local/share/muster/remote-token "$MUSTER_HOME/remote-token"
chmod 600 "$MUSTER_HOME/remote-token"
muster-rc register agent-b --role worker
```

Shorten the poll so you are not waiting on the default 10s:

```bash
export MUSTER_POLL_INTERVAL=2s
```

### 4a — cross-device delivery and wake

```bash
# on device A
muster-rc send agent-b "cross-device hello" --from agent-a

# on device B, within a poll interval
muster-rc inbox agent-b    # the message is there
```

**The badge on B must light.** That is the assertion — not that the message
arrived, which only proves the store works, but that B was *told*.

Then the reverse direction, which exercises a different path (B's poller rather
than A's inline reconcile).

### 4b — the roster is shared but badges are not

```bash
muster-rc agents           # on BOTH devices: the full roster, agent-a and agent-b
```

Each device's own badge must reflect only its own sessions. A badge on A that
advertises `agent-b` is the alias-scoping bug; a badge on A that never lights
when B writes is the unread self-exclusion bug. Both were real on this branch.

### 4c — originated threads

Start a thread from A, have B reply, and confirm **A's badge lights.** This is
the fourth arm of the concern predicate — a reply on a thread you started must
wake you even though the thread is not addressed *to* you. It was missing from
the plan once and would have regressed silently.

### 4d — scoped broadcast

```bash
# on device A, with the two agents in DIFFERENT projects
muster-rc send --broadcast --project <a-project> "scoped" --from agent-a
```

Only agents in that project may receive it. A broadcast that lights every badge
on the bus is the cross-project leak — this was a Critical found in final
review, and it is invisible unless you deliberately test with two projects.

### 4e — task handoff

```bash
# on device A — the human CLI has no task-creation command, so use the raw op
muster-rc debug task_create from=agent-a to_kind=role to_target=worker \
  subject="handoff" body="claim me"

# on device B
muster-rc tasks agent-b    # the task is visible; claim it from the agent side
```

Exactly one device may win the claim. This is the path that most resembles real
use and the one where a dropped wake hurts most — the sender is blocking.

### Cleanup

```bash
rm -rf ~/.muster-rc-dev-a ~/.muster-rc-dev-b
```

**Failure looks like:** a badge that never lights (dropped wake), a badge
showing the other device's agents (alias scoping), a scoped broadcast reaching
everyone (project filter), or two devices both winning a claim (the conditional
write).

---

## Stage 5 — what cannot be tested, and what to watch for

Three windows exist only under real DynamoDB with concurrent writers and index
replication lag. No test in this repo can reach them, and each is documented in
`docs/hosted-backend.md` and in the relevant package comment.

| Symptom | What it is | Data lost? |
|---|---|---|
| Mail in `muster inbox` you were never told about | The missed-notification window: two writers into one recipient partition can bury an entry's badge | No — message and thread survive |
| A badge that lights one message late, catching up on the next | Poll/reconcile skew: the wake is delayed, not dropped | No |
| `muster events --follow` missing a line the backlog query shows | The events cursor can skip an id that had not committed | No — the row exists |

None loses data; all three cost a notification. If you see one, it is a known
window rather than a new bug — but note the circumstances, because the
frequency is unmeasured and a common occurrence would change the calculus.

**What would be a new bug:** a message that is *not* in `muster inbox` at all, a
claim won by two agents, or a badge showing another device's aliases. Those are
not on the known list.

---

## Cost

Stages 3–5 run against real AWS. At the volume this testing produces the bill
is effectively zero — DynamoDB on-demand and Lambda's always-free tier cover
it. See the cost section of `docs/hosted-backend.md` for the steady-state
model.

Tear the stack down when finished if you are not keeping it:

```bash
aws cloudformation delete-stack --stack-name <your-stack>
```

Note the DynamoDB table is deleted with it — a remote bus has no export, so its
history goes with the stack.
