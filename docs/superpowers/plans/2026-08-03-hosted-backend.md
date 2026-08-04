# Hosted Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional remote backend so muster agents can coordinate across
devices, with the local SQLite path unchanged and dependency-free as the default.

**Architecture:** The local daemon stays on every device — it keeps the unix
socket and owns tmux wake — but in `remote` mode forwards whole `proto.Request`s
upstream instead of dispatching locally. Upstream is the *same* `Dispatch()`
running in a Lambda behind a Function URL, over a DynamoDB implementation of the
existing store interface. Wake unifies behind one local operation (reconcile
this device's session badges), triggered inline after local writes and by a
poller for remote-originated traffic.

**Tech Stack:** Go 1.26.4, AWS SDK for Go v2 (`config`, `service/dynamodb`,
`feature/dynamodb/attributevalue`) and `aws-lambda-go` — **server side only**;
DynamoDB Local for tests; CloudFormation for deployment. Devices in remote mode
use net/http and a bearer token, with no AWS dependency whatsoever.

**Spec:** `docs/superpowers/specs/2026-08-03-hosted-backend-design.md`

## Global Constraints

- **cgo-free.** The binary must build under `CGO_ENABLED=0`. The AWS SDK v2 is
  pure Go; do not add cgo dependencies. `just verify` runs `cross` which
  cross-compiles darwin/linux × arm64/amd64 — that is the enforcement.
- **stdout is sacred in `mcp` mode and in `lambda` mode.** All diagnostics go to
  stderr. A stray `fmt.Println` on those paths corrupts the protocol.
- **`just verify` must stay fast and dependency-free.** DynamoDB-backed tests
  skip cleanly when `MUSTER_DDB_ENDPOINT` is unset. `just verify-dynamo` is the
  recipe that requires the container.
- **Local mode must be byte-for-byte unchanged.** No AWS package may be imported
  on a code path reachable from `MUSTER_BACKEND=local`. Default is `local`.
- **The AWS SDK ships only in the Lambda artifact.** `internal/lambdamode` and
  `internal/dynamostore` are reachable **only** under the `lambda` build tag.
  The default binary — the one every device runs, in local *and* remote mode —
  must contain no AWS code at all. Baseline for comparison: the pre-change
  binary is 17,075,090 bytes (~16MB); an untagged `just build` must stay within
  a few hundred KB of that. If it jumps toward 25MB, an AWS import has leaked
  into the default build and that is a constraint violation, not a size
  regression to accept.

  This works because remote mode authenticates with a bearer token over plain
  HTTP (see Task 12), so a device using the hosted backend needs no AWS SDK
  either. The only artifact that carries it is the Lambda zip.
- **One canonical module per concern.** Identity capture stays in
  `internal/tmuxenv`. Do not fork it.
- **Knobs, not constants.** Operator-tunable defaults over hardcoded numbers.
- **macOS tests** use `internal/mustertest.ShortHome()` for anything involving
  the unix socket (the `sun_path` ~104-char limit).
- **Branch model.** This work happens in a worktree off `dev` at
  `<repo-root>/.worktrees/feat-hosted-backend`, merged to `dev` via PR. Never
  develop on `main`.

---

## File Structure

**New packages:**

- `internal/store/api.go` — the exported `store.API` interface (moved from
  `daemon.storeAPI`) plus shared result types (`DevicePollResult`, `SessionRef`).
- `internal/device/device.go` — device identity: generate-and-persist a UUID at
  `<MUSTER_HOME>/device-id`, honoring `MUSTER_DEVICE_ID`.
- `internal/dynamostore/` — the DynamoDB implementation of `store.API`, split by
  concern to mirror `internal/store`: `store.go` (client, table bootstrap, key
  helpers, counters), `agents.go`, `threads.go`, `tasks.go`, `kv.go`,
  `events.go`, `idem.go`, `poll.go`.
- `internal/storetest/` — the backend conformance suite, run against both
  implementations.
- `internal/remote/` — the upstream transport: bearer-token auth, timeouts,
  retry policy, idempotency-key generation. No AWS dependency.
- `internal/lambdamode/` — the Function URL adapter over `daemon.Dispatch`.
- `contrib/cloudformation/muster-backend.yaml` — table, function, URL, role.

**Modified:**

- `internal/daemon/daemon.go` — `storeAPI` replaced by `store.API`; `Serve`
  split into `New` + listener; `dispatch` exported as `Dispatch`; server-side
  idempotency wrapper; remote-mode forwarding path; the poller.
- `internal/proto/proto.go` — `Request.IdemKey`.
- `internal/store/` — `Agent.DeviceID`, schema + migration, `idem` table,
  `DevicePoll`.
- `cmd/muster/main.go` — backend selection, and a `runLambda()` call whose two
  implementations are selected by build tag.
- `cmd/muster/lambda_on.go` (`//go:build lambda`) — imports `internal/lambdamode`
  and calls `lambdamode.Run()`. This is the *only* file in `cmd/muster` that may
  reference an AWS-dependent package.
- `cmd/muster/lambda_off.go` (`//go:build !lambda`) — a stub returning a clear
  error. Imports nothing AWS.
- `justfile` — `verify-dynamo`, and a tagged variant in `cross`.
- `.github/workflows/` — the dynamo CI job and the Lambda release artifact.

---

## Phase 1 — Seams

These three tasks change no behavior. They are independently shippable and
should merge even if later phases stall.

### Task 1: Extract `store.API` and split `daemon.Serve`

**Files:**
- Create: `internal/store/api.go`
- Modify: `internal/daemon/daemon.go:25-83`
- Test: `internal/daemon/surface_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `store.API` (the 22 methods of today's `daemon.storeAPI` plus
  `UnreadCount`, all with signatures unchanged);
  `daemon.New(s store.API, n wake.Notifier) *Daemon`;
  `daemon.Serve(socketPath string, s store.API, n wake.Notifier) (*Daemon, error)`
  (unchanged signature apart from the interface type);
  `(*Daemon).Dispatch(req proto.Request) proto.Response`.

- [ ] **Step 1: Write the failing test**

Add to `internal/daemon/surface_test.go`:

```go
func TestNewDispatchesWithoutListener(t *testing.T) {
	s := newDaemonTestStore(t) // existing helper in this package
	d := daemon.New(s, nil)
	resp := d.Dispatch(proto.Request{Op: "list_agents"})
	if !resp.OK {
		t.Fatalf("list_agents via New+Dispatch: %s", resp.Error)
	}
}

func TestStoreSatisfiesAPI(t *testing.T) {
	var _ store.API = (*store.Store)(nil)
}
```

If `newDaemonTestStore` does not exist under that name in this package, use
whatever the existing tests use to build a `*store.Store`; do not add a second
helper.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run 'TestNewDispatchesWithoutListener|TestStoreSatisfiesAPI' -v`
Expected: FAIL — `undefined: daemon.New`, `undefined: store.API`.

- [ ] **Step 3: Create `internal/store/api.go`**

Move the interface body verbatim from `daemon.storeAPI`. Because it now lives in
`store`, drop the `store.` qualifiers:

```go
package store

// API is the store surface the daemon depends on. It lives here rather than in
// daemon so that alternate backends (see internal/dynamostore) can satisfy it
// without importing daemon purely to name the interface. *Store satisfies this
// interface as-is.
//
// UnreadCount is included even though the daemon reaches it only indirectly
// (via SessionUnread): the backend conformance suite asserts on it directly,
// and a backend that got it wrong while getting SessionUnread right would be a
// latent bug nothing else catches.
type API interface {
	RegisterAgent(Agent) error
	ListAgents() ([]Agent, error)
	GetAgent(alias string) (Agent, bool, error)
	DepartAgent(alias string) error
	DepartStaleSiblings(socketPath, sessionID string, created int64, keepAlias string) ([]string, error)
	SetSessionLabel(socketPath, sessionID, label string, manual bool) (int64, error)
	DeleteAgent(alias string) error
	CreateThread(t Thread, firstBody string) (int64, error)
	AppendEntry(threadID int64, fromAgent, body, statusChange string) (int64, error)
	ClaimTask(threadID int64, byAgent string) error
	TransitionTask(threadID int64, byAgent, newStatus, note string) error
	GetThread(id int64) (Thread, []Entry, error)
	Threads(limit int) ([]Thread, error)
	Inbox(alias string) ([]Thread, error)
	MarkRead(alias string) error
	UnreadCount(alias string) (int, error)
	SessionUnread(socketPath, sessionID string) (total, action int, err error)
	KVSet(key, value, updatedBy string) error
	KVGet(key string) (KVPair, bool, error)
	AppendEvent(e Event) error
	Events(q EventQuery) ([]Event, error)
	MaxEventID() (int64, error)
	PruneEvents(olderThanMillis int64) (int64, error)
}
```

- [ ] **Step 4: Rewire `internal/daemon/daemon.go`**

Delete the `storeAPI` declaration. Change the `Daemon.s` field to `store.API`,
and both constructors to take `store.API`. Split `Serve`:

```go
// New builds a Daemon over s with no listener bound. Lambda mode uses this to
// get a Dispatch target without a unix socket; n may be nil, in which case no
// notifications are delivered.
func New(s store.API, n wake.Notifier) *Daemon {
	return &Daemon{s: s, n: n}
}

// Serve binds socketPath (replacing any stale socket) and serves in a
// goroutine. n may be nil, in which case no notifications are delivered.
func Serve(socketPath string, s store.API, n wake.Notifier) (*Daemon, error) {
	_ = os.Remove(socketPath) // clear a stale socket from a previous run
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	d := New(s, n)
	d.ln = ln
	go d.acceptLoop()
	return d, nil
}
```

`Close` must tolerate a nil listener, since `New` leaves it nil:

```go
// Close stops accepting connections. Safe on a Daemon built by New (no listener).
func (d *Daemon) Close() error {
	if d.ln == nil {
		return nil
	}
	return d.ln.Close()
}
```

Rename `dispatch` to `Dispatch` (exported) and update its call site in `handle`.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/daemon/... ./internal/store/... -race`
Expected: PASS.

- [ ] **Step 6: Verify nil-notifier safety**

This is the assumption lambda mode rests on, and the spec flags it as
unverified. Add to `internal/daemon/wake_wiring_test.go`:

```go
func TestNilNotifierWritePathIsSafe(t *testing.T) {
	s := newDaemonTestStore(t)
	d := daemon.New(s, nil)

	reg := d.Dispatch(proto.Request{Op: "register_agent", Args: map[string]any{
		"alias": "a1", "role": "worker",
	}})
	if !reg.OK {
		t.Fatalf("register: %s", reg.Error)
	}
	// send_message reaches notifyForThread, which reaches the notifier.
	send := d.Dispatch(proto.Request{Op: "send_message", Args: map[string]any{
		"from": "a1", "to_kind": "agent", "to_target": "a1", "body": "hello",
	}})
	if !send.OK {
		t.Fatalf("send with nil notifier: %s", send.Error)
	}
}
```

Run: `go test ./internal/daemon/ -run TestNilNotifierWritePathIsSafe -race -v`

If this panics, fix `notifyForThread` and `pushSessionAgents` to return early
when `d.n == nil` — do not work around it in the test.

- [ ] **Step 7: Run the full gate**

Run: `just verify`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/store/api.go internal/daemon/
git commit -m "refactor(daemon): extract store.API, split Serve into New + listener

Exports the pluggable-store seam so alternate backends can satisfy it
without importing daemon, and lets lambda mode build a Dispatch target
without binding a unix socket. No behavior change."
```

---

### Task 2: Device identity

**Files:**
- Create: `internal/device/device.go`, `internal/device/device_test.go`
- Modify: `internal/store/models.go`, `internal/store/schema.sql`,
  `internal/store/store.go` (the `migrate` alters list), `internal/store/agents.go`
- Test: `internal/store/agents_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `device.ID() (string, error)`; `store.Agent.DeviceID string` with
  json tag `device_id`, persisted through `RegisterAgent`/`ListAgents`/`GetAgent`.

- [ ] **Step 1: Write the failing test for device.ID**

```go
package device_test

func TestIDIsStableAcrossCalls(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)

	first, err := device.ID()
	if err != nil {
		t.Fatalf("ID: %v", err)
	}
	if first == "" {
		t.Fatal("ID returned empty string")
	}
	second, err := device.ID()
	if err != nil {
		t.Fatalf("second ID: %v", err)
	}
	if first != second {
		t.Fatalf("ID not stable: %q then %q", first, second)
	}
}

func TestIDHonorsEnvOverride(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	t.Setenv("MUSTER_DEVICE_ID", "laptop-fixed")

	got, err := device.ID()
	if err != nil {
		t.Fatalf("ID: %v", err)
	}
	if got != "laptop-fixed" {
		t.Fatalf("override ignored: got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "device-id")); !os.IsNotExist(err) {
		t.Fatal("override should not write the device-id file")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/device/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement `internal/device/device.go`**

```go
// Package device resolves this machine's stable muster device identity.
package device

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/schuettc/muster/internal/paths"
)

// FileName is the device id's filename within paths.Home().
const FileName = "device-id"

// ID returns this device's stable identifier: $MUSTER_DEVICE_ID if set,
// otherwise a UUID generated once and persisted at <MUSTER_HOME>/device-id.
//
// SocketPath cannot serve this purpose — two machines can both have
// /tmp/tmux-501/default — so cross-device wake needs an identifier that is
// unique per machine rather than per tmux server.
func ID() (string, error) {
	if v := strings.TrimSpace(os.Getenv("MUSTER_DEVICE_ID")); v != "" {
		return v, nil
	}
	p := filepath.Join(paths.Home(), FileName)
	if b, err := os.ReadFile(p); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v, nil
		}
	}
	if err := os.MkdirAll(paths.Home(), 0o755); err != nil {
		return "", err
	}
	v := uuid.NewString()
	if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
		return "", err
	}
	return v, nil
}
```

- [ ] **Step 4: Promote the uuid dependency**

`github.com/google/uuid` is currently an indirect dependency. Run:

```bash
go get github.com/google/uuid@v1.6.0
go mod tidy
```

Confirm it moved to the direct `require` block in `go.mod`.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/device/ -race -v`
Expected: PASS.

- [ ] **Step 6: Write the failing store test**

Add to `internal/store/agents_test.go`:

```go
func TestRegisterAgentPersistsDeviceID(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "a1", DeviceID: "dev-abc"}); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	got, ok, err := s.GetAgent("a1")
	if err != nil || !ok {
		t.Fatalf("GetAgent: %v ok=%v", err, ok)
	}
	if got.DeviceID != "dev-abc" {
		t.Fatalf("DeviceID = %q, want dev-abc", got.DeviceID)
	}
}
```

Note: this file is in package `store` (internal tests), so drop the `store.`
qualifier if that is the existing convention in the file.

- [ ] **Step 7: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestRegisterAgentPersistsDeviceID -v`
Expected: FAIL — `unknown field DeviceID`.

- [ ] **Step 8: Add the column**

In `internal/store/models.go`, add to `Agent` after `SessionCreated`:

```go
	// DeviceID identifies the machine this agent registered from — the
	// wake-routing key once a bus spans devices. SocketPath cannot serve
	// this purpose (two machines can both have /tmp/tmux-501/default).
	// '' = unknown (registered before this column existed).
	DeviceID string `json:"device_id"`
```

In `internal/store/schema.sql`, add to the `agents` table:

```sql
    device_id     TEXT NOT NULL DEFAULT '',
```

In `internal/store/store.go`, append to the `alters` slice:

```go
		`ALTER TABLE agents ADD COLUMN device_id TEXT NOT NULL DEFAULT ''`,
```

Then thread `device_id` through the `INSERT`/`ON CONFLICT` in `RegisterAgent`
and through the `SELECT` column lists and `Scan` targets in `ListAgents`,
`GetAgent`, and `DepartStaleSiblings`. Every `SELECT` that builds an `Agent`
must be updated — grep for `session_created` to find them all, since
`device_id` goes in the same position.

- [ ] **Step 9: Run tests to verify they pass**

Run: `go test ./internal/store/ -race`
Expected: PASS, including `TestOpenMigrationIsIdempotent`.

- [ ] **Step 10: Commit**

```bash
git add internal/device/ internal/store/ go.mod go.sum
git commit -m "feat(store): add Agent.DeviceID and internal/device identity

A stable per-machine identifier, persisted at <MUSTER_HOME>/device-id and
overridable via MUSTER_DEVICE_ID. Additive column; local mode simply never
varies it."
```

---

### Task 3: Idempotency key on the wire

**Files:**
- Modify: `internal/proto/proto.go`
- Test: `internal/proto/proto_test.go` (create if absent)

**Interfaces:**
- Consumes: nothing.
- Produces: `proto.Request.IdemKey string` with json tag `idem,omitempty`.

- [ ] **Step 1: Write the failing test**

```go
package proto_test

func TestRequestOmitsEmptyIdemKey(t *testing.T) {
	b, err := json.Marshal(proto.Request{Op: "list_agents"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "idem") {
		t.Fatalf("empty IdemKey must not appear on the wire: %s", b)
	}
}

func TestRequestRoundTripsIdemKey(t *testing.T) {
	b, err := json.Marshal(proto.Request{Op: "send_message", IdemKey: "k-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got proto.Request
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.IdemKey != "k-1" {
		t.Fatalf("IdemKey = %q, want k-1", got.IdemKey)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proto/ -v`
Expected: FAIL — `unknown field IdemKey`.

- [ ] **Step 3: Add the field**

```go
// Request is one operation call. Args are op-specific.
type Request struct {
	Op   string         `json:"op"`
	Args map[string]any `json:"args,omitempty"`
	// IdemKey deduplicates a write across transport retries. It is set only
	// on the daemon→upstream hop in remote mode; local dispatch ignores it,
	// and omitempty keeps it off the wire entirely for local clients, so the
	// format stays compatible with pre-remote binaries.
	IdemKey string `json:"idem,omitempty"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/proto/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proto/
git commit -m "feat(proto): add omitempty IdemKey to Request

Carries a write-deduplication key on the daemon-to-upstream hop. Ignored
by local dispatch and absent from the wire when empty, so the format stays
compatible with pre-remote binaries."
```

---

## Phase 2 — DynamoDB backend

### Task 4: DynamoDB test harness and table bootstrap

**Files:**
- Create: `internal/dynamostore/store.go`, `internal/dynamostore/store_test.go`
- Modify: `justfile`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `dynamostore.Open(ctx context.Context, table string) (*Store, error)`;
  `dynamostore.EnsureTable(ctx context.Context, c *dynamodb.Client, table string) error`;
  test helper `newTestStore(t *testing.T) *Store` that skips when
  `MUSTER_DDB_ENDPOINT` is unset.

**Table design (locked — later tasks depend on these exact key shapes):**

| Entity | PK (S) | SK (N) | GSI1PK (S) | GSI1SK (N) | GSI2PK (S) | GSI2SK (N) |
|---|---|---|---|---|---|---|
| thread meta | `THREAD#<id>` | `0` | `RCPT#<to_kind>#<to_target>` | `<thread id>` | — | — |
| entry | `THREAD#<tid>` | `<entry id>` | `RCPT#<to_kind>#<to_target>` | `<entry id>` | `ENTRIES` | `<entry id>` |
| agent | `AGENT#<alias>` | `0` | `ROSTER` | `0` | — | — |
| kv | `KV#<key>` | `0` | — | — | — | — |
| event | `EVENT#<id>` | `0` | `EVTAGENT#<agent>` | `<id>` | `EVENTS` | `<id>` |
| counter | `COUNTER#<name>` | `0` | — | — | — | — |
| idem | `IDEM#<key>` | `0` | — | — | — | — |

Entries carry the thread's recipient denormalized onto GSI1 so "unread for me"
is a sort-key-bounded query with no join. Thread metadata uses SK `0` so
`GetThread` is one query over `THREAD#<id>` with entries following in id order.

- [ ] **Step 1: Add AWS SDK dependencies**

```bash
go get github.com/aws/aws-sdk-go-v2/config@latest
go get github.com/aws/aws-sdk-go-v2/service/dynamodb@latest
go get github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue@latest
go mod tidy
```

- [ ] **Step 2: Confirm cgo-free still holds, and that the SDK stays out of the binary**

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...   # compiles dynamostore itself
just cross                                              # the shipped binary
go list -deps ./cmd/muster | grep aws                   # must print NOTHING
```

`just cross` alone is not sufficient here: nothing imports `dynamostore` yet, so
the compiler would skip it entirely and the cgo question would go unasked.
`go build ./...` is what actually compiles it.

The `go list` check is the constraint from Global Constraints — the default
binary carries no AWS code. It must print nothing at every task from here on,
not just this one. If any AWS package pulls in cgo, stop and report: that is a
blocking constraint violation, not something to work around.

- [ ] **Step 3: Write the failing test**

```go
package dynamostore

import (
	"context"
	"os"
	"testing"
)

// newTestStore returns a Store against DynamoDB Local, or skips. `just verify`
// must stay dependency-free, so an unset endpoint is a skip, not a failure;
// `just verify-dynamo` is the recipe that guarantees the container is up.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("MUSTER_DDB_ENDPOINT") == "" {
		t.Skip("MUSTER_DDB_ENDPOINT unset; run `just verify-dynamo`")
	}
	table := "muster-test-" + t.Name()
	s, err := Open(context.Background(), table)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.DropTable(context.Background()) })
	return s
}

func TestOpenCreatesTable(t *testing.T) {
	s := newTestStore(t)
	ok, err := s.TableExists(context.Background())
	if err != nil {
		t.Fatalf("TableExists: %v", err)
	}
	if !ok {
		t.Fatal("Open did not create the table")
	}
}
```

Table names must be unique per test so parallel tests do not collide.
`t.Name()` contains `/` for subtests — replace it with `-` when building the
name.

- [ ] **Step 4: Run test to verify it fails**

```bash
docker run -d --rm -p 8000:8000 --name muster-ddb amazon/dynamodb-local
MUSTER_DDB_ENDPOINT=http://localhost:8000 go test ./internal/dynamostore/ -v
```

Expected: FAIL — package does not exist.

- [ ] **Step 5: Implement `store.go`**

Cover: the `Store` struct wrapping `*dynamodb.Client` plus the table name;
`Open` (loads AWS config, honors `MUSTER_DDB_ENDPOINT` via `BaseEndpoint` and
supplies dummy static credentials when it is set, since DynamoDB Local accepts
any); `EnsureTable` (on-demand billing mode, PK `pk` string, SK `sk` number,
GSI1 on `gsi1pk`/`gsi1sk`, GSI2 on `gsi2pk`/`gsi2sk`, both projecting ALL, TTL
enabled on attribute `ttl`, waiting for ACTIVE); `TableExists`; `DropTable`
(test-only, guarded to names starting `muster-test-`); and key-builder helpers:

```go
func pkThread(id int64) string  { return "THREAD#" + strconv.FormatInt(id, 10) }
func pkAgent(alias string) string { return "AGENT#" + alias }
func pkKV(key string) string    { return "KV#" + key }
func pkEvent(id int64) string   { return "EVENT#" + strconv.FormatInt(id, 10) }
func pkCounter(n string) string { return "COUNTER#" + n }
func pkIdem(key string) string  { return "IDEM#" + key }

// rcpt is the GSI1 partition for a thread's recipient. Entries carry their
// thread's rcpt denormalized so unread math is one bounded query.
func rcpt(toKind, toTarget string) string {
	if toKind == "broadcast" {
		return "RCPT#broadcast"
	}
	return "RCPT#" + toKind + "#" + toTarget
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `MUSTER_DDB_ENDPOINT=http://localhost:8000 go test ./internal/dynamostore/ -race -v`
Expected: PASS.

- [ ] **Step 7: Verify the skip path**

Run: `go test ./internal/dynamostore/ -v`
Expected: PASS with `SKIP` — no container required.

- [ ] **Step 8: Add the justfile recipe**

```make
# DynamoDB backend tests — requires Docker. NOT part of `just verify`, which
# must stay fast and dependency-free.
verify-dynamo:
    #!/usr/bin/env bash
    set -euo pipefail
    docker rm -f muster-ddb >/dev/null 2>&1 || true
    docker run -d --rm -p 8000:8000 --name muster-ddb amazon/dynamodb-local >/dev/null
    trap 'docker rm -f muster-ddb >/dev/null 2>&1 || true' EXIT
    for _ in $(seq 1 30); do
      curl -s http://localhost:8000 >/dev/null 2>&1 && break
      sleep 0.5
    done
    MUSTER_DDB_ENDPOINT=http://localhost:8000 go test -race ./internal/dynamostore/... ./internal/storetest/...
```

- [ ] **Step 9: Run both gates**

Run: `just verify && just verify-dynamo`
Expected: both PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/dynamostore/ justfile go.mod go.sum
git commit -m "feat(dynamostore): table bootstrap and DynamoDB Local test harness

Single-table design with two GSIs: GSI1 partitions by recipient for
unread math, GSI2 is the global entry log the device poller reads.
Backend tests skip when MUSTER_DDB_ENDPOINT is unset so \`just verify\`
stays dependency-free; \`just verify-dynamo\` runs them under Docker."
```

---

### Task 5: Counters and agents

**Files:**
- Create: `internal/dynamostore/agents.go`, `internal/dynamostore/agents_test.go`
- Modify: `internal/dynamostore/store.go` (counter helper)

**Interfaces:**
- Consumes: `store.Agent` (Task 2), key helpers (Task 4).
- Produces: `(*Store).nextID(ctx context.Context, name string) (int64, error)`;
  `RegisterAgent`, `ListAgents`, `GetAgent`, `DepartAgent`, `DeleteAgent`,
  `SetSessionLabel`, `DepartStaleSiblings` — signatures exactly as in `store.API`.

- [ ] **Step 1: Write the failing counter test**

```go
func TestNextIDIsMonotonic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seen := map[int64]bool{}
	for i := 0; i < 5; i++ {
		id, err := s.nextID(ctx, "entry")
		if err != nil {
			t.Fatalf("nextID: %v", err)
		}
		if id <= 0 || seen[id] {
			t.Fatalf("nextID returned %d (dup or non-positive)", id)
		}
		seen[id] = true
	}
}

func TestNextIDIsAtomicUnderConcurrency(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const n = 20
	var mu sync.Mutex
	got := map[int64]bool{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := s.nextID(ctx, "entry")
			if err != nil {
				t.Errorf("nextID: %v", err)
				return
			}
			mu.Lock()
			got[id] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(got) != n {
		t.Fatalf("allocated %d distinct ids, want %d — counter is not atomic", len(got), n)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `MUSTER_DDB_ENDPOINT=http://localhost:8000 go test ./internal/dynamostore/ -run TestNextID -race -v`
Expected: FAIL — `undefined: nextID`.

- [ ] **Step 3: Implement `nextID`**

An `UpdateItem` with `ADD n :one` and `ReturnValues: UPDATED_NEW`. Ids must be
**globally** monotonic per counter name, not per thread — `Agent.LastReadEntryID`
is a global watermark and per-thread sequences would silently corrupt unread
math. Counter names in use: `"thread"`, `"entry"`, `"event"`.

- [ ] **Step 4: Run to verify it passes**

Run: `MUSTER_DDB_ENDPOINT=http://localhost:8000 go test ./internal/dynamostore/ -run TestNextID -race -v`
Expected: PASS.

- [ ] **Step 5: Write the failing agents tests**

```go
func TestRegisterAgentUpsertsAndRevivesDeparted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.RegisterAgent(store.Agent{Alias: "a1", Role: "worker", DeviceID: "d1"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.DepartAgent("a1"); err != nil {
		t.Fatalf("depart: %v", err)
	}
	// Re-registering revives the tombstone without disturbing read state.
	if err := s.RegisterAgent(store.Agent{Alias: "a1", Role: "producer", DeviceID: "d1"}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	got, ok, err := s.GetAgent("a1")
	if err != nil || !ok {
		t.Fatalf("GetAgent: %v ok=%v", err, ok)
	}
	if got.Departed {
		t.Fatal("re-register must clear Departed")
	}
	if got.Role != "producer" {
		t.Fatalf("Role = %q, want producer", got.Role)
	}
	_ = ctx
}

func TestListAgentsOrdersByAlias(t *testing.T) {
	s := newTestStore(t)
	for _, a := range []string{"c", "a", "b"} {
		if err := s.RegisterAgent(store.Agent{Alias: a}); err != nil {
			t.Fatalf("register %s: %v", a, err)
		}
	}
	got, err := s.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	var aliases []string
	for _, a := range got {
		aliases = append(aliases, a.Alias)
	}
	want := []string{"a", "b", "c"}
	if !slices.Equal(aliases, want) {
		t.Fatalf("aliases = %v, want %v", aliases, want)
	}
}
```

- [ ] **Step 6: Run to verify they fail**

Run: `MUSTER_DDB_ENDPOINT=http://localhost:8000 go test ./internal/dynamostore/ -run 'TestRegisterAgent|TestListAgents' -race -v`
Expected: FAIL — methods undefined.

- [ ] **Step 7: Implement `agents.go`**

`RegisterAgent` is an `UpdateItem` on `pkAgent(alias)` that sets every mutable
field, forces `departed = false`, sets `registered_at` only via
`if_not_exists`, and leaves `last_read_entry_id`/`last_read_at` untouched —
matching the SQLite `ON CONFLICT` semantics exactly, including that read state
survives the round trip.

`ListAgents` queries GSI1 partition `ROSTER`. DynamoDB has no ORDER BY on a
non-key attribute, and GSI1SK is `0` for all agents, so sort by alias in Go
after the query to match `ORDER BY alias`. Departed agents are included —
their rows are history, not gone.

`DepartAgent` sets `departed = true` and nothing else. `DeleteAgent` removes
the item. `SetSessionLabel` and `DepartStaleSiblings` filter the roster by
`(socket_path, session_id, session_created)` in Go, applying the same
ghost-guard discrimination the SQLite versions use; read
`internal/store/agents.go` and preserve its semantics rather than reinventing
them.

- [ ] **Step 8: Run to verify they pass**

Run: `MUSTER_DDB_ENDPOINT=http://localhost:8000 go test ./internal/dynamostore/ -race -v`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/dynamostore/
git commit -m "feat(dynamostore): atomic id counters and the agent roster

Counter items updated with ADD give globally monotonic ids, which
LastReadEntryID depends on — per-thread sequences would corrupt unread
math. RegisterAgent mirrors the SQLite upsert including departed-revival
and read-state preservation."
```

---

### Task 6: Threads and entries

**Files:**
- Create: `internal/dynamostore/threads.go`, `internal/dynamostore/threads_test.go`

**Interfaces:**
- Consumes: `nextID`, key helpers, `rcpt`.
- Produces: `CreateThread`, `AppendEntry`, `GetThread`, `Inbox`, `Threads`,
  `MarkRead`, `UnreadCount`, `SessionUnread` — signatures exactly as in `store.API`.

- [ ] **Step 1: Write the failing tests**

```go
func TestCreateThreadAndGetThread(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
		Subject: "hi",
	}, "first body")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	th, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.Subject != "hi" || th.ToTarget != "a2" {
		t.Fatalf("thread round trip wrong: %+v", th)
	}
	if len(entries) != 1 || entries[0].Body != "first body" {
		t.Fatalf("entries = %+v, want one with 'first body'", entries)
	}
}

func TestEntriesReturnInIDOrder(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
	}, "one")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	for _, body := range []string{"two", "three"} {
		if _, err := s.AppendEntry(id, "a2", body, ""); err != nil {
			t.Fatalf("AppendEntry %s: %v", body, err)
		}
	}
	_, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	var bodies []string
	for _, e := range entries {
		bodies = append(bodies, e.Body)
	}
	want := []string{"one", "two", "three"}
	if !slices.Equal(bodies, want) {
		t.Fatalf("bodies = %v, want %v", bodies, want)
	}
}

func TestUnreadCountRespectsWatermark(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "a2"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
	}, "unread one"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	n, err := s.UnreadCount("a2")
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("UnreadCount = %d, want 1", n)
	}
	if err := s.MarkRead("a2"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	n, err = s.UnreadCount("a2")
	if err != nil {
		t.Fatalf("UnreadCount after MarkRead: %v", err)
	}
	if n != 0 {
		t.Fatalf("UnreadCount after MarkRead = %d, want 0", n)
	}
}

func TestBroadcastCountsAsUnread(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAgent(store.Agent{Alias: "a2"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "broadcast",
	}, "all hands"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	n, err := s.UnreadCount("a2")
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("broadcast unread = %d, want 1", n)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `MUSTER_DDB_ENDPOINT=http://localhost:8000 go test ./internal/dynamostore/ -run 'Thread|Unread|Broadcast' -race -v`
Expected: FAIL — methods undefined.

- [ ] **Step 3: Implement `threads.go`**

`CreateThread` allocates a thread id and an entry id, then writes both the
metadata item (SK `0`) and the first entry in one `TransactWriteItems` so a
thread never exists without its opening entry.

`AppendEntry` allocates an entry id, writes the entry, and bumps the thread's
`updated_at`. Every entry carries `gsi1pk = rcpt(thread.ToKind, thread.ToTarget)`
and `gsi1sk = <entry id>`, plus `gsi2pk = "ENTRIES"` and `gsi2sk = <entry id>`.
Read the thread metadata to get its recipient before writing.

`GetThread` is one query on `pkThread(id)`, splitting SK `0` (metadata) from the
rest (entries, already in id order).

**CORRECTION (found during Task 6 implementation): this said "three address
forms" and was WRONG.** `threadConcerns` in `internal/store/threads.go` has
**four** arms — addressed to the alias, to its role, broadcast, **or originated
by the alias** (`threads.from_agent = ?`). Its doc comment records why: "the
surfaces diverging is exactly how replies to originated threads once went
invisible." Implementing three arms would have re-introduced originator
blindness on the hosted backend only. Any surface answering "does this thread
concern alias X" needs all four.

`Inbox` queries GSI1 for the address forms that can reach the alias —
`RCPT#agent#<alias>`, `RCPT#role#<role>` for the agent's role, and
`RCPT#broadcast` — collects distinct thread ids, and loads their metadata.

`UnreadCount` queries the same three partitions with `gsi1sk > last_read_entry_id`
and counts entries not authored by the alias itself. `MarkRead` sets the
watermark to the current max entry id. `SessionUnread` sums across the aliases
registered to a `(socket_path, session_id)` pair.

Match `internal/store/threads.go` and `agents.go` semantics exactly — read them
first. Where behavior is ambiguous, the SQLite implementation is the
specification.

- [ ] **Step 4: Run to verify they pass**

Run: `MUSTER_DDB_ENDPOINT=http://localhost:8000 go test ./internal/dynamostore/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dynamostore/
git commit -m "feat(dynamostore): threads, entries, and unread math

Entries carry their thread's recipient denormalized onto GSI1, so unread
for an alias is three sort-key-bounded queries with no join. CreateThread
writes metadata and first entry in one transaction."
```

---

### Task 7: Task claim and transition

**Files:**
- Create: `internal/dynamostore/tasks.go`, `internal/dynamostore/tasks_test.go`

**Interfaces:**
- Consumes: `nextID`, key helpers.
- Produces: `ClaimTask(threadID int64, byAgent string) error`,
  `TransitionTask(threadID int64, byAgent, newStatus, note string) error`.
  Both return `store.ErrNotClaimable` / the same sentinel errors the SQLite
  implementation returns.

- [ ] **Step 1: Write the failing tests**

```go
func TestClaimTaskSucceedsOnceThenFails(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(store.Thread{
		Kind: "task", FromAgent: "a1", ToKind: "role", ToTarget: "worker",
		Status: "open",
	}, "do the thing")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := s.ClaimTask(id, "a2"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := s.ClaimTask(id, "a3"); !errors.Is(err, store.ErrNotClaimable) {
		t.Fatalf("second claim err = %v, want ErrNotClaimable", err)
	}
}

func TestClaimTaskIsAtomicUnderConcurrency(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(store.Thread{
		Kind: "task", FromAgent: "a1", ToKind: "role", ToTarget: "worker",
		Status: "open",
	}, "race me")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	const n = 8
	var wins int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.ClaimTask(id, fmt.Sprintf("a%d", i)); err == nil {
				atomic.AddInt64(&wins, 1)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d concurrent claims succeeded, want exactly 1", wins)
	}
}

func TestClaimTaskRecordsStatusChangeEntry(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateThread(store.Thread{
		Kind: "task", FromAgent: "a1", ToKind: "role", ToTarget: "worker",
		Status: "open",
	}, "do the thing")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := s.ClaimTask(id, "a2"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	_, entries, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	last := entries[len(entries)-1]
	if last.StatusChange != "claimed" || last.FromAgent != "a2" {
		t.Fatalf("last entry = %+v, want claimed by a2", last)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `MUSTER_DDB_ENDPOINT=http://localhost:8000 go test ./internal/dynamostore/ -run TestClaimTask -race -v`
Expected: FAIL — methods undefined.

- [ ] **Step 3: Implement `tasks.go`**

`ClaimTask` is the SQLite compare-and-swap expressed as a condition:

- Allocate an entry id.
- `TransactWriteItems` with two operations: an `Update` on the thread metadata
  setting `status = "claimed"` and `updated_at`, guarded by
  `ConditionExpression: #status = :open`; and a `Put` of the status-change entry.
- On `TransactionCanceledException` whose cancellation reason is
  `ConditionalCheckFailed`, return `store.ErrNotClaimable`. Any other error
  propagates.

Do **not** implement this as read-then-write. The condition expression is the
whole correctness mechanism — with no reserved-concurrency-1 backstop, a
read-then-write here lets two agents claim the same task.

`TransitionTask` validates `newStatus` against `store.TaskStates` before
touching DynamoDB (same as SQLite), then updates status and appends the
status-change entry in one transaction.

- [ ] **Step 4: Run to verify they pass**

Run: `MUSTER_DDB_ENDPOINT=http://localhost:8000 go test ./internal/dynamostore/ -race -v`
Expected: PASS, including the concurrency test.

- [ ] **Step 5: Commit**

```bash
git add internal/dynamostore/
git commit -m "feat(dynamostore): task claim/transition via conditional writes

ClaimTask expresses the SQLite compare-and-swap as a ConditionExpression
inside a TransactWriteItems with the status-change entry. This is the only
thing preventing double-claims — there is no single-writer backstop."
```

---

### Task 8: KV, events, and idempotency records

**Files:**
- Create: `internal/dynamostore/kv.go`, `internal/dynamostore/events.go`,
  `internal/dynamostore/idem.go`, and their `_test.go` files
- Modify: `internal/store/api.go`, `internal/store/schema.sql`,
  `internal/store/store.go`, and add `internal/store/idem.go`

**Interfaces:**
- Consumes: key helpers, `nextID`.
- Produces: `KVSet`, `KVGet`, `AppendEvent`, `Events`, `MaxEventID`,
  `PruneEvents`; plus two new methods added to `store.API` and implemented by
  **both** backends:

```go
// IdemBegin claims key for a first delivery. found=false means this caller
// owns execution and must call IdemComplete. found=true with done=true means
// the op already ran and resp is its recorded response. found=true with
// done=false means an identical request is in flight.
IdemBegin(key string) (resp []byte, done bool, found bool, err error)
IdemComplete(key string, resp []byte) error
```

- [ ] **Step 1: Write the failing idempotency tests**

Place these in `internal/storetest/` so they run against both backends (the
harness lands in Task 9; for now put them in `internal/dynamostore/idem_test.go`
and move them in Task 9).

```go
func TestIdemBeginClaimsThenReportsDone(t *testing.T) {
	s := newTestStore(t)
	if _, _, found, err := s.IdemBegin("k1"); err != nil || found {
		t.Fatalf("first IdemBegin: found=%v err=%v, want found=false", found, err)
	}
	if _, done, found, err := s.IdemBegin("k1"); err != nil || !found || done {
		t.Fatalf("in-flight IdemBegin: found=%v done=%v err=%v, want found=true done=false", found, done, err)
	}
	if err := s.IdemComplete("k1", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("IdemComplete: %v", err)
	}
	resp, done, found, err := s.IdemBegin("k1")
	if err != nil || !found || !done {
		t.Fatalf("completed IdemBegin: found=%v done=%v err=%v", found, done, err)
	}
	if string(resp) != `{"ok":true}` {
		t.Fatalf("recorded response = %s", resp)
	}
}

func TestIdemBeginIsAtomicUnderConcurrency(t *testing.T) {
	s := newTestStore(t)
	const n = 8
	var claims int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, found, err := s.IdemBegin("race"); err == nil && !found {
				atomic.AddInt64(&claims, 1)
			}
		}()
	}
	wg.Wait()
	if claims != 1 {
		t.Fatalf("%d callers claimed the key, want exactly 1", claims)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `MUSTER_DDB_ENDPOINT=http://localhost:8000 go test ./internal/dynamostore/ -run TestIdem -race -v`
Expected: FAIL — methods undefined.

- [ ] **Step 3: Implement the DynamoDB side**

`kv.go`: `KVSet` is a `PutItem` on `pkKV(key)` (last-write-wins, matching
SQLite). `KVGet` is a `GetItem`.

`events.go`: `AppendEvent` allocates an event id, writes the item with
`gsi1pk = "EVTAGENT#" + agent`, `gsi2pk = "EVENTS"`, and a `ttl` attribute set
to now plus a retention window (default 30 days, exposed as a knob). `Events`
queries GSI1 when the query names an agent and GSI2 otherwise. `MaxEventID`
reads the `event` counter. `PruneEvents` returns `(0, nil)` with a comment
explaining that DynamoDB's native TTL supersedes it on this backend — it must
remain on the interface because the SQLite backend still needs it.

`idem.go`: `IdemBegin` is a `PutItem` on `pkIdem(key)` with
`ConditionExpression: attribute_not_exists(pk)`, `state = "pending"`, and a
`ttl` 24 hours out. A `ConditionalCheckFailedException` means the key exists —
`GetItem` it and report `done` plus the recorded response. `IdemComplete` is an
`UpdateItem` setting `state = "done"` and `resp`.

- [ ] **Step 4: Implement the SQLite side**

Add `internal/store/idem.go` with the same two methods over a new table, so the
interface is honest on both backends and the conformance suite covers it:

```sql
CREATE TABLE IF NOT EXISTS idem (
    key        TEXT PRIMARY KEY,
    state      TEXT NOT NULL,          -- 'pending' | 'done'
    resp       BLOB,
    created_at INTEGER NOT NULL
);
```

Add the `CREATE TABLE` to `schema.sql`. No `ALTER` is needed — `CREATE TABLE IF
NOT EXISTS` in the applied schema covers pre-existing databases. `IdemBegin`
uses `INSERT ... ON CONFLICT DO NOTHING` and checks `RowsAffected` for the
atomic claim. Local mode never populates this table (local clients send no
`IdemKey`), but the implementation must be correct because the conformance suite
runs against it.

Add both methods to `store.API`.

- [ ] **Step 5: Run to verify they pass**

Run: `MUSTER_DDB_ENDPOINT=http://localhost:8000 go test ./internal/dynamostore/ ./internal/store/ -race`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/dynamostore/ internal/store/
git commit -m "feat: kv, events with native TTL, and idempotency records

Adds IdemBegin/IdemComplete to store.API with implementations on both
backends. Events carry a TTL attribute on DynamoDB, which supersedes
PruneEvents there; PruneEvents stays on the interface for SQLite."
```

---

### Task 9: Backend conformance suite

**Files:**
- Create: `internal/storetest/conformance.go`, `internal/storetest/conformance_test.go`
- Modify: move the backend-agnostic tests written in Tasks 5–8 into it

**Interfaces:**
- Consumes: `store.API`, both implementations.
- Produces: `storetest.RunConformance(t *testing.T, newStore func(t *testing.T) store.API)`.

- [ ] **Step 1: Write the harness**

```go
// Package storetest holds the backend conformance suite: one set of behavioral
// tests run against every store.API implementation, so the SQLite and DynamoDB
// backends cannot drift.
package storetest

// RunConformance runs the full behavioral suite against newStore. Each
// subtest gets a fresh store.
func RunConformance(t *testing.T, newStore func(t *testing.T) store.API) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, newStore(t))
		})
	}
}

type conformanceCase struct {
	name string
	fn   func(t *testing.T, s store.API)
}

var cases = []conformanceCase{
	{"RegisterAgentUpsertsAndRevivesDeparted", testRegisterAgentUpsert},
	{"ListAgentsOrdersByAlias", testListAgentsOrder},
	{"CreateThreadAndGetThread", testCreateThread},
	{"EntriesReturnInIDOrder", testEntryOrder},
	{"UnreadCountRespectsWatermark", testUnreadWatermark},
	{"BroadcastCountsAsUnread", testBroadcastUnread},
	{"ClaimTaskSucceedsOnceThenFails", testClaimOnce},
	{"ClaimTaskIsAtomicUnderConcurrency", testClaimAtomic},
	{"ClaimTaskRecordsStatusChangeEntry", testClaimEntry},
	{"IdemBeginClaimsThenReportsDone", testIdemLifecycle},
	{"IdemBeginIsAtomicUnderConcurrency", testIdemAtomic},
	{"KVSetIsLastWriteWins", testKVLastWriteWins},
	{"EventsFilterByAgent", testEventsByAgent},
}
```

Move the test bodies from Tasks 5–8 into `conformance.go` as
`func(t *testing.T, s store.API)` functions. They were written against the
`store.API` surface already, so the move is mechanical.

- [ ] **Step 2: Wire both backends**

```go
func TestSQLiteConformance(t *testing.T) {
	storetest.RunConformance(t, func(t *testing.T) store.API {
		db := filepath.Join(t.TempDir(), "bus.db")
		s, err := store.Open(db)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestDynamoConformance(t *testing.T) {
	if os.Getenv("MUSTER_DDB_ENDPOINT") == "" {
		t.Skip("MUSTER_DDB_ENDPOINT unset; run `just verify-dynamo`")
	}
	storetest.RunConformance(t, func(t *testing.T) store.API {
		table := "muster-test-" + strings.ReplaceAll(t.Name(), "/", "-")
		s, err := dynamostore.Open(context.Background(), table)
		if err != nil {
			t.Fatalf("dynamostore.Open: %v", err)
		}
		t.Cleanup(func() { _ = s.DropTable(context.Background()) })
		return s
	})
}
```

- [ ] **Step 3: Run both**

Run: `just verify` — SQLite conformance passes, Dynamo skips.
Run: `just verify-dynamo` — both pass.

Any divergence is a **DynamoDB bug**, not a reason to weaken the test. The
SQLite implementation is the specification.

- [ ] **Step 4: Commit**

```bash
git add internal/storetest/ internal/dynamostore/ internal/store/
git commit -m "test: backend conformance suite over store.API

One behavioral suite run against both backends so they cannot drift. The
SQLite implementation is the specification; any divergence is a DynamoDB
bug."
```

---

## Phase 3 — Idempotent dispatch

### Task 10: Server-side idempotency wrapper

**Files:**
- Modify: `internal/daemon/daemon.go`
- Create: `internal/daemon/idem_test.go`

**Interfaces:**
- Consumes: `store.API.IdemBegin`/`IdemComplete` (Task 8),
  `proto.Request.IdemKey` (Task 3).
- Produces: `(*Daemon).Dispatch` now deduplicates writes when `IdemKey` is set;
  `daemon.IsWriteOp(op string) bool`.

- [ ] **Step 1: Write the failing test**

```go
func TestDispatchDeduplicatesWritesByIdemKey(t *testing.T) {
	s := newDaemonTestStore(t)
	d := daemon.New(s, nil)

	if r := d.Dispatch(proto.Request{Op: "register_agent", Args: map[string]any{
		"alias": "a1",
	}}); !r.OK {
		t.Fatalf("register: %s", r.Error)
	}
	req := proto.Request{
		Op:      "send_message",
		Args:    map[string]any{"from": "a1", "to_kind": "agent", "to_target": "a1", "body": "once"},
		IdemKey: "k-dup",
	}
	first := d.Dispatch(req)
	if !first.OK {
		t.Fatalf("first send: %s", first.Error)
	}
	second := d.Dispatch(req)
	if !second.OK {
		t.Fatalf("replay: %s", second.Error)
	}

	// The replay must return the original response, and must not have created
	// a second thread.
	threads := d.Dispatch(proto.Request{Op: "list_threads", Args: map[string]any{"limit": 100}})
	if !threads.OK {
		t.Fatalf("list_threads: %s", threads.Error)
	}
	if n := threadCount(t, threads); n != 1 {
		t.Fatalf("replayed write created %d threads, want 1", n)
	}
	if !reflect.DeepEqual(first.Data, second.Data) {
		t.Fatalf("replay returned different data:\n first=%#v\nsecond=%#v", first.Data, second.Data)
	}
}

func TestDispatchIgnoresIdemKeyOnReads(t *testing.T) {
	s := newDaemonTestStore(t)
	d := daemon.New(s, nil)
	// Reads must not consume idempotency records — two identical reads with
	// the same key both execute.
	r1 := d.Dispatch(proto.Request{Op: "list_agents", IdemKey: "k-read"})
	r2 := d.Dispatch(proto.Request{Op: "list_agents", IdemKey: "k-read"})
	if !r1.OK || !r2.OK {
		t.Fatalf("reads failed: %s / %s", r1.Error, r2.Error)
	}
}
```

Write `threadCount` as a small helper that asserts on the shape
`list_threads` actually returns — check the existing daemon tests for how they
decode `Response.Data` and follow that pattern.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/daemon/ -run TestDispatch -race -v`
Expected: FAIL — the replay creates a second thread.

- [ ] **Step 3: Implement the wrapper**

```go
// writeOps are the ops that mutate state. Idempotency applies to all of them
// uniformly rather than a classified subset: several are naturally idempotent
// (kv_set is last-write-wins, register_agent is an upsert) and would not
// strictly need a key, but a uniform rule cannot be got wrong, and the cost is
// about one extra write unit per mutation.
//
// CAS ops need this despite looking idempotent: if a task_claim succeeds but
// its response is lost, a naive replay returns ErrNotClaimable and the original
// caller wrongly concludes it failed.
var writeOps = map[string]bool{
	"register_agent": true, "deregister_agent": true, "purge_agent": true,
	"send_message": true, "task_create": true, "reply": true,
	"task_claim": true, "task_transition": true,
	"kv_set": true, "log_event": true, "set_label": true,
	"prune_events": true,
}

// IsWriteOp reports whether op mutates state.
func IsWriteOp(op string) bool { return writeOps[op] }

// Dispatch executes one request. When req.IdemKey is set on a write op, the
// op runs at most once: a replay returns the recorded response verbatim, and a
// collision with an identical request still in flight returns a retryable
// error. Local clients send no key, so local mode is unaffected.
func (d *Daemon) Dispatch(req proto.Request) proto.Response {
	if req.IdemKey == "" || !IsWriteOp(req.Op) {
		return d.dispatch(req)
	}
	recorded, done, found, err := d.s.IdemBegin(req.IdemKey)
	if err != nil {
		return proto.Response{Error: "idempotency: " + err.Error()}
	}
	if found {
		if !done {
			return proto.Response{Error: "retry: identical request in flight"}
		}
		var resp proto.Response
		if err := json.Unmarshal(recorded, &resp); err != nil {
			return proto.Response{Error: "idempotency: corrupt record: " + err.Error()}
		}
		return resp
	}
	resp := d.dispatch(req)
	if b, err := json.Marshal(resp); err == nil {
		if err := d.s.IdemComplete(req.IdemKey, b); err != nil {
			fmt.Fprintln(os.Stderr, "muster: idem complete:", err)
		}
	}
	return resp
}
```

Rename the existing exported `Dispatch` from Task 1 back to unexported
`dispatch`, so the wrapper above becomes the public entry point. Update the
`handle` call site to call the new `Dispatch`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/daemon/ -race -v`
Expected: PASS.

- [ ] **Step 5: Run the full gate**

Run: `just verify && just verify-dynamo`
Expected: both PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/
git commit -m "feat(daemon): idempotent dispatch for keyed writes

A write carrying an IdemKey runs at most once: replays return the recorded
response verbatim and in-flight collisions return a retryable error. Applies
uniformly to every write op. Local clients send no key, so local mode is
untouched."
```

---

## Phase 4 — Lambda mode

### Task 11: Function URL handler and `muster lambda`

**Files:**
- Create: `internal/lambdamode/lambdamode.go`, `internal/lambdamode/lambdamode_test.go`
- Modify: `cmd/muster/main.go`, `internal/humancli` (usage/help entry)

**Interfaces:**
- Consumes: `daemon.New`, `daemon.Dispatch`, `dynamostore.Open`.
- Produces: `lambdamode.Authenticator` (interface, see Step 4);
  `lambdamode.EnvAuth` (the v1 implementation, exported so tests can use it);
  `lambdamode.Handler(d *daemon.Daemon, auth Authenticator) func(context.Context, events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error)`;
  `lambdamode.Run() int`.

**Note on the test code below:** it is written as `lambdamode.Handler(d)` for
readability. Every one of those calls is `lambdamode.Handler(d, lambdamode.EnvAuth{})`
in the real test file — `EnvAuth` reads the `MUSTER_TOKEN` values the tests set
with `t.Setenv`.

- [ ] **Step 1: Add the Lambda dependency**

```bash
go get github.com/aws/aws-lambda-go@latest
go mod tidy
just cross
```

`just cross` must still pass — the Lambda runtime library is pure Go.

- [ ] **Step 2: Write the failing test**

```go
// authed is the header set every handler test needs; the handler rejects
// anything else with 401 before it reaches Dispatch.
func authed() map[string]string {
	return map[string]string{"authorization": "Bearer good-token"}
}

func TestHandlerDispatchesRequestBody(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	s := newLambdaTestStore(t) // a *store.Store is fine; this tests the adapter
	d := daemon.New(s, nil)
	h := lambdamode.Handler(d)

	body, _ := json.Marshal(proto.Request{Op: "list_agents"})
	resp, err := h(context.Background(), events.LambdaFunctionURLRequest{
		Body: string(body), Headers: authed(),
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got proto.Response
	if err := json.Unmarshal([]byte(resp.Body), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !got.OK {
		t.Fatalf("dispatch failed: %s", got.Error)
	}
}

func TestHandlerRejectsMalformedBody(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	d := daemon.New(newLambdaTestStore(t), nil)
	h := lambdamode.Handler(d)
	resp, err := h(context.Background(), events.LambdaFunctionURLRequest{
		Body: "{not json", Headers: authed(),
	})
	if err != nil {
		t.Fatalf("handler must not return a transport error for a bad body: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandlerRejectsMissingOrWrongToken(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	d := daemon.New(newLambdaTestStore(t), nil)
	h := lambdamode.Handler(d)
	body, _ := json.Marshal(proto.Request{Op: "list_agents"})

	for _, tc := range []struct {
		name string
		hdr  map[string]string
	}{
		{"missing", nil},
		{"wrong", map[string]string{"authorization": "Bearer nope"}},
		{"malformed", map[string]string{"authorization": "good-token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := h(context.Background(), events.LambdaFunctionURLRequest{
				Body: string(body), Headers: tc.hdr,
			})
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if resp.StatusCode != 401 {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

func TestHandlerAcceptsPreviousToken(t *testing.T) {
	// Rotation overlap: a device still holding the old token must keep working
	// until it is rolled forward. Without this, rotating breaks every device
	// at once, which means it never happens.
	t.Setenv("MUSTER_TOKEN", "new-token")
	t.Setenv("MUSTER_TOKEN_PREVIOUS", "old-token")
	d := daemon.New(newLambdaTestStore(t), nil)
	h := lambdamode.Handler(d)
	body, _ := json.Marshal(proto.Request{Op: "list_agents"})

	for _, tok := range []string{"new-token", "old-token"} {
		resp, err := h(context.Background(), events.LambdaFunctionURLRequest{
			Body:    string(body),
			Headers: map[string]string{"authorization": "Bearer " + tok},
		})
		if err != nil {
			t.Fatalf("handler with %s: %v", tok, err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("%s rejected: status %d", tok, resp.StatusCode)
		}
	}
}

func TestHandlerRejectsOversizedBody(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	d := daemon.New(newLambdaTestStore(t), nil)
	h := lambdamode.Handler(d)
	resp, err := h(context.Background(), events.LambdaFunctionURLRequest{
		Body:    strings.Repeat("x", 8*1024*1024),
		Headers: map[string]string{"authorization": "Bearer good-token"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if resp.StatusCode != 413 {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestHandlerDecodesBase64Body(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	d := daemon.New(newLambdaTestStore(t), nil)
	h := lambdamode.Handler(d)
	body, _ := json.Marshal(proto.Request{Op: "list_agents"})
	resp, err := h(context.Background(), events.LambdaFunctionURLRequest{
		Body:            base64.StdEncoding.EncodeToString(body),
		IsBase64Encoded: true,
		Headers:         authed(),
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 — base64 bodies must be decoded", resp.StatusCode)
	}
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/lambdamode/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 4: Implement `lambdamode.go`**

`Handler` runs these checks in order, and the order matters — each one is
cheaper than the next, and none of them may reach DynamoDB:

1. **Authenticate.** Read the `authorization` header (Function URL headers
   arrive lowercased; do not assume canonical casing), require the `Bearer `
   prefix, and compare the remainder against `MUSTER_TOKEN` and, if set,
   `MUSTER_TOKEN_PREVIOUS`, using `crypto/subtle.ConstantTimeCompare`. Anything
   else is HTTP 401. Two accepted tokens exist so a rotation can roll across
   devices before the old one retires; with one token, rotating breaks every
   device simultaneously.
2. **Cap the body.** Reject bodies over 6MB with HTTP 413 before parsing.
   Lambda's own payload limit is 6MB, so this is about not spending CPU on
   something that cannot be legitimate.
3. **Decode and dispatch.** Honor `IsBase64Encoded`, unmarshal to
   `proto.Request`, call `d.Dispatch`, marshal the `proto.Response` back with
   `Content-Type: application/json`.

A malformed body returns HTTP 400 with a `proto.Response` carrying the error —
**not** a Go error, which Lambda would render as a 502 and lose the message.

If `MUSTER_TOKEN` is unset, `Handler` must reject *everything* with 401 rather
than allowing unauthenticated access. A misconfigured deployment must fail
closed: this endpoint is publicly reachable, so an empty-token fallback would
silently expose the whole bus.

**Put authentication behind a seam.** A shared bearer token is explicitly a
first step, not the destination — the spec's "This is a first step" section
records the planned upgrade to per-device tokens held in DynamoDB, then OIDC.
Resolve credentials through an interface rather than reading the environment
inline, so that upgrade is a one-file change instead of a rewrite of the request
path:

```go
// Authenticator decides whether a presented bearer token is valid. The v1
// implementation compares against MUSTER_TOKEN / MUSTER_TOKEN_PREVIOUS; the
// planned successor looks up per-device hashed tokens in DynamoDB so a single
// device can be revoked without rotating the fleet.
type Authenticator interface {
	Valid(ctx context.Context, token string) bool
}

// EnvAuth is the v1 implementation: constant-time comparison against
// MUSTER_TOKEN and, during rotation, MUSTER_TOKEN_PREVIOUS. Exported so tests
// can construct it. Valid returns false when MUSTER_TOKEN is unset — a
// misconfigured deployment fails closed.
type EnvAuth struct{}
```

`Handler` takes an `Authenticator` as its second parameter; `Run` wires
`EnvAuth{}`.

A dispatch that returns `Response.OK == false` still returns HTTP 200: the
protocol carries its own error channel, and the client must see the
`proto.Response` rather than a transport failure. The one exception is the
in-flight idempotency collision, which must return HTTP 409 so the transport's
retry policy can distinguish it.

`Run` wires the process: read `MUSTER_DDB_TABLE`, `dynamostore.Open`,
`daemon.New(s, nil)`, `lambda.Start(Handler(d))`. All diagnostics to stderr.

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./internal/lambdamode/ -race -v`
Expected: PASS.

- [ ] **Step 6: Wire the mode into `cmd/muster` behind the build tag**

`main.go` gets the routing case and calls an indirection — it must **not**
import `lambdamode`, or the AWS SDK lands in every binary:

```go
	case "lambda":
		if wantsHelp(os.Args[2:]) {
			_ = humancli.HelpFor("lambda", os.Stdout)
			return
		}
		os.Exit(runLambda())
```

`cmd/muster/lambda_on.go`:

```go
//go:build lambda

package main

import "github.com/schuettc/muster/internal/lambdamode"

// runLambda serves the AWS Lambda runtime. Built only under the `lambda` tag:
// it is the sole path that pulls the AWS SDK in, and the default binary that
// every device runs must not carry it.
func runLambda() int { return lambdamode.Run() }
```

`cmd/muster/lambda_off.go`:

```go
//go:build !lambda

package main

import (
	"fmt"
	"os"
)

// runLambda reports that this binary was built without lambda mode. The AWS
// SDK ships only in the Lambda release artifact (built with `-tags lambda`),
// so the default binary omits it entirely — see the plan's Global Constraints.
func runLambda() int {
	fmt.Fprintln(os.Stderr, "muster: this binary was built without lambda mode "+
		"(rebuild with -tags lambda; the released muster-lambda-*.zip already has it)")
	return 2
}
```

Add the corresponding `humancli` help entry so `muster lambda --help` works and
usage stays canonical — the file comment in `main.go` warns that a second
subcommand list once shipped a release whose usage advertised a command `main()`
refused to route.

- [ ] **Step 7: Add the tagged build to `just cross`**

The tagged configuration must compile in CI or it will rot silently. Append to
the `cross` recipe, after the existing loop:

```make
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags lambda -ldflags "{{ ldflags }}" -o /dev/null ./cmd/muster
```

- [ ] **Step 8: Verify the default binary stayed small**

```bash
just build && stat -f %z bin/muster    # macOS; use `stat -c %s` on Linux
```

Expected: within a few hundred KB of the 17,075,090-byte baseline in Global
Constraints. A jump toward 25MB means an AWS import leaked into the untagged
build — find it with `go list -deps ./cmd/muster | grep aws` (which must print
nothing) rather than accepting the size.

- [ ] **Step 9: Run the full gate**

Run: `just verify`
Expected: PASS, including both `cross` configurations.

- [ ] **Step 10: Commit**

```bash
git add internal/lambdamode/ cmd/muster/ internal/humancli/ justfile go.mod go.sum
git commit -m "feat(lambda): Function URL adapter over daemon.Dispatch

The Lambda is a transport adapter, not a second implementation: body in,
proto.Request out, Dispatch, proto.Response back. Protocol errors stay
HTTP 200 so the client sees them; only in-flight idempotency collisions
surface as 409 for the retry policy."
```

---

## Phase 5 — Remote mode

### Task 12: The upstream transport

**Files:**
- Create: `internal/remote/remote.go`, `internal/remote/remote_test.go`

**Interfaces:**
- Consumes: `proto.Request`/`proto.Response`, `daemon.IsWriteOp`.
- Produces: `remote.New(url, token string, opts ...Option) (*Client, error)`;
  `(*Client).Call(ctx context.Context, req proto.Request) (proto.Response, error)`;
  `remote.ReadToken() (string, error)`.

**No AWS dependency.** This package must not import any `aws-sdk-go-v2`
package. A device in remote mode needs no AWS credentials, no region, and no
profile — it POSTs JSON over HTTPS with a bearer token. If you find yourself
reaching for the SDK here, stop: that is the requirement this design exists to
satisfy.

- [ ] **Step 1: Write the failing tests**

```go
func TestCallAttachesIdemKeyToWrites(t *testing.T) {
	var seen []proto.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req proto.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen = append(seen, req)
		_ = json.NewEncoder(w).Encode(proto.Response{OK: true})
	}))
	t.Cleanup(srv.Close)

	c, err := remote.New(srv.URL, "test-token")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Call(context.Background(), proto.Request{Op: "send_message"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(seen) != 1 || seen[0].IdemKey == "" {
		t.Fatalf("write op sent without an IdemKey: %+v", seen)
	}
}

func TestCallOmitsIdemKeyOnReads(t *testing.T) {
	var seen []proto.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req proto.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen = append(seen, req)
		_ = json.NewEncoder(w).Encode(proto.Response{OK: true})
	}))
	t.Cleanup(srv.Close)

	c, _ := remote.New(srv.URL, "test-token")
	if _, err := c.Call(context.Background(), proto.Request{Op: "list_agents"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if seen[0].IdemKey != "" {
		t.Fatalf("read op carried an IdemKey: %q", seen[0].IdemKey)
	}
}

func TestCallReusesIdemKeyAcrossRetries(t *testing.T) {
	var keys []string
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req proto.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		keys = append(keys, req.IdemKey)
		n++
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(proto.Response{OK: true})
	}))
	t.Cleanup(srv.Close)

	c, _ := remote.New(srv.URL, "test-token", remote.WithBackoff(time.Millisecond))
	if _, err := c.Call(context.Background(), proto.Request{Op: "send_message"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("got %d attempts, want 3", len(keys))
	}
	if keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatalf("retries used different keys: %v — this duplicates writes", keys)
	}
}

func TestCallRetriesOn409InFlight(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(proto.Response{OK: true})
	}))
	t.Cleanup(srv.Close)

	c, _ := remote.New(srv.URL, "test-token", remote.WithBackoff(time.Millisecond))
	resp, err := c.Call(context.Background(), proto.Request{Op: "send_message"})
	if err != nil || !resp.OK {
		t.Fatalf("409 must be retried: err=%v resp=%+v", err, resp)
	}
}

func TestCallSendsBearerToken(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(proto.Response{OK: true})
	}))
	t.Cleanup(srv.Close)

	c, _ := remote.New(srv.URL, "test-token")
	if _, err := c.Call(context.Background(), proto.Request{Op: "list_agents"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if auth != "Bearer test-token" {
		t.Fatalf("Authorization = %q, want %q", auth, "Bearer test-token")
	}
}

func TestCallDoesNotRetry401(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c, _ := remote.New(srv.URL, "wrong", remote.WithBackoff(time.Millisecond))
	if _, err := c.Call(context.Background(), proto.Request{Op: "list_agents"}); err == nil {
		t.Fatal("expected an error on 401")
	}
	if n != 1 {
		t.Fatalf("401 was retried %d times — a bad token is permanent, not transient", n)
	}
}

func TestReadTokenRejectsLoosePermissions(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)

	p := filepath.Join(dir, "remote-token")
	if err := os.WriteFile(p, []byte("secret\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := remote.ReadToken(); err == nil {
		t.Fatal("world-readable token file must be rejected")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	got, err := remote.ReadToken()
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}
	if got != "secret" {
		t.Fatalf("token = %q, want secret (trailing newline must be trimmed)", got)
	}
}

func TestCallSurfacesUnreachableUpstreamPromptly(t *testing.T) {
	c, _ := remote.New("http://127.0.0.1:1", "test-token",
		remote.WithBackoff(time.Millisecond), remote.WithTimeout(200*time.Millisecond))
	start := time.Now()
	if _, err := c.Call(context.Background(), proto.Request{Op: "list_agents"}); err == nil {
		t.Fatal("expected an error from an unreachable upstream")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s — an unreachable upstream must fail fast, not hang", time.Since(start))
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/remote/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement `remote.go`**

The client holds a URL, a bearer token, an `*http.Client` with a timeout, and a
backoff base. `Option` values cover `WithBackoff` and `WithTimeout`. There is no
signer and no credential provider — see the no-AWS-dependency note above.

`ReadToken` reads `<paths.Home()>/remote-token`, trims surrounding whitespace,
and **fails if the file's mode is not 0600**. The token is deliberately not an
environment variable: muster runs alongside coding agents that read their own
environment, so `MUSTER_REMOTE_TOKEN` would be one `env` call away from an agent
transcript. Refusing loose permissions is what keeps the file alternative
actually safer rather than merely different.

`Call` sets `Authorization: Bearer <token>` on every request. It generates one
`IdemKey` per logical call — via `uuid.NewString()`, only when
`daemon.IsWriteOp(req.Op)` — and **holds it constant across every retry of that
call**. This is the entire point: a fresh key per attempt would duplicate
writes, which is the failure this design exists to prevent.

Retry on connection errors, HTTP 5xx, 429, and 409 (the in-flight idempotency
collision), with exponential backoff and a cap. Do **not** retry any other 4xx.
401 in particular is a permanent misconfiguration — retrying it just burns
Lambda invocations against a token that will never work.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/remote/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/remote/ go.mod go.sum
git commit -m "feat(remote): bearer-token upstream transport with idempotent retries

One IdemKey per logical write, held constant across every retry of that
call — a fresh key per attempt would duplicate writes, which is the exact
failure idempotency exists to prevent."
```

---

### Task 13: Remote-mode daemon and badge reconciliation

**Files:**
- Modify: `internal/daemon/daemon.go`, `cmd/muster/main.go`
- Create: `internal/daemon/remote_test.go`

**Interfaces:**
- Consumes: `remote.Client`, `device.ID`.
- Produces: `daemon.ServeRemote(socketPath string, up Upstream, n wake.Notifier, deviceID string) (*Daemon, error)`;
  `daemon.Upstream` interface (`Call(context.Context, proto.Request) (proto.Response, error)`);
  `(*Daemon).ReconcileLocalSessions()`.

- [ ] **Step 1: Write the failing test**

```go
type fakeUpstream struct {
	mu   sync.Mutex
	reqs []proto.Request
	resp proto.Response
}

func (f *fakeUpstream) Call(_ context.Context, req proto.Request) (proto.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return f.resp, nil
}

func TestRemoteModeForwardsRequestsUpstream(t *testing.T) {
	home, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", home)
	t.Setenv("MUSTER_NO_AUTOSPAWN", "1")

	up := &fakeUpstream{resp: proto.Response{OK: true}}
	sock := filepath.Join(home, "sock")
	d, err := daemon.ServeRemote(sock, up, nil, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	resp, err := client.Call(sock, proto.Request{Op: "list_agents"})
	if err != nil {
		t.Fatalf("client.Call: %v", err)
	}
	if !resp.OK {
		t.Fatalf("response not OK: %s", resp.Error)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.reqs) != 1 || up.reqs[0].Op != "list_agents" {
		t.Fatalf("upstream saw %+v, want one list_agents", up.reqs)
	}
}

func TestRemoteModeDoesNotDispatchLocally(t *testing.T) {
	// A remote-mode daemon has no store. If it dispatched locally it would
	// panic on a nil store rather than forwarding — this asserts the write
	// path goes upstream too.
	home, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", home)
	t.Setenv("MUSTER_NO_AUTOSPAWN", "1")

	up := &fakeUpstream{resp: proto.Response{OK: true}}
	sock := filepath.Join(home, "sock")
	d, err := daemon.ServeRemote(sock, up, nil, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if _, err := client.Call(sock, proto.Request{Op: "send_message", Args: map[string]any{
		"from": "a1", "to_kind": "agent", "to_target": "a2", "body": "x",
	}}); err != nil {
		t.Fatalf("client.Call: %v", err)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.reqs) != 1 || up.reqs[0].Op != "send_message" {
		t.Fatalf("write did not go upstream: %+v", up.reqs)
	}
}
```

Note `MUSTER_NO_AUTOSPAWN` — the existing escape hatch in
`internal/client/client.go`. Without it, a failed dial spawns the compiled test
binary with `serve` and recursively re-runs the suite. That fork bomb has been
observed in CI; set it in every test that touches the socket.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/daemon/ -run TestRemoteMode -race -v`
Expected: FAIL — `undefined: daemon.ServeRemote`.

- [ ] **Step 3: Implement remote mode**

Add the `Upstream` interface and an `up Upstream` plus `deviceID string` field
to `Daemon`. `ServeRemote` binds the socket exactly like `Serve` but leaves
`d.s` nil and sets `d.up`.

In `handle`, route by mode: when `d.up != nil`, forward the whole request
upstream and return the response, then fire `ReconcileLocalSessions` in a
goroutine if `IsWriteOp(req.Op)`. Otherwise dispatch locally as today.

`ReconcileLocalSessions` iterates the `(socket_path, session_id)` pairs with
live local agents, and for each takes the **local** `sessionLock`, calls
`session_unread` upstream, and writes the tmux option through the notifier. This
is `sessionLock` moving from a server-side concern to a local-daemon one, which
is where it belonged: `tmuxenv` confirms `socket_path` is the tmux server
socket, so the key is device-scoped and two devices cannot contend.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/daemon/ -race -v`
Expected: PASS.

- [ ] **Step 5: Wire backend selection in `cmd/muster/main.go`**

```go
// runServe runs the daemon until SIGINT/SIGTERM, returning the process exit
// code. MUSTER_BACKEND selects local (default) or remote; local mode touches
// no AWS code at all.
func runServe() int {
	if err := os.MkdirAll(paths.Home(), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "muster: mkdir:", err)
		return 1
	}
	notifier := wake.NewTmuxNotifier("@muster_inbox", 500*time.Millisecond)

	var d *daemon.Daemon
	switch os.Getenv("MUSTER_BACKEND") {
	case "", "local":
		s, err := store.Open(paths.DBPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, "muster: open store:", err)
			return 1
		}
		defer func() { _ = s.Close() }()
		d, err = daemon.Serve(paths.SocketPath(), s, notifier)
		if err != nil {
			fmt.Fprintln(os.Stderr, "muster: serve:", err)
			return 1
		}
	case "remote":
		url := os.Getenv("MUSTER_REMOTE_URL")
		if url == "" {
			fmt.Fprintln(os.Stderr, "muster: MUSTER_BACKEND=remote requires MUSTER_REMOTE_URL")
			return 1
		}
		token, err := remote.ReadToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, "muster: remote token:", err)
			return 1
		}
		up, err := remote.New(url, token)
		if err != nil {
			fmt.Fprintln(os.Stderr, "muster: remote:", err)
			return 1
		}
		id, err := device.ID()
		if err != nil {
			fmt.Fprintln(os.Stderr, "muster: device id:", err)
			return 1
		}
		d, err = daemon.ServeRemote(paths.SocketPath(), up, notifier, id)
		if err != nil {
			fmt.Fprintln(os.Stderr, "muster: serve:", err)
			return 1
		}
	default:
		fmt.Fprintf(os.Stderr, "muster: unknown MUSTER_BACKEND %q (want local or remote)\n", os.Getenv("MUSTER_BACKEND"))
		return 1
	}
	defer func() { _ = d.Close() }()
	fmt.Fprintln(os.Stderr, "muster daemon listening at", paths.SocketPath())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	return 0
}
```

- [ ] **Step 6: Run the full gate**

Run: `just verify`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/daemon/ cmd/muster/
git commit -m "feat(daemon): remote mode forwards upstream, wakes locally

The local daemon keeps the unix socket and tmux wake and forwards whole
proto.Requests upstream. sessionLock moves to the local daemon, where its
(socket_path, session_id) key is device-scoped by construction."
```

---

### Task 14: `device_poll` and the poller

**Files:**
- Create: `internal/store/poll.go`, `internal/dynamostore/poll.go`,
  `internal/daemon/poller.go`, `internal/daemon/poller_test.go`
- Modify: `internal/store/api.go`, `internal/daemon/daemon.go` (dispatch case)

**Interfaces:**
- Consumes: `store.API`, `Upstream`, `device.ID`.
- Produces:

```go
// SessionRef identifies one tmux session on one device.
type SessionRef struct {
	SocketPath string `json:"socket_path"`
	SessionID  string `json:"session_id"`
}

// DevicePollResult is what a device needs to know after a poll: which of its
// sessions have new mail, and the watermark to resume from.
type DevicePollResult struct {
	MaxEntryID int64        `json:"max_entry_id"`
	Sessions   []SessionRef `json:"sessions"`
}
```

plus `store.API.DevicePoll(deviceID string, sinceEntryID int64) (DevicePollResult, error)`,
the `device_poll` dispatch case, and `(*Daemon).StartPoller(interval time.Duration)`.

- [ ] **Step 1: Write the failing store test**

Add to the conformance suite so both backends are covered:

```go
func testDevicePollFindsNewMail(t *testing.T, s store.API) {
	if err := s.RegisterAgent(store.Agent{
		Alias: "a2", DeviceID: "dev-1",
		SocketPath: "/tmp/tmux-501/default", SessionID: "$1",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	before, err := s.DevicePoll("dev-1", 0)
	if err != nil {
		t.Fatalf("DevicePoll: %v", err)
	}
	if len(before.Sessions) != 0 {
		t.Fatalf("no mail yet, got sessions %+v", before.Sessions)
	}
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
	}, "wake up"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	after, err := s.DevicePoll("dev-1", before.MaxEntryID)
	if err != nil {
		t.Fatalf("DevicePoll: %v", err)
	}
	if len(after.Sessions) != 1 || after.Sessions[0].SessionID != "$1" {
		t.Fatalf("sessions = %+v, want one $1", after.Sessions)
	}
	if after.MaxEntryID <= before.MaxEntryID {
		t.Fatal("watermark did not advance")
	}
}

func testDevicePollIgnoresOtherDevices(t *testing.T, s store.API) {
	if err := s.RegisterAgent(store.Agent{
		Alias: "a2", DeviceID: "dev-1",
		SocketPath: "/tmp/tmux-501/default", SessionID: "$1",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.CreateThread(store.Thread{
		Kind: "message", FromAgent: "a1", ToKind: "agent", ToTarget: "a2",
	}, "for dev-1 only"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	got, err := s.DevicePoll("dev-2", 0)
	if err != nil {
		t.Fatalf("DevicePoll: %v", err)
	}
	if len(got.Sessions) != 0 {
		t.Fatalf("dev-2 was told to wake for dev-1's mail: %+v", got.Sessions)
	}
}
```

Register both in the `cases` slice from Task 9.

- [ ] **Step 2: Run to verify they fail**

Run: `just verify-dynamo`
Expected: FAIL — `DevicePoll` undefined.

- [ ] **Step 3: Implement `DevicePoll` on both backends**

The server holds both the roster and the entries, so it does the filtering: find
entries with id greater than `sinceEntryID`, resolve which of them concern an
agent whose `DeviceID` matches, and return the distinct
`(SocketPath, SessionID)` pairs plus the new max entry id. On DynamoDB this
reads GSI2 partition `ENTRIES` with a sort-key lower bound. On SQLite it is a
join. Entries authored by an agent on the polling device still count — the
reconcile is idempotent and special-casing them adds a bug surface for nothing.

**"Concern" here means the full four-arm `threadConcerns` predicate, not just
the recipient.** An entry on a thread the local agent *originated* must wake
that agent, exactly as it lands in their inbox. Reusing the same predicate
Task 6 implemented for `Inbox`/`UnreadCount` is mandatory, not a convenience:
if the poller and the inbox disagree, a peer's reply appears in `muster inbox`
but never lights the pane — silently, and only on the hosted backend. Do not
re-derive the predicate here; call into whatever Task 6 made canonical.

- [ ] **Step 4: Add the dispatch case**

In `Dispatch`'s op switch:

```go
	case "device_poll":
		deviceID, _ := a["device_id"].(string)
		since := int64(argFloat(a, "since_entry_id"))
		res, err := d.s.DevicePoll(deviceID, since)
		if err != nil {
			return proto.Response{Error: err.Error()}
		}
		return proto.Response{OK: true, Data: res}
```

Use whatever numeric-argument helper the surrounding cases already use rather
than introducing `argFloat` if one exists.

- [ ] **Step 5: Write the failing poller test**

```go
func TestPollerReconcilesOnNewMail(t *testing.T) {
	home, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", home)
	t.Setenv("MUSTER_NO_AUTOSPAWN", "1")

	up := &scriptedUpstream{
		pollResults: []store.DevicePollResult{
			{MaxEntryID: 0},
			{MaxEntryID: 7, Sessions: []store.SessionRef{{SocketPath: "/tmp/s", SessionID: "$1"}}},
		},
	}
	n := &recordingNotifier{}
	d, err := daemon.ServeRemote(filepath.Join(home, "sock"), up, n, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	d.StartPoller(5 * time.Millisecond)

	waitFor(t, time.Second, func() bool { return n.Count() > 0 })

	if got := up.LastSince(); got != 7 {
		t.Fatalf("poller resumed from %d, want the advanced watermark 7", got)
	}
}

func TestPollerSkipsWhenNoLocalAgents(t *testing.T) {
	// A device with an empty local roster has nobody to wake and must not
	// poll at all — this is what keeps an idle device free.
	home, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", home)
	t.Setenv("MUSTER_NO_AUTOSPAWN", "1")

	up := &scriptedUpstream{}
	d, err := daemon.ServeRemote(filepath.Join(home, "sock"), up, &recordingNotifier{}, "dev-1")
	if err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	// No register_agent call, so the local roster is empty.
	d.StartPoller(5 * time.Millisecond)
	time.Sleep(60 * time.Millisecond) // several tick intervals

	if n := up.PollCount(); n != 0 {
		t.Fatalf("idle device made %d upstream poll calls, want 0", n)
	}
}
```

This is the one test where a bare sleep is correct: it asserts that *nothing*
happens, so there is no predicate to wait on. Keep the sleep short and the tick
interval much shorter, so several ticks elapse within it.

Write these three helpers in the test file, with exactly these methods, since
the tests above call them:

- `scriptedUpstream` — implements `daemon.Upstream`. Returns `pollResults[i]`
  for the i-th `device_poll` call (repeating the last once exhausted) and
  `proto.Response{OK: true}` for anything else. Exposes `PollCount() int` and
  `LastSince() int64` (the `since_entry_id` argument of the most recent poll).
  All state guarded by a mutex — the poller runs on its own goroutine and these
  tests run under `-race`.
- `recordingNotifier` — implements `wake.Notifier`, counting calls. Exposes
  `Count() int`, mutex-guarded.
- `waitFor(t *testing.T, limit time.Duration, cond func() bool)` — polls `cond`
  on a short tick until `limit`, then `t.Fatal`s. Use this rather than a bare
  `time.Sleep` for any assertion that something *did* happen; a sleep there is
  both slower and flakier.

- [ ] **Step 6: Run to verify it fails**

Run: `go test ./internal/daemon/ -run TestPoller -race -v`
Expected: FAIL — `undefined: StartPoller`.

- [ ] **Step 7: Implement the poller**

```go
// StartPoller begins polling upstream for mail addressed to this device's
// agents, reconciling local session badges when any arrives. It is the wake
// path for traffic originating on other devices; same-device writes reconcile
// inline and never wait for a tick.
//
// The poller does nothing while this device has no live local agents — an idle
// device costs nothing. On error it logs to stderr and backs off; it never
// takes down the daemon and never blocks the unix socket.
func (d *Daemon) StartPoller(base time.Duration) {
	go d.pollLoop(base)
}
```

The loop tracks `sinceEntryID`, sleeps `base` between ticks, doubles the
interval up to a cap while results are empty, and resets to `base` after a tick
that found mail. Skip the upstream call entirely when the local roster is
empty. On a result with sessions, call `ReconcileLocalSessions` for exactly
those sessions.

Read the interval from `MUSTER_POLL_INTERVAL` (default `10s`) in
`cmd/muster/main.go` and pass it to `StartPoller` after `ServeRemote`.

- [ ] **Step 8: Run to verify it passes**

Run: `go test ./internal/daemon/ -race -v && just verify && just verify-dynamo`
Expected: all PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/store/ internal/dynamostore/ internal/daemon/ internal/storetest/ cmd/muster/
git commit -m "feat: device_poll and the remote-mode poller

The server does the filtering — it holds both the roster and the entries —
so a device gets back exactly the sessions needing reconciliation plus a
watermark, in one round trip. A device with no live local agents does not
poll at all."
```

---

## Phase 6 — Deployment

### Task 15: CloudFormation, release artifact, and docs

**Files:**
- Create: `contrib/cloudformation/muster-backend.yaml`,
  `docs/hosted-backend.md`
- Modify: `.github/workflows/release.yml`, `.github/workflows/ci.yml`,
  `README.md`

**Interfaces:**
- Consumes: everything above.
- Produces: a deployable stack and the operator instructions for it.

- [ ] **Step 1: Write the CloudFormation template**

One stack containing: the DynamoDB table (on-demand billing, PK `pk` string, SK
`sk` number, GSI1 and GSI2 as specified in Task 4, TTL enabled on `ttl`); the
Lambda function (runtime `provided.al2023`, architecture `arm64`, handler
`bootstrap`, env vars `MUSTER_DDB_TABLE` / `MUSTER_TOKEN` /
`MUSTER_TOKEN_PREVIOUS`, memory 256MB, timeout 30s, reserved concurrency 10);
the Function URL with `AuthType: NONE`; and the execution role granting only the
table actions the store uses.

The bearer token is a stack **`Parameter` with `NoEcho: true`**, so it does not
appear in stack events, the console, or `describe-stacks` output.
`MUSTER_TOKEN_PREVIOUS` is a second parameter defaulting to empty, used only
during rotation.

`AuthType: NONE` means this endpoint is publicly reachable and the token is the
only thing protecting it. Say so in a comment at the top of the template — the
next person to read it should not have to infer that from the auth type.

Reserved concurrency of 10 is a **cost guard** — blast radius against a bug that
spins the poller. Comment it as such in the template. It is not a correctness
mechanism and must not be described as one: correctness comes from the
conditional writes in Tasks 5–8.

Add an output for the Function URL, since that is what operators put in
`MUSTER_REMOTE_URL`.

- [ ] **Step 2: Validate the template**

Run: `aws cloudformation validate-template --template-body file://contrib/cloudformation/muster-backend.yaml`
Expected: no errors. If no AWS credentials are available in the working
environment, say so and use `cfn-lint` instead rather than skipping validation.

- [ ] **Step 3: Add the Lambda artifact to the release workflow**

In `.github/workflows/release.yml`, after the existing cross-compilation, build
the Lambda bundle from the linux/arm64 binary:

```yaml
      - name: Build Lambda bundle
        run: |
          CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags lambda \
            -ldflags "$LDFLAGS" -o bootstrap ./cmd/muster
          zip -j muster-lambda-arm64.zip bootstrap
```

The `-tags lambda` is not optional: without it the zip contains a binary whose
`runLambda` is the stub, and the function would fail at runtime with "built
without lambda mode". The released device binaries are built **without** the
tag, which is what keeps the AWS SDK out of them.

Attach `muster-lambda-arm64.zip` to the release alongside the existing binaries
and checksums, and include it in the checksum file.

The Lambda entrypoint is `muster lambda`, but the `provided.al2023` runtime
invokes the binary named `bootstrap` with no arguments. Handle this in
`cmd/muster/main.go`: when `AWS_LAMBDA_FUNCTION_NAME` is set and no subcommand
was given, route to lambda mode rather than printing usage. Add a test for that
dispatch decision.

- [ ] **Step 4: Add the dynamo CI job**

In `.github/workflows/ci.yml`, add a job separate from the `just verify` gate:

```yaml
  dynamo:
    runs-on: ubuntu-latest
    services:
      dynamodb:
        image: amazon/dynamodb-local
        ports: ['8000:8000']
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version-file: go.mod }
      - run: MUSTER_DDB_ENDPOINT=http://localhost:8000 go test -race ./internal/dynamostore/... ./internal/storetest/...
```

`just verify` stays the required gate and remains dependency-free. This job is
additional coverage, not a replacement.

- [ ] **Step 5: Write the operator documentation**

`docs/hosted-backend.md` covers: what this is and what it is not (self-hosted,
one operator, no service anyone else's data flows through); generating a token
(`openssl rand -base64 32`) and deploying the stack with it; setting
`MUSTER_BACKEND=remote` and `MUSTER_REMOTE_URL` on each device and installing
the token at `<MUSTER_HOME>/remote-token` with mode 0600; the full environment
variable table from the spec; expected latency (30–50ms warm, one 200–400ms cold
start after an idle gap); and the cost model.

State plainly that **devices need no AWS credentials** — that is the point of
the token — and equally plainly that **the Function URL is publicly reachable**,
so both the URL and the token should be treated as secrets. Document the
rotation procedure: set `MUSTER_TOKEN_PREVIOUS` to the current token, set
`MUSTER_TOKEN` to the new one, redeploy, roll the devices, then clear
`MUSTER_TOKEN_PREVIOUS` and redeploy again.

**Re-verify the AWS list prices before publishing them.** The spec's table was
built from recalled rates and is explicitly flagged as needing confirmation.
Correct the spec too if they have moved.

Note plainly that there is no migration path from an existing local bus — a
remote bus starts empty.

- [ ] **Step 6: Run the full gate**

Run: `just verify && just verify-dynamo`
Expected: both PASS.

- [ ] **Step 7: Commit**

```bash
git add contrib/ docs/ .github/ README.md
git commit -m "feat: CloudFormation stack, Lambda release artifact, operator docs

One stack: table, function, IAM-authed Function URL, and a least-privilege
execution role. Reserved concurrency of 10 is a cost guard, not a
correctness mechanism."
```

- [ ] **Step 8: Bump VERSION and open the PR**

Bump `VERSION` on the branch — the release workflow tags `v<VERSION>` when the
promotion PR merges to `main`, and a merge that does not bump it releases
nothing. Open the PR from the worktree branch into `dev`.

Before opening it, check the back-merge trap: run
`git log origin/dev..origin/main` and confirm nothing on `main` is missing from
`dev`.

---

## Self-Review Notes

**Spec coverage.** Every section of the design maps to at least one task: modes
and enabling refactors (1, 11, 13), authentication (12, 15), wake and the poller
(13, 14), device identity (2), the DynamoDB model (4–8), concurrency (5, 7),
idempotency (3, 8, 10, 12), configuration and deployment (13, 15), error
handling (12, 14), and testing (4, 9, 15). The spec's "out of scope" list is
carried into Task 15's documentation rather than implemented.

**Known risk.** Task 5's `DepartStaleSiblings` and `SetSessionLabel` are the
least mechanical translations in Phase 2 — they encode the ghost-guard
discrimination added in v0.7.5 and extended by the lineage work in v0.9.0. The
conformance suite in Task 9 is what catches a divergence there; if their
behavior proves hard to express against the roster query, that is the point to
stop and reconsider rather than to approximate.

**Escape hatch.** If Phase 2 proves substantially harder than estimated, the
spec records a small always-on box running the existing binary against SQLite as
a rejected-on-cost-not-merit alternative. It needs no store rewrite at all —
Phase 1 and Phases 4–6 would still apply, with `dynamostore` replaced by the
existing `store`. That is a decision to bring back to the operator, not one to
make mid-plan.
