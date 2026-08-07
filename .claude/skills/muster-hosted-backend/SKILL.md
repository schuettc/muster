---
name: muster-hosted-backend
description: Use when the operator wants muster's bus to span more than one machine — deploying the optional hosted backend to their own AWS account with muster-deploy, adding a second device to an existing bus, putting a custom domain on the endpoint, or diagnosing a hosted bus that is not delivering. Fires on "one bus across my laptop and desktop", "deploy muster to AWS", "muster-deploy", MUSTER_BACKEND=remote, or a device that joined but shows an empty roster.
---

# Standing up muster's hosted backend

By default every machine runs its own bus and they are unrelated: agents on the
laptop cannot address agents on the desktop. The hosted backend replaces the
local SQLite store with DynamoDB behind a Lambda, so one bus spans devices. It
is opt-in and the local path is untouched.

## Before anything: the token rule

The bus is protected by a single bearer token. The API endpoint is publicly
reachable and there is no authorizer in front of it, so **that token is the
entire security of every message the operator has ever sent.**

**Never put the token in your context.** Concretely:

- Do not `cat`, `echo`, or `head` the token file.
- Do not run `muster-deploy -join` yourself. Its whole purpose is to print the
  token, so running it puts a live credential in this transcript. Tell the
  operator to run it in their own terminal.
- Do not paste the token into a command you construct. Use `$(cat <path>)`
  instead — the shell expands it, and only the command text reaches you.

Checking health this way is fine, because only a status code comes back:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST "$MUSTER_REMOTE_URL" \
  -H "Authorization: Bearer $(cat "$MUSTER_HOME/remote-token")" \
  -d '{"op":"list_agents"}'
```

If a token does end up on screen, say so plainly and tell the operator to
rotate it. Rotation is documented in `docs/hosted-backend.md` and is designed
to run without downtime.

## Deploying

`muster-deploy` is not installed with `muster` — it links the AWS SDK, which
the device binary deliberately does not, and it is needed on one machine once:

```sh
curl -fsSL https://muster.tools/install.sh | sh -s -- --with-deploy
```

The operator needs AWS credentials with rights to CloudFormation, IAM, Lambda,
DynamoDB, API Gateway, CloudWatch Logs, and S3, plus ACM and Route53 for a
custom domain. An administrator identity covers it; an IAM-only one fails
partway through the stack. Only the deploying machine needs any of this.

```sh
muster-deploy --region us-east-1
```

That does everything: creates the artifact bucket, downloads the Lambda zip
matching its own version, uploads it, applies the stack, waits, and prints the
endpoint. On a first deploy it generates a token and writes it to
`<MUSTER_HOME>/remote-token` at mode 0600 without printing it. On an update it
keeps the token already in the stack, so re-running never rotates the fleet's
credential by accident.

It costs roughly $0.41/month at personal scale. Deleting the stack deletes the
table and every message on the bus, with no backup.

## Adding a second device

Ask the operator to run `muster-deploy -join` **themselves** and follow what it
prints. It gives them the endpoint, the commands for the new machine, the token
(separated from the paste-able block so it never enters that machine's shell
history), and a fingerprint.

The token has to be carried across by a human, and that is not a gap to work
around. It is a `NoEcho` stack parameter so CloudFormation will not return it,
and the premise of this backend is that devices need no AWS credentials — a
device with no credentials cannot fetch its own credential. Any trusted channel
works: a password manager, AirDrop, `scp`, or typing 44 characters.

Then have them verify with fingerprints rather than by comparing the secret:

```sh
tr -d '\n' < "$MUSTER_HOME/remote-token" | shasum -a 256 | cut -c1-16
```

On the joining device, set `MUSTER_BACKEND=remote` and `MUSTER_REMOTE_URL`, and
**do not set `MUSTER_DEVICE_ID`** — each machine must derive its own, because
device identity is what keeps one machine's agents from being mistaken for
another's.

Start the daemon **from a shell that has those exports**. One started without
them silently comes up on the local SQLite bus and looks completely fine; an
empty roster on a device you believe joined is almost always this.

## A custom domain

Worth suggesting when the operator cares about stability, not looks: the
generated endpoint embeds a random api-id, so deleting and recreating the stack
hands out a different URL and every device breaks at once. A custom domain is
the only thing that survives that.

```sh
muster-deploy --region us-east-1 --domain muster.example.com --hosted-zone Z0123456789
```

Without a Route53 zone in the same account, the operator must validate an ACM
certificate themselves — **in the stack's region**, not us-east-1; the
"certificates live in us-east-1" rule is a CloudFront rule and does not apply
to an HTTP API's regional custom domain — and pass `--cert`.

`muster-deploy` refuses `--domain` without one of those before making any AWS
call, because that combination leaves CloudFormation waiting hours on a DNS
validation record nothing will publish.

## When it is not working

Read the status code first; each one points somewhere different.

| Symptom | Where to look |
|---|---|
| `401` | The device's token does not match the stack's. Compare fingerprints, don't compare tokens. |
| `500`, empty Lambda log group | The request never reached the function. Check the API access log group — it is the only place a pre-integration failure appears. |
| `429` | Route throttling. Something is polling far harder than expected, or the endpoint is being hammered. Look before raising the limit. |
| `403` mentioning Function URL authorization | A stale endpoint from an older deploy. Take `MUSTER_REMOTE_URL` from the stack's `MusterUrl` output. |
| Cold-start failure about TTL | Usually IAM wearing a TTL costume: `dynamodb:DescribeTimeToLive` is a distinct action from `DescribeTable`. |
| Empty roster on one device | Almost always a local daemon — the exports were missing from the shell that started it. |

There are two log groups and the distinction matters: the Lambda group has the
function's own diagnostics, and the API access log group is the only record of
requests rejected *before* the function ran. An empty Lambda group during
failures means look at the other one.

## Verifying delivery actually works

Do not conclude wake is broken from a single check — three things produce false
failures, in this order of likelihood.

**Check the badge before reading the inbox.** `muster inbox` marks the thread
read, which clears the badge. Assert `tmux show-options -t <session> -v
@muster_inbox` first, then read to confirm the message.

**Give it time and poll.** Reconcile is asynchronous even on the writing
device, so a badge takes about a second. The poller widens its interval while
the bus is quiet, so a cross-device badge can take 10–15 seconds against a 2s
setting. Loop for ~30 seconds before concluding anything.

**One tmux session per device.** If two daemons on one machine badge the same
tmux session, whichever polled last wins and a daemon with nothing unread
actively clears the option — so each device's agents need their own session.
This only bites when simulating two devices on one machine; real machines have
separate tmux servers.

## Things that are true and surprising

Addressing is device-blind. An alias means the same agent from every machine,
and aliases are unique bus-wide — registering one that already exists takes it
over rather than creating a second agent. `muster agents` grows a DEVICE column
only when the roster spans machines; another machine's agent shows `◌` for
liveness because a local tmux probe cannot answer for a remote session.

`muster gc` only reaps agents belonging to the machine it runs on. Each device
runs its own and together they cover the bus.
