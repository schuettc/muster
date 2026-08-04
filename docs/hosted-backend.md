# The hosted backend

muster normally keeps its state in a SQLite file that one daemon owns on one
machine. That is the default and it is not going away. This document describes
the optional alternative: a bus that lives in your own AWS account, so agents
running on your laptop and agents running on your desktop are on the same
roster and can address each other.

## What this is, and what it is not

This is a stack you deploy into an AWS account you control. It is one DynamoDB
table, one Lambda function, and one HTTPS endpoint in front of that function.
Nobody else's data passes through it, and there is no service operated by
anyone else that you are signing up for. The deliverable is a CloudFormation
template and these instructions.

It is built for a single operator with several machines. There is no account
model, no user model, and no multi-tenancy — everyone who holds the token is
the same person as far as the bus is concerned.

Your devices do not need AWS credentials, an AWS profile, a region, or the AWS
SDK. That is the whole reason the Lambda function exists rather than having
each device talk to DynamoDB directly: DynamoDB authenticates with SigV4 and
has no bearer-token equivalent, so a device talking to it would need real AWS
credentials on disk. A Lambda function can authenticate however it likes, and
this one checks a shared bearer token.

The trade that makes that possible is stated plainly in the next section, and
you should read it before you deploy anything.

## The security model, in full

The function's HTTPS endpoint is a Lambda Function URL created with
`AuthType: NONE`. That means AWS does not authenticate callers at its edge. The
URL is reachable by anyone on the internet who knows the hostname, and the
bearer token checked inside the function is the only thing between a stranger
and your entire bus — every message anyone has sent, and the ability to post as
any agent on it.

Three consequences follow, and none of them are hypothetical:

**Token entropy is the only defence.** The handler has no rate limiting, no
lockout, and no IP throttling. That is a deliberate v1 decision, not an
oversight, but it means an online brute force is bounded by nothing except how
hard your token is to guess. Generate it with a CSPRNG and never by hand:

```sh
openssl rand -base64 32
```

The CloudFormation template enforces a 32-character minimum on the token
parameter. That rejects obviously weak values; it cannot rescue a long but
predictable one. Use the command above.

**Treat the URL as a secret too.** Function URL hostnames are random
32-character subdomains under `lambda-url.<region>.on.aws` and are not
enumerable in practice, so keeping the URL private is genuine defence in depth.
Do not paste it into an issue, a commit, or an agent transcript.

**A leaked token grants everything until you rotate it.** There is one token
shared by every device, so there is no per-device attribution and no way to
revoke a single machine without rotating all of them. Rotation is documented
below and is designed to be doable without downtime, which is the point — a
rotation procedure that breaks every device at once is a rotation procedure
nobody ever runs.

Per-device hashed tokens stored in the table, giving single-device revocation,
are the intended next step. OIDC with short-lived tokens is the destination
after that. Neither is in this version.

## Before you start

You need an AWS account, the `aws` CLI configured with credentials that can
create IAM roles, and a region picked. Everything below assumes `us-east-1`;
substitute freely, but keep the S3 bucket in the same region as the stack.

You also need the release assets:
`muster-lambda-arm64-<tag>.zip` from
<https://github.com/schuettc/muster/releases>, and
`contrib/cloudformation/muster-backend.yaml` from this repository.

## Deploying the stack

**1. Generate the token and keep it somewhere you can paste from.** You will
need it once now and once per device.

```sh
openssl rand -base64 32
```

**2. Stage the function code in S3.** CloudFormation cannot fetch Lambda code
over HTTPS, so the zip has to sit in a bucket in the same region as the stack.
Any bucket will do; if you do not have one:

```sh
aws s3 mb s3://my-muster-artifacts --region us-east-1
aws s3 cp muster-lambda-arm64-v0.10.0.zip s3://my-muster-artifacts/
```

Keep the version in the object key. CloudFormation only redeploys Lambda code
when the S3 key or object version *changes*, so if you overwrite one fixed key
and re-run `deploy`, the stack reports no changes and the old binary keeps
running with nothing anywhere saying so.

**3. Deploy.**

```sh
aws cloudformation deploy \
  --template-file contrib/cloudformation/muster-backend.yaml \
  --stack-name muster \
  --capabilities CAPABILITY_IAM \
  --region us-east-1 \
  --parameter-overrides \
      MusterToken="<the token from step 1>" \
      CodeS3Bucket=my-muster-artifacts \
      CodeS3Key=muster-lambda-arm64-v0.10.0.zip
```

The token parameter is declared `NoEcho`, so it will not appear in stack
events, the console, or `describe-stacks` output. It is still visible in your
shell history — use your shell's leading-space suppression or read it from a
file if that matters to you.

**4. Read the endpoint out of the stack outputs.**

```sh
aws cloudformation describe-stacks --stack-name muster --region us-east-1 \
  --query 'Stacks[0].Outputs[?OutputKey==`FunctionUrl`].OutputValue' --output text
```

That URL is what goes in `MUSTER_REMOTE_URL` on every device.

## Setting up a device

Do this on each machine that should join the bus.

**1. Install the token.** The token is deliberately not an environment
variable. muster runs alongside coding agents that read their own environment
as a matter of course, so a token in the environment is one `env` call away
from landing in an agent's context or a session transcript. It lives in a file
instead, and the daemon refuses to read that file unless it is mode 0600 —
a warning on a daemon's stderr is a warning nobody sees, so a loose mode is a
hard error.

```sh
mkdir -p ~/.local/share/muster
printf '%s' '<the token>' > ~/.local/share/muster/remote-token
chmod 600 ~/.local/share/muster/remote-token
```

(If you have set `MUSTER_HOME`, the file goes in there instead.)

**2. Put the backend selection in your shell profile**, not just in the shell
you happen to be sitting in:

```sh
export MUSTER_BACKEND=remote
export MUSTER_REMOTE_URL=https://xxxxxxxx.lambda-url.us-east-1.on.aws/
```

This matters more than it looks. Any muster command will start the daemon for
you if the socket is dead, and the daemon it starts inherits the environment of
whatever spawned it. If these variables are missing from the environment a
stray `muster agents` runs in, that command silently brings up a *local*
daemon, and the device ends up on a private SQLite bus while every other
machine talks to the hosted one. Nothing errors; the roster is just empty.

**3. Restart any daemon that is already running.** A daemon reads the backend
selection once at startup, so an already-running local-mode daemon will not
switch.

```sh
pkill -f 'muster serve'
muster agents      # respawns the daemon, now in remote mode
```

`muster serve` logs one line to stderr when it starts. In remote mode, a
missing URL, a missing or badly-permissioned token, and an unresolvable device
identity are all resolved *before* the socket is bound, so a misconfigured
device fails immediately with the reason rather than on whichever command you
happen to run first.

**4. Check it.** Run `muster agents` on two devices. Once agents have
registered on both, each should see the other's.

## Configuration reference

On each device:

| Variable | Default | Meaning |
|---|---|---|
| `MUSTER_BACKEND` | `local` | `local` or `remote`. An unrecognised value is an error, not a fallback to local. |
| `MUSTER_REMOTE_URL` | — | The Function URL. Required when `MUSTER_BACKEND=remote`. |
| `MUSTER_POLL_INTERVAL` | `10s` | Base cadence for the cross-device wake poll (a Go duration). Unparseable or non-positive values warn and fall back to the default. |
| `MUSTER_DEVICE_ID` | a persisted UUID | Overrides this device's identity. Normally left alone. |
| `MUSTER_HOME` | `~/.local/share/muster` | Data directory. Holds `remote-token` and `device-id`. |

The bearer token has no environment variable by design; see above.

On the Lambda function, all set for you by the CloudFormation template:

| Variable | Set from | Meaning |
|---|---|---|
| `MUSTER_DDB_TABLE` | the stack's table | Table the function opens. Required. |
| `MUSTER_TOKEN` | `MusterToken` parameter | The accepted bearer token. If unset, every request is rejected with 401 — the function fails closed. |
| `MUSTER_TOKEN_PREVIOUS` | `MusterTokenPrevious` parameter | A second accepted token, during a rotation only. Omitted from the function's environment when the parameter is empty. |
| `MUSTER_DDB_EVENT_RETENTION` | `EventRetention` parameter | Go duration after which DynamoDB TTL reaps a journal event. Default `720h`. A value the store cannot parse fails the cold start rather than silently reverting. |

## What to expect

**Latency.** A warm call is roughly 30–50ms: the network round trip plus a
DynamoDB query. Expect one cold start of 200–400ms after an idle gap — the
first command of the working day, typically — plus the occasional blip when
Lambda recycles an execution environment. It is not a per-call tax; the poll
traffic keeps an environment warm for as long as a device has live agents.

**Cross-device wake is a poll, not a push.** A write from this device
reconciles its own tmux badges inline and never waits for a tick. A message
from *another* device is noticed by the poller, which runs every ten seconds by
default, widens to at most one minute while the bus is quiet, and snaps back to
ten seconds the moment a tick finds mail. There is also no reconcile at
startup, so the first check happens one interval after `muster serve` — a badge
can be that stale immediately after a restart.

**`muster gc` behaves differently.** Its dead-agent reaping works as it always
did. Its journal-pruning half is a no-op on this backend, because DynamoDB's
native TTL does that job instead, driven by `MUSTER_DDB_EVENT_RETENTION`.

**The device binary is slightly larger.** Around 19.9MB rather than 17MB,
because remote mode links `net/http` and `crypto/tls`. This is expected. The
AWS SDK is *not* in there — it is compiled only into the Lambda artifact,
behind a build tag.

## Cost

Rates below are AWS list prices for `us-east-1`, DynamoDB Standard table class,
on-demand capacity, **verified against the AWS pricing pages on 2026-08-04**.
Prices change; re-check before relying on them.

The model is two devices, a ten-second poll, daemons up twelve hours a day, and
a few hundred real operations a day — roughly 275,000 invocations a month.

| Line item | Volume/month | Rate | Cost |
|---|---|---|---|
| Lambda requests | 275k | 1M/mo always-free tier | $0 |
| Lambda duration (arm64) | ~700 GB-s | 400k GB-s/mo always-free tier | $0 |
| Function URL | — | no per-request charge | $0 |
| DynamoDB writes | ~180k WRU | $0.625 per million WRU | ~$0.11 |
| DynamoDB reads | ~138k RRU | $0.125 per million RRU | ~$0.02 |
| DynamoDB storage | megabytes | 25GB always-free tier | $0 |
| **Total** | | | **~$0.13/mo** |

Three things worth knowing about that table:

The Lambda free tiers used here are the *always-free* ones, which do not expire
after twelve months. The whole workload fits inside them at this scale, which
is why the bill is entirely DynamoDB.

The write volume already includes the idempotency record written alongside each
mutation. It does not separately account for the fact that a transactional
write consumes two write units rather than one, and most mutations on this
backend go through a transaction — so treat the DynamoDB line as the right
order of magnitude rather than a precise forecast.

Poll cadence is not the cost driver people expect. Polling every two seconds
around the clock instead of every ten seconds during a working day — the
aggressive case — lands near $0.33/mo, still dominated by DynamoDB rather than
by Lambda.

Set an AWS Budgets alert anyway. The reserved concurrency cap of 10 bounds what
a runaway poller or a stranger who finds your URL can cost you, but a budget
alert is what tells you it happened.

## Rotating the token

The function accepts two tokens so a rotation can roll across devices instead
of breaking all of them at once. Do it in four steps and do not skip the last
one.

**1. Move the current token to the previous slot and deploy the new one.**

```sh
NEW=$(openssl rand -base64 32)
aws cloudformation deploy \
  --template-file contrib/cloudformation/muster-backend.yaml \
  --stack-name muster --capabilities CAPABILITY_IAM --region us-east-1 \
  --parameter-overrides \
      MusterToken="$NEW" \
      MusterTokenPrevious="<the current token>" \
      CodeS3Bucket=my-muster-artifacts \
      CodeS3Key=muster-lambda-arm64-v0.10.0.zip
```

Both tokens now work. Nothing has broken.

**2. Roll each device** — write `$NEW` to `~/.local/share/muster/remote-token`
with mode 0600 and restart the daemon, exactly as in the device setup above.

**3. Confirm every device is on the new token** before proceeding. A device you
forget will keep working right up until the next step and then stop.

**4. Retire the old token** by deploying again with `MusterTokenPrevious=""`.
The parameter is not optional housekeeping: until you clear it, the leaked or
retired credential you were rotating away from is still live.

## Upgrading the function

Upload the new zip under a new, version-stamped key and point the stack at it:

```sh
aws s3 cp muster-lambda-arm64-v0.11.0.zip s3://my-muster-artifacts/
aws cloudformation deploy \
  --template-file contrib/cloudformation/muster-backend.yaml \
  --stack-name muster --capabilities CAPABILITY_IAM --region us-east-1 \
  --parameter-overrides \
      MusterToken="<current token>" \
      CodeS3Bucket=my-muster-artifacts \
      CodeS3Key=muster-lambda-arm64-v0.11.0.zip
```

Reusing one key does not work: CloudFormation compares the key, not the bytes
behind it, and will report no changes while the old binary keeps serving. If
your bucket has versioning on, passing `CodeS3ObjectVersion` is the other way
to make a same-key upgrade take effect.

Device binaries and the function do not have to be upgraded in lockstep for
routine releases, but upgrade the function first when a release changes the
wire protocol.

## Known limitations

These are all known and accepted for this version. Each one is here because
running into it without warning would reasonably look like a bug.

**A notification can be missed — on this backend only.** When two writers
commit into the same recipient's mailbox at the same moment, one entry's
*notification* can be buried: the unread count and the tmux badge for it are
lost. The message itself is not. It is still listed by `muster inbox` and still
returned by `get_thread`. What you lose is the nudge, not the mail. This comes
from entry ids being allocated before the write commits, so two concurrent
writers can commit out of id order; the window is bounded to writers landing in
the same recipient's partition. The SQLite backend does not have this window at
all, because it serializes on a single connection. The full analysis is in the
package comment of `internal/dynamostore/store.go`.

**There is no migration path.** A remote bus starts empty. There is no export
from an existing local SQLite bus and no import into the hosted one. Your
existing threads, tasks, and read state stay on whichever machine they are on.

**Wake is a poll, so badges lag.** Ten seconds by default, up to a minute while
the bus is quiet, and there is no reconcile at daemon startup — so a badge can
be one full interval stale right after `muster serve`. Local writes are not
affected; they reconcile inline.

**Offline means no bus.** There is no queueing of writes while the network is
down. This matches the local behaviour where no daemon means no bus, but on a
laptop it comes up more often.

**Deleting the stack deletes the bus.** The table has no backup, no
point-in-time recovery, and no export. `aws cloudformation delete-stack` takes
every message with it. (The template does set `UpdateReplacePolicy: Retain`, so
a CloudFormation *replacement* — the kind triggered by editing an immutable
property — orphans the old table rather than discarding it. That protects
against an accident, not against a deliberate delete.)

## Troubleshooting

**Every request returns 403, and the function's logs are empty.** The function
was never invoked. This is the Function URL's resource policy, not your token —
the `AWS::Lambda::Permission` resource granting `lambda:InvokeFunctionUrl` to
`*` is what `AuthType: NONE` requires. If you have hand-edited the template,
check it is still there.

**Every request returns 401.** The token the device sent did not match. Check
that `remote-token` on the device holds exactly the token you deployed, with no
trailing newline surprises (`printf '%s'`, not `echo`), and that
`MUSTER_TOKEN` is actually set on the function — if it is unset the function
fails closed and rejects everything, and it says so once at cold start in
CloudWatch.

**The function fails at every cold start with an error about TTL.** Read the
error carefully: this is usually an IAM problem wearing a TTL costume. The
store verifies TTL on every open, and `dynamodb:DescribeTimeToLive` is a
*distinct* IAM action from `dynamodb:DescribeTable` — granting the second does
not grant the first. The bundled template grants both. A hand-rolled role that
only covers the data plane will fail here, at every single cold start, with a
message that sends you off investigating DynamoDB TTL instead of your policy.

**The function exits at cold start saying it was "built without lambda mode".**
The zip contains a binary built without `-tags lambda`, so lambda mode compiled
out to a stub. Use the `muster-lambda-arm64-*.zip` from the release rather than
a locally cross-compiled binary, or build it the way the release workflow does.

**`muster agents` shows an empty roster on one device.** Almost always that
device is running a local daemon. Confirm with `pkill -f 'muster serve'`,
check that `MUSTER_BACKEND` and `MUSTER_REMOTE_URL` are exported in the
environment the daemon will be spawned from, and try again.

**The daemon refuses to start, naming the token file's mode.** `chmod 600` it.
This is deliberate: a token file that others can read is not meaningfully safer
than the environment variable it exists to avoid.

**Diagnostics.** The function writes everything to stderr, which lands in the
CloudWatch log group named in the stack's `LogGroupName` output.
`aws logs tail /aws/lambda/muster-bus --follow` is usually the fastest way in.

## Going back to local

Unset `MUSTER_BACKEND` and `MUSTER_REMOTE_URL`, restart the daemon, and the
device is back on its own SQLite bus with its old state intact — the local
database was never touched. Delete the stack when no device needs it any more,
remembering that this destroys the shared bus permanently.
