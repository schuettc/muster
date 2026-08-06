# CLAUDE.md — muster

Conventions for working in this repo (humans and agents).

## What muster is

A local multi-agent coordination bus: independent coding-agent sessions (Claude
Code + OpenAI Codex, each in its own tmux tab) hand tasks/messages to each other —
no copy/paste, subscription-only (the bus never calls a model; it routes between
agents already running on their own plans). One static Go binary, multi-mode
(`serve` daemon · `mcp` stdio server · human CLI · `lambda` handler for the
optional hosted backend).

## Build / test / run

- **`just verify`** — the gate: `gofmt`, `golangci-lint`, `go test -race`, build,
  `cross` (all four release targets plus the `-tags lambda` build). Run it before
  every commit; CI runs the same recipe, so local and CI can't drift.
- **`just verify-dynamo`** — the second gate, deliberately NOT part of `verify`
  because it needs Docker. Runs `internal/dynamostore` and the DynamoDB half of
  the cross-backend conformance suite against DynamoDB Local. Without an endpoint
  those tests *skip*, so `verify` compiles and vets them but proves nothing about
  DynamoDB semantics — run this after touching `internal/dynamostore`. The
  `dynamo` job in CI runs the same two packages against a service container.
- **cgo-free** — the binary builds under `CGO_ENABLED=0` (pure-Go SQLite via
  `modernc.org/sqlite`). Don't add cgo dependencies.
- **macOS tests** use `internal/mustertest.ShortHome()` for unix-socket paths (the
  `sun_path` ~104-char limit; `t.TempDir()` is too long and breaks the socket).
- Build + run: `go build -o ~/.local/bin/muster ./cmd/muster`, then
  `muster serve | mcp | agents | send | inbox | tasks | nudge | register | …`.

## Branch model

`feat/* → dev → main`. CI (`just verify`) is required on `dev` and `main`; `main` is
the release line. Never develop on `main` — do feature work in a git worktree off
`dev`, merged via PR.

**Releases are automated.** The `VERSION` file is the knob: bump it on `dev`, and
when the promotion PR merges to `main`, the release workflow tags `v<VERSION>`,
creates the GitHub release with generated notes, and attaches cross-compiled
binaries (darwin/linux × arm64/amd64) with checksums. A merge to `main` that
doesn't bump `VERSION` releases nothing. Afterwards, run
`contrib/release-sign.sh v<VERSION>` from a Mac to sign + notarize the darwin
assets in place (CI attaches unsigned ones).

## Architecture (the mental model)

- **daemon = API.** A lazy unix-socket daemon speaking newline-delimited JSON
  (`internal/proto`). The MCP server (`internal/mcpserver`) and the human CLI
  (`internal/humancli`) are **peer clients** of the daemon — neither goes through the
  other. Any daemon op is reachable from a plain CLI subcommand.
- **tmux = substrate.** Liveness, wake, and identity lean on tmux — but only through
  `internal/tmuxenv` (the one canonical capture path) or the injected
  `wake.Notifier`. Keep `internal/daemon` and `internal/store` tmux-agnostic.
- **Wake is split.** `internal/wake` *notifies* (sets the `@muster_inbox` tmux
  option; never types into a pane). `internal/nudge` is the **only** send-keys path.
  The daemon never types.

## The naming contract

The tmux option pair `@claude_task` / `@claude_task_manual` is the neutral
meeting point between muster and the operator's dotfiles (spec:
docs/superpowers/specs/2026-08-05-conversation-identity-naming-design.md).
Intentional gestures (prefix T, `muster label`, the SessionStart projection
of a transcript custom-title) set both; automatic syncs write only the label
and defer to the flag; readers trust the pair. The conversation's transcript
custom-title is the one durable name — tmux and the bus are projections.
Attribution requires proof: `session_created = 0` never matches a live
session (tmuxenv.IsSessionAlive), while DepartStaleSiblings still spares
those rows from reaping — attribution and tombstoning are distinct decisions.

## Hard rules

- **stdout is sacred in `mcp` mode** — it is the MCP channel. All diagnostics go to
  **stderr**. A stray `fmt.Println` on an mcp-mode path corrupts the protocol.
- **The AWS SDK never reaches a device binary.** Only `internal/dynamostore` and
  `internal/lambdamode` may import `github.com/aws/aws-sdk-go-v2/...` or
  `aws-lambda-go`, and the ONLY edge into them is `cmd/muster/lambda_on.go`,
  which carries `//go:build lambda`. Drop an AWS import into any untagged
  package — `internal/remote`, `internal/daemon`, `internal/store` — and it
  links into the binary every device installs. Devices reach the hosted bus over
  plain HTTPS with a bearer token (`internal/remote`) and need no AWS
  credentials, profile, region, or SDK; that is the entire reason the Lambda
  tier exists rather than devices talking to DynamoDB directly. `just cross`
  (in `verify`) builds both configurations, and `.github/workflows/release.yml`
  builds the device binaries without the tag and the Lambda zip with it — so
  check the build tag before adding an AWS import, and never widen that edge.
- **One canonical module per concern** — identity capture lives in `internal/tmuxenv`,
  not copied around. Extend the owner; don't fork it.
- **Knobs, not constants** — operator-tunable defaults over hardcoded numbers.

## Package map

`cmd/muster` entrypoint · `internal/proto` wire protocol · `internal/client` daemon
client · `internal/daemon` the daemon · `internal/store` the `store.API` interface
+ its SQLite implementation · `internal/mcpserver` MCP tools · `internal/humancli`
operator CLI · `internal/wake` notify · `internal/nudge` send-keys ·
`internal/tmuxenv` tmux capture/liveness/label · `internal/harnessenv` paneless
harness-session capture (tmuxenv's counterpart) · `internal/paths` socket+db paths ·
`internal/clock` injectable time · `internal/mustertest` shared test helpers.

Hosted backend (all optional; a device links only `remote` and `device`):
`internal/dynamostore` the **second `store.API` implementation**, on DynamoDB —
lambda-only · `internal/lambdamode` Function URL → `daemon.Dispatch` adapter —
lambda-only · `internal/remote` the device's HTTPS+bearer-token transport to the
hosted bus · `internal/device` this machine's stable device identity ·
`internal/storetest` the conformance suite both `store.API` implementations must
pass, so the two backends cannot drift.

`store.API` having two implementations is the thing to hold onto: behaviour SQLite
gets for free from one pinned connection (ordering, serialization) costs conditional
writes and transactions on DynamoDB, and the divergences that survived are documented
in the package comments of `internal/dynamostore/store.go` and `events.go`. Add an
op to `store.API` and you owe both backends plus a conformance test.

## Using the bus itself

If you're an agent working here and want to coordinate with sessions in other
terminals, the `.claude/skills/muster-coordination` skill is the etiquette
(register, inbox, send/reply, addressing, the wake model).

## Worktrees

Every distinct line of work — a feature, a fix, an agent's task — runs in its own
git worktree on its own branch. **Never branch-switch this clone**; it may hold
uncommitted work.

Worktrees live at **`<repo-root>/.worktrees/<branch>`** — inside the repo, resolved
from the repo root, never as a sibling directory:

```bash
ROOT=$(git rev-parse --show-toplevel)
git -C "$ROOT" worktree add "$ROOT/.worktrees/<branch>" -b <branch> origin/dev
```

- **Never a sibling** (`../<repo>-<topic>`, `<repo>-worktrees/`). Siblings escape the
  repo, clutter the parent directory, and strand themselves when the repo moves.
- **Never `/tmp`.** On macOS it symlinks to `/private/tmp`; worktrees under it break
  vite-node/vitest module resolution *before a single test runs*.
- Remove the worktree and its branch once merged:
  `git -C "$ROOT" worktree remove "$ROOT/.worktrees/<branch>"`.
- `.worktrees/` and `.claude/worktrees/` are gitignored.
