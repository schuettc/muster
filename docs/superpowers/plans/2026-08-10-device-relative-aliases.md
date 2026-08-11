# Device-Relative Aliases Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A machine's device name is stamped into every alias it mints but never shown on that machine, so local names read short while stored names stay globally unique.

**Architecture:** Two pure string helpers in `internal/device` (add prefix / remove prefix) are the whole mechanism. Minting calls the first; human-facing render sites call the second; alias *inputs* are expanded before they reach `internal/resolve`, which keeps the resolver's device-blind contract intact. Nothing in the store changes — alias remains the primary key.

**Tech Stack:** Go, SQLite (`internal/store`), DynamoDB (`internal/dynamostore`), `tabwriter` CLI rendering, Bubble Tea TUI (`internal/station`), MCP stdio server (`internal/mcpserver`).

**Spec:** `docs/superpowers/specs/2026-08-10-device-relative-aliases-design.md`

## Global Constraints

- No store schema change. Alias stays the primary key in both SQLite and DynamoDB.
- Stripping applies to **human surfaces only**: CLI tables, station TUI, the `@muster_agent` tmux option. Model surfaces (`internal/mcpserver` views, `register_agent`/`validate` detail strings, `internal/humancli/hook.go` injected text) keep the full stored alias.
- Resolution tries `<device-name>-<given>` **before** the literal `given`.
- Every alias this machine mints is seeded — derived, positional arg, `$MUSTER_ALIAS`, `become`, paneless allocation. No exceptions, no `--global` flag.
- The device name is auto-adopted from the sanitized hostname at first registration and pinned to `<MUSTER_HOME>/device-name`. Adoption runs **ahead of** seeding.
- `internal/daemon` is tmux-agnostic by rule (see repo CLAUDE.md). Do not add tmux reads there.
- Repo convention: `gofmt` runs as a pre-commit hook (lefthook). Tests use `t.Setenv("MUSTER_HOME", t.TempDir())` for isolation.

---

### Task 1: Pure prefix helpers in `internal/device`

The seed rule currently lives inside `humancli.seedAlias`, which reads the device name itself. The daemon needs the same rule with an explicitly-passed name (it snapshots the name at startup), so the rule moves to pure functions that take the name as a parameter.

**Files:**
- Create: `internal/device/alias.go`
- Test: `internal/device/alias_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `device.Seed(deviceName, alias string) string` and `device.Strip(deviceName, alias string) string`. Every later task uses one or both.

- [ ] **Step 1: Write the failing tests**

Create `internal/device/alias_test.go`:

```go
package device

import "testing"

// TestSeedIsIdempotent is the property that would bite in production: hooks
// re-register on every session start, and `become` can be handed a full name
// read off another machine. Seeding either must land on the same string.
func TestSeedIsIdempotent(t *testing.T) {
	once := Seed("personal", "dotfiles/main")
	if once != "personal-dotfiles/main" {
		t.Fatalf("Seed = %q, want %q", once, "personal-dotfiles/main")
	}
	if twice := Seed("personal", once); twice != once {
		t.Fatalf("Seed not idempotent: %q then %q", once, twice)
	}
}

// TestSeedLeavesTheDeviceNameItselfAlone keeps a session named after the
// machine from doubling into "personal-personal".
func TestSeedLeavesTheDeviceNameItselfAlone(t *testing.T) {
	if got := Seed("personal", "personal"); got != "personal" {
		t.Fatalf("Seed(%q) = %q, want it untouched", "personal", got)
	}
}

// TestSeedRequiresBothOperands: an empty name means nothing to prefix with,
// and an empty alias is rejected upstream — neither may produce a bare dash.
func TestSeedRequiresBothOperands(t *testing.T) {
	if got := Seed("", "dotfiles/main"); got != "dotfiles/main" {
		t.Fatalf("Seed with no device name = %q, want it untouched", got)
	}
	if got := Seed("personal", ""); got != "" {
		t.Fatalf("Seed of empty alias = %q, want empty", got)
	}
}

// TestStripIsSeedsInverse pins the round trip both ways.
func TestStripIsSeedsInverse(t *testing.T) {
	if got := Strip("personal", "personal-dotfiles/main"); got != "dotfiles/main" {
		t.Fatalf("Strip = %q, want %q", got, "dotfiles/main")
	}
	if got := Strip("personal", Seed("personal", "dotfiles/main")); got != "dotfiles/main" {
		t.Fatalf("Strip(Seed(x)) = %q, want %q", got, "dotfiles/main")
	}
}

// TestStripLeavesForeignAndBareAliases pins the two rows that must render in
// full: another machine's alias, and a legacy row minted before seeding.
func TestStripLeavesForeignAndBareAliases(t *testing.T) {
	if got := Strip("personal", "work-dotfiles/main"); got != "work-dotfiles/main" {
		t.Fatalf("Strip of a foreign alias = %q, want it untouched", got)
	}
	if got := Strip("personal", "dotfiles/main"); got != "dotfiles/main" {
		t.Fatalf("Strip of a bare alias = %q, want it untouched", got)
	}
	if got := Strip("", "personal-dotfiles/main"); got != "personal-dotfiles/main" {
		t.Fatalf("Strip with no device name = %q, want it untouched", got)
	}
}

// TestStripNeverEmptiesAnAlias: an alias that IS the device name has no
// remainder to show, and an empty alias is unaddressable.
func TestStripNeverEmptiesAnAlias(t *testing.T) {
	if got := Strip("personal", "personal"); got != "personal" {
		t.Fatalf("Strip(%q) = %q, want it untouched", "personal", got)
	}
	if got := Strip("personal", "personal-"); got != "personal-" {
		t.Fatalf("Strip of a bare-prefix alias = %q, want it untouched", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/device/ -run 'TestSeed|TestStrip' -v`
Expected: FAIL — `undefined: Seed`, `undefined: Strip`.

- [ ] **Step 3: Write the implementation**

Create `internal/device/alias.go`:

```go
package device

import "strings"

// Seed prefixes alias with deviceName, so two machines cannot mint the same
// one. Derived aliases come from a tmux session name or a directory basename,
// both identical on two machines with the same repos checked out; registration
// is an upsert on the alias primary key, so the second machine to register
// would take the row — and because the roster row IS the identity, it would
// take the inbox and read-state with it.
//
// Seeding is idempotent, and that is load-bearing rather than tidy. Session
// hooks re-register on every session start, and `become` may be handed a full
// alias an operator read off another machine, where it renders unstripped.
// Seeding either input twice must land on the same string, or each pass mints
// a second identity and orphans the previous one's mail.
func Seed(deviceName, alias string) string {
	if deviceName == "" || alias == "" {
		return alias
	}
	if strings.HasPrefix(alias, deviceName+"-") || alias == deviceName {
		return alias
	}
	return deviceName + "-" + alias
}

// Strip removes deviceName's prefix from alias for display on the machine
// that minted it — the one context that already knows its own name.
//
// It needs no per-row device data, which is what keeps this cheap: only this
// machine mints this prefix, so the prefix alone identifies a local row.
// internal/station never decodes device_id at all, and the MCP roster row
// omits it; neither has to change.
//
// An alias equal to the device name, or one that is nothing BUT the prefix, is
// returned untouched: stripping either would leave an empty string, and an
// alias is an address.
func Strip(deviceName, alias string) string {
	if deviceName == "" || alias == "" {
		return alias
	}
	rest, ok := strings.CutPrefix(alias, deviceName+"-")
	if !ok || rest == "" {
		return alias
	}
	return rest
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/device/ -run 'TestSeed|TestStrip' -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/device/alias.go internal/device/alias_test.go
git commit -m "feat(device): pure Seed/Strip alias prefix helpers"
```

---

### Task 2: Auto-adopt the device name

A hostname is not stable — it changes on a network name collision, a domain join, or a restore from backup. The prefix is stamped into the roster at registration, so a drifting hostname silently re-registers a machine under a new prefix and orphans every alias behind the old one. Adoption pins it once.

**Files:**
- Modify: `internal/device/device.go` (add `Adopt`, after `SetName`)
- Test: `internal/device/device_test.go` (append)

**Interfaces:**
- Consumes: `SanitizeName`, `SetName`, `NameConfigured` (all existing in `internal/device/device.go`).
- Produces: `device.Adopt() (name string, adopted bool, err error)` — returns the configured name; `adopted` is true only on the call that wrote the file, so the caller can report it once.

- [ ] **Step 1: Write the failing tests**

Append to `internal/device/device_test.go`:

```go
// TestAdoptPinsTheHostnameOnce is the drift guard: the seed is stamped into
// the roster at registration, so it must not follow a hostname that changes
// after a network collision, a domain join, or a restore.
func TestAdoptPinsTheHostnameOnce(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "")

	name, adopted, err := Adopt()
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !adopted {
		t.Fatal("first Adopt did not report adopting")
	}
	if name == "" {
		t.Fatal("Adopt returned an empty name")
	}
	if got := NameConfigured(); got != name {
		t.Fatalf("NameConfigured after Adopt = %q, want %q", got, name)
	}

	again, adopted, err := Adopt()
	if err != nil {
		t.Fatalf("second Adopt: %v", err)
	}
	if adopted {
		t.Fatal("second Adopt reported adopting again; the name must be pinned")
	}
	if again != name {
		t.Fatalf("second Adopt = %q, want the pinned %q", again, name)
	}
}

// TestAdoptDefersToAConfiguredName: an operator's choice is never overwritten.
func TestAdoptDefersToAConfiguredName(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "")
	if _, err := SetName("personal"); err != nil {
		t.Fatalf("SetName: %v", err)
	}
	name, adopted, err := Adopt()
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if adopted {
		t.Fatal("Adopt overwrote a configured name")
	}
	if name != "personal" {
		t.Fatalf("Adopt = %q, want %q", name, "personal")
	}
}

// TestAdoptHonoursTheEnvOverrideWithoutWriting: $MUSTER_DEVICE_NAME is a
// single-shell override, so it must not be persisted into the file.
func TestAdoptHonoursTheEnvOverrideWithoutWriting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MUSTER_HOME", home)
	t.Setenv("MUSTER_DEVICE_NAME", "throwaway")
	name, adopted, err := Adopt()
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if adopted {
		t.Fatal("Adopt persisted an env override")
	}
	if name != "throwaway" {
		t.Fatalf("Adopt = %q, want %q", name, "throwaway")
	}
	if _, err := os.Stat(filepath.Join(home, NameFileName)); !os.IsNotExist(err) {
		t.Fatal("Adopt wrote device-name while an env override was set")
	}
}
```

If `os` and `path/filepath` are not already imported in this test file, add them to its import block.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/device/ -run TestAdopt -v`
Expected: FAIL — `undefined: Adopt`.

- [ ] **Step 3: Write the implementation**

Add to `internal/device/device.go`, immediately after `SetName`:

```go
// Adopt guarantees this machine has a configured device name, taking the
// sanitized hostname if the operator has not chosen one. It reports the name
// in force and whether this call is what established it, so a caller can
// announce the adoption exactly once.
//
// Pinning matters more than the name itself. A hostname is not stable — macOS
// renames a machine when another on the network already answers to that name,
// and a domain join or a restore from backup can change it too. The device
// name is stamped into every alias at registration, so a seed that followed a
// live hostname would silently start registering a machine under a new prefix
// and orphan every alias and inbox behind the old one. Writing it once makes
// the seed immutable regardless of what the OS later does.
//
// An operator's own choice — the file, or $MUSTER_DEVICE_NAME — is never
// overwritten. The env override is honoured without being persisted: it is a
// single-shell override by contract, and writing it would make a throwaway
// value permanent.
func Adopt() (string, bool, error) {
	if n := NameConfigured(); n != "" {
		return n, false, nil
	}
	host, err := os.Hostname()
	if err != nil {
		return "", false, fmt.Errorf("device: no configured name and no hostname to adopt: %w", err)
	}
	name, err := SetName(host)
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/device/ -v`
Expected: PASS, including the pre-existing device tests.

- [ ] **Step 5: Commit**

```bash
git add internal/device/device.go internal/device/device_test.go
git commit -m "feat(device): adopt and pin the hostname as the device name"
```

---

### Task 3: Every minted alias is seeded

`seedAlias` currently seeds only *derived* aliases, and only when a name is configured. Both carve-outs go. Adoption runs ahead of the seed, which is what lets the `NameConfigured()` gate be deleted rather than widened — without it, dropping the gate would seed against an empty string.

**Files:**
- Modify: `internal/humancli/aliasseed.go` (replace the body of `seedAlias`)
- Modify: `internal/humancli/identity.go:64-73` (seed the arg and `$MUSTER_ALIAS` branches)
- Modify: `internal/humancli/become.go:75` (seed the claim target)
- Test: `internal/humancli/aliasseed_test.go` (rewrite), `internal/humancli/become_test.go` (append)

**Interfaces:**
- Consumes: `device.Seed`, `device.Adopt` (Tasks 1–2).
- Produces: `seedAlias(alias string) string` — now unconditional; every mint site calls it.

- [ ] **Step 1: Rewrite the seed tests**

Replace the whole body of `internal/humancli/aliasseed_test.go`:

```go
package humancli

import "testing"

// TestSeedAliasAppliesToEveryMintedName pins the rule that replaced the
// derived-only carve-out. Once the prefix is hidden locally, a typed name and
// a derived one look identical on screen, so an operator cannot tell which of
// their names is device-scoped and which will collide on a second machine.
// One rule with no exceptions is the only rule that survives being invisible.
func TestSeedAliasAppliesToEveryMintedName(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	for _, in := range []string{"dotfiles/main", "galley/design", "muster-2"} {
		want := "personal-" + in
		if got := seedAlias(in); got != want {
			t.Fatalf("seedAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSeedAliasIsIdempotent: hooks re-register on every session start, and an
// operator may paste back a full alias read off the other machine.
func TestSeedAliasIsIdempotent(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	once := seedAlias("dotfiles/main")
	if twice := seedAlias(once); twice != once {
		t.Fatalf("seedAlias not idempotent: %q then %q", once, twice)
	}
	if got := seedAlias("personal"); got != "personal" {
		t.Fatalf("seedAlias(%q) = %q, want it untouched", "personal", got)
	}
}

// TestSeedAliasAdoptsAName covers the ordering the whole design rests on:
// adoption runs ahead of the seed, so there is always a name to seed with.
func TestSeedAliasAdoptsAName(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "")
	got := seedAlias("dotfiles/main")
	if got == "dotfiles/main" {
		t.Fatal("seedAlias did not adopt a device name before seeding")
	}
	if want := seedAlias("dotfiles/main"); got != want {
		t.Fatalf("seedAlias not stable across calls: %q then %q", got, want)
	}
}

func TestSeedAliasLeavesEmptyAlone(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	if got := seedAlias(""); got != "" {
		t.Fatalf("seedAlias(\"\") = %q, want \"\"", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/humancli/ -run TestSeedAlias -v`
Expected: FAIL — `TestSeedAliasAdoptsAName` fails ("did not adopt a device name"), and the old gate leaves typed names untouched.

- [ ] **Step 3: Rewrite `seedAlias`**

Replace the whole body of `internal/humancli/aliasseed.go`:

```go
package humancli

import "github.com/schuettc/muster/internal/device"

// seedAlias prefixes an alias with this machine's device name, adopting a name
// first if the operator has not chosen one.
//
// Every alias this machine mints is seeded — derived, typed, or allocated.
// The rule used to exempt typed names on the reasoning that an explicit choice
// should not be silently rewritten. Hiding the prefix locally inverts that:
// once the roster shows "dotfiles/main" and "galley/design" side by side,
// nothing distinguishes the alias that is device-scoped from the one that will
// collide, so an operator would reasonably type `become galley/design` on a
// second machine believing it behaves like every other name on screen.
//
// Adoption runs ahead of the seed (device.Adopt), which is why this needs no
// "is a name configured" gate: there is always a name by the time an alias is
// minted. An adoption failure degrades to the unseeded alias rather than
// blocking registration — a machine that cannot name itself still needs to be
// able to register.
func seedAlias(alias string) string {
	if alias == "" {
		return alias
	}
	name, _, err := device.Adopt()
	if err != nil {
		return alias
	}
	return device.Seed(name, alias)
}
```

- [ ] **Step 4: Seed the remaining mint sites**

In `internal/humancli/identity.go`, replace the alias switch at lines 64-73 so every branch seeds:

```go
	alias := ""
	switch {
	case len(rest) > 0:
		// Typed names seed exactly like derived ones. With the prefix hidden
		// locally, an operator cannot see which of their names is device-scoped,
		// so an exception here would be invisible at the moment it mattered.
		alias = seedAlias(rest[0])
	case os.Getenv("MUSTER_ALIAS") != "":
		alias = seedAlias(os.Getenv("MUSTER_ALIAS"))
	case c.SessionName != "":
		// Derived: two machines can easily share a tmux session name.
		alias = seedAlias(c.SessionName)
	default:
```

In `internal/humancli/become.go`, seed the claim target while keeping the short form for display and for the name typed into the pane. Replace line 75 and its surroundings:

```go
	// The CLAIM is seeded; the injected harness name and the confirmation are
	// not. syncAgentName sets the tmux/harness session name, which is a human
	// surface and the identity `proj` reads — it must stay short, or the
	// prefix reappears in exactly the title bar this design clears.
	claim := seedAlias(to)
	raw, err := callData("become", map[string]any{"from": fromAlias, "to": claim})
```

The existing `syncAgentName(out, to, ...)` call at line 93 and the `fmt.Fprintf` at line 95 keep using `to`, unchanged.

- [ ] **Step 5: Add the become round-trip test**

Append to `internal/humancli/become_test.go`:

```go
// TestBecomeSeedsTheClaimNotTheInjectedName pins the split inside become: the
// stored alias carries the device prefix, while the name typed into the pane
// and reported to the operator stays short. Injecting the seeded name would
// put the prefix straight back into the tmux session name and the title bar.
func TestBecomeSeedsTheClaimNotTheInjectedName(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	if got, want := seedAlias("galley/design"), "personal-galley/design"; got != want {
		t.Fatalf("claim = %q, want %q", got, want)
	}
	// A full alias pasted back from the other machine claims the same row.
	if got, want := seedAlias("personal-galley/design"), "personal-galley/design"; got != want {
		t.Fatalf("re-seeded claim = %q, want %q", got, want)
	}
}
```

- [ ] **Step 6: Run the package tests**

Run: `go test ./internal/humancli/ -v`
Expected: PASS. Pre-existing register/become tests that assert an unseeded alias will fail here — update them to expect the seeded form, since seeding every mint is the intended change.

- [ ] **Step 7: Commit**

```bash
git add internal/humancli/aliasseed.go internal/humancli/aliasseed_test.go internal/humancli/identity.go internal/humancli/become.go internal/humancli/become_test.go
git commit -m "feat(alias): seed every minted alias, adopting a device name first"
```

---

### Task 4: Strip on the CLI's human surfaces, with collision-aware rendering

Stripping can make two visible rows render the same string — a legacy unprefixed row beside a new seeded one, or a foreign machine's bare alias matching one of ours. When that happens both render in full. This is not migration-only code: the cross-device case is permanent.

**Files:**
- Create: `internal/humancli/dispalias.go`
- Test: `internal/humancli/dispalias_test.go`
- Modify: `internal/humancli/humancli.go:236-240` (agents ALIAS column), `:514-542` (inbox/tasks FROM/TO), `:620` (nudge)
- Modify: `internal/humancli/thread.go:70,89`
- Modify: `internal/humancli/identity.go:152,160,206,322,351`, `internal/humancli/become.go:96`, `internal/humancli/register_ack.go:29`

**Interfaces:**
- Consumes: `device.Strip` (Task 1).
- Produces: `aliasDisplay(aliases []string) map[string]string` — a full→display map for one view, collision-aware; and `dispAlias(alias string) string` for single-alias messages where no other row is in view.

- [ ] **Step 1: Write the failing tests**

Create `internal/humancli/dispalias_test.go`:

```go
package humancli

import "testing"

// TestAliasDisplayStripsTheLocalPrefix is the everyday case: this machine's
// own rows read short.
func TestAliasDisplayStripsTheLocalPrefix(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	d := aliasDisplay([]string{"personal-dotfiles/main", "work-dotfiles/main"})
	if got, want := d["personal-dotfiles/main"], "dotfiles/main"; got != want {
		t.Fatalf("local alias rendered %q, want %q", got, want)
	}
	if got, want := d["work-dotfiles/main"], "work-dotfiles/main"; got != want {
		t.Fatalf("foreign alias rendered %q, want %q", got, want)
	}
}

// TestAliasDisplayKeepsCollidingRowsInFull covers both the migration case (a
// legacy bare row beside a seeded one) and the permanent one (a foreign bare
// alias matching one of ours). Two rows must never render the same string.
func TestAliasDisplayKeepsCollidingRowsInFull(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	d := aliasDisplay([]string{"personal-dotfiles/main", "dotfiles/main"})
	if got, want := d["personal-dotfiles/main"], "personal-dotfiles/main"; got != want {
		t.Fatalf("colliding local alias rendered %q, want %q", got, want)
	}
	if got, want := d["dotfiles/main"], "dotfiles/main"; got != want {
		t.Fatalf("colliding bare alias rendered %q, want %q", got, want)
	}
}

// TestDispAliasStripsWithoutAView covers the single-alias confirmations
// ("registered X"), where no other row is on screen to collide with.
func TestDispAliasStripsWithoutAView(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	if got, want := dispAlias("personal-dotfiles/main"), "dotfiles/main"; got != want {
		t.Fatalf("dispAlias = %q, want %q", got, want)
	}
	if got, want := dispAlias("work-dotfiles/main"), "work-dotfiles/main"; got != want {
		t.Fatalf("dispAlias of a foreign alias = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/humancli/ -run 'TestAliasDisplay|TestDispAlias' -v`
Expected: FAIL — `undefined: aliasDisplay`, `undefined: dispAlias`.

- [ ] **Step 3: Write the implementation**

Create `internal/humancli/dispalias.go`:

```go
package humancli

import "github.com/schuettc/muster/internal/device"

// dispAlias renders one alias for a human: this machine's prefix comes off,
// anything else is left alone. For a single-alias message ("registered X")
// there is no other row on screen to be confused with, so no collision check
// is needed.
//
// Human surfaces only. Model-facing text — the MCP views, the register_agent
// and validate detail strings, the hook output injected into agent context —
// keeps the full alias: a model writes aliases into message bodies and task
// descriptions that are read on the OTHER machine, where a bare name
// re-resolves against that device and lands on a different, real agent.
func dispAlias(alias string) string {
	return device.Strip(device.Name(), alias)
}

// aliasDisplay builds the full→display map for one view. Stripping can make
// two rows render the same string — a legacy unprefixed row beside a seeded
// one, or another machine's bare alias matching one of ours — and a roster
// showing one name twice is worse than one showing a prefix. Both sides of a
// collision render in full.
//
// This mirrors station's existing treatment of ambiguous labels, which falls
// back from "label" to "label (alias)" when computeLabelCollisions fires.
func aliasDisplay(aliases []string) map[string]string {
	name := device.Name()
	short := make(map[string]string, len(aliases))
	count := make(map[string]int, len(aliases))
	for _, a := range aliases {
		s := device.Strip(name, a)
		short[a] = s
		count[s]++
	}
	for a, s := range short {
		if count[s] > 1 {
			short[a] = a
		}
	}
	return short
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/humancli/ -run 'TestAliasDisplay|TestDispAlias' -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Apply to the roster table**

In `internal/humancli/humancli.go`, build the map once before the render loop (just after `showDevice := len(devices) > 1`, around line 193):

```go
	all := make([]string, 0, len(agents))
	for _, a := range agents {
		all = append(all, a.Alias)
	}
	disp := aliasDisplay(all)
```

Then in both `fmt.Fprintf` calls in the loop (lines 236-240), replace `a.Alias` with `disp[a.Alias]`.

- [ ] **Step 6: Apply to the remaining human surfaces**

Wrap each of these aliases in `dispAlias(...)` — single-alias messages with no view to collide within:

- `internal/humancli/identity.go:152,160` — the `registered %s` lines
- `internal/humancli/identity.go:206` — `deregistered %s`
- `internal/humancli/identity.go:322,351` — the gc `purged`/`tombstoned` lines
- `internal/humancli/become.go:96` — `you are now '%s' (was '%s')`, both operands
- `internal/humancli/humancli.go:620` — the nudge line
- `internal/humancli/register_ack.go:29` — the ack's `muster inbox '<alias>'` hint

For the multi-row views, build a map with `aliasDisplay` over the aliases in view and index it:

- `internal/humancli/humancli.go:514-542` — `printThreads`' FROM / TO / LAST-FROM columns
- `internal/humancli/thread.go:70,89` — the thread header and per-entry authors

- [ ] **Step 7: Run the full package suite**

Run: `go test ./internal/humancli/ -v`
Expected: PASS. Golden-output tests asserting full aliases in CLI text need updating to the stripped form; that is the intended change.

- [ ] **Step 8: Commit**

```bash
git add internal/humancli/
git commit -m "feat(cli): render this machine's aliases short, in full on collision"
```

---

### Task 5: Strip in the station TUI

**Files:**
- Modify: `internal/station/model.go:1503` (`dispLabel`), `:1519` (`dispToTarget`)
- Test: `internal/station/model_test.go` (append)

**Interfaces:**
- Consumes: `device.Strip` (Task 1).
- Produces: nothing new — the change is inside station's existing display helpers, so all ~15 call sites inherit it.

- [ ] **Step 1: Write the failing test**

Append to `internal/station/model_test.go`:

```go
// TestDispLabelStripsTheLocalDevicePrefix: station is a human surface, so this
// machine's rows read short there exactly as they do in the CLI. Station never
// decodes device_id, and does not need to — only this machine mints this
// prefix, so the prefix alone identifies a local row.
func TestDispLabelStripsTheLocalDevicePrefix(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	m := Model{Options: Options{Aliases: true}}
	if got, want := m.dispLabel("personal-dotfiles/main"), "dotfiles/main"; got != want {
		t.Fatalf("dispLabel = %q, want %q", got, want)
	}
	if got, want := m.dispLabel("work-dotfiles/main"), "work-dotfiles/main"; got != want {
		t.Fatalf("dispLabel of a foreign alias = %q, want %q", got, want)
	}
}
```

Check `Options`' actual field name for alias-mode in `internal/station/model.go:155` and match it; the point of the test is the strip, not the mode.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/station/ -run TestDispLabelStrips -v`
Expected: FAIL — `dispLabel` returns the full alias.

- [ ] **Step 3: Implement**

In `internal/station/model.go`, apply `device.Strip(device.Name(), alias)` to every path in `dispLabel` and `dispToTarget` that returns or embeds a raw alias — including the `label (alias)` collision fallback, where the parenthesised alias is display text. Add the `internal/device` import.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/station/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/station/
git commit -m "feat(station): render this machine's aliases short"
```

---

### Task 6: Expand short aliases on input (CLI side)

Expansion happens on the **input**, before it reaches `internal/resolve`. That keeps the resolver's device-blind contract — `resolve.Candidate` has no device field and gains none — and confines the change to callers.

**Files:**
- Modify: `internal/humancli/dispalias.go` (add `expandAlias`)
- Modify: `internal/humancli/resolve.go:59` (`resolveVia`)
- Modify: `internal/humancli/identity.go:177` (`deregister`), `internal/humancli/become.go:105` (`--from`), `internal/humancli/thread.go` (`reply --from`), `internal/humancli/events.go` + `watch.go` (`--agent`)
- Test: `internal/humancli/dispalias_test.go` (append), `internal/humancli/resolve_test.go` (append)

**Interfaces:**
- Consumes: `device.Seed` (Task 1) — expansion is the same string operation as seeding.
- Produces: `expandAlias(given string, exists func(string) bool) string`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/humancli/dispalias_test.go`:

```go
// TestExpandAliasPrefersTheLocalRow is the invariant the design exists for: on
// this machine, a short name is mine. Exact-first would have made that
// conditional on what some other machine happens to have registered — and when
// one does, you would silently get theirs.
func TestExpandAliasPrefersTheLocalRow(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	roster := map[string]bool{"personal-dotfiles/main": true, "dotfiles/main": true}
	exists := func(a string) bool { return roster[a] }
	if got, want := expandAlias("dotfiles/main", exists), "personal-dotfiles/main"; got != want {
		t.Fatalf("expandAlias = %q, want %q", got, want)
	}
}

// TestExpandAliasFallsBackToTheLiteral: a full foreign alias, and a bare alias
// with no local counterpart, both still resolve exactly.
func TestExpandAliasFallsBackToTheLiteral(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	roster := map[string]bool{"work-dotfiles/main": true, "legacy": true}
	exists := func(a string) bool { return roster[a] }
	if got, want := expandAlias("work-dotfiles/main", exists), "work-dotfiles/main"; got != want {
		t.Fatalf("expandAlias = %q, want %q", got, want)
	}
	if got, want := expandAlias("legacy", exists), "legacy"; got != want {
		t.Fatalf("expandAlias = %q, want %q", got, want)
	}
}

// TestExpandAliasLeavesUnknownNamesAlone so the caller's own error message
// names what the operator actually typed.
func TestExpandAliasLeavesUnknownNamesAlone(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	exists := func(string) bool { return false }
	if got, want := expandAlias("typo", exists), "typo"; got != want {
		t.Fatalf("expandAlias = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/humancli/ -run TestExpandAlias -v`
Expected: FAIL — `undefined: expandAlias`.

- [ ] **Step 3: Implement**

Append to `internal/humancli/dispalias.go`:

```go
// expandAlias turns a name typed on this machine into the alias actually
// stored, trying <device>-<given> before the literal given.
//
// Local-first is the invariant, not an optimisation: on this machine a short
// name is mine. Exact-first would be strictly additive and never change what
// resolves today, but it would make that promise conditional on what another
// machine happens to have registered — and in the case where one holds the
// bare string, the operator silently reaches a stranger's agent instead of
// their own. That is the action-at-a-distance this design removes.
//
// An unknown name is returned unchanged so the caller's error names what was
// actually typed, not a prefixed string the operator never wrote.
func expandAlias(given string, exists func(string) bool) string {
	if given == "" || exists == nil {
		return given
	}
	if seeded := device.Seed(device.Name(), given); seeded != given && exists(seeded) {
		return seeded
	}
	return given
}
```

- [ ] **Step 4: Wire it into `resolveVia`**

In `internal/humancli/resolve.go`, expand before resolving — the roster is already in hand:

```go
func resolveVia(given string) (string, error) {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return "", err
	}
	var rows []agentRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return "", err
	}
	// Expand a short local name to the stored alias BEFORE resolution, so
	// internal/resolve stays device-blind (its Candidate carries no device
	// field, and its precedence rules are unchanged).
	byAlias := make(map[string]bool, len(rows))
	for _, r := range rows {
		byAlias[r.Alias] = true
	}
	given = expandAlias(given, func(a string) bool { return byAlias[a] })
	return ResolveTarget(enrichAgents(rows), given, callerProject())
}
```

- [ ] **Step 5: Wire it into the exact-match bypass sites**

Each of these takes an alias raw and exact-matches it. Fetch the roster (via the same `list_agents` call the surrounding code already makes, or add one) and pass the input through `expandAlias` before use:

- `internal/humancli/identity.go:177` — `deregister [alias]`
- `internal/humancli/become.go:105` — `--from`, checked against `becomeLiveAliases`; use that list as the `exists` predicate
- `internal/humancli/thread.go` — `reply --from`
- `internal/humancli/events.go` and `internal/humancli/watch.go` — `--agent`

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/humancli/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/humancli/
git commit -m "feat(cli): expand short local aliases before resolution"
```

---

### Task 7: Daemon-side expansion

MCP hands `to_target` straight to the daemon with no client-side resolution, so the daemon is the only place a model-supplied target is checked. Daemon-side expansion is required, not optional. The daemon has no device identity in local mode — `d.deviceID`/`d.deviceName` are the remote-mode half — so the name has to be plumbed in.

**Files:**
- Modify: `internal/daemon/daemon.go:85-101` (`New`, `Serve` — accept the device name), `internal/daemon/resolve.go:26`
- Modify: `cmd/muster/main.go:154` (local `Serve` call)
- Test: `internal/daemon/resolve_test.go` (append)

**Interfaces:**
- Consumes: `device.Seed` (Task 1), `device.Name` (existing).
- Produces: `Daemon.deviceName` populated in local mode too; `resolveAgentTarget` expanding its `given`.

- [ ] **Step 1: Write the failing test**

Append to `internal/daemon/resolve_test.go`:

```go
// TestResolveAgentTargetExpandsAShortLocalAlias covers the path a model takes:
// MCP passes to_target straight through with no client-side resolution, so if
// the daemon does not expand, a model that read a short alias off the roster
// cannot address it at all.
func TestResolveAgentTargetExpandsAShortLocalAlias(t *testing.T) {
	d := newTestDaemon(t) // existing helper in this package
	d.deviceName = "personal"
	registerTestAgent(t, d, "personal-dotfiles/main")

	got, err := d.resolveAgentTarget("", "dotfiles/main")
	if err != nil {
		t.Fatalf("resolveAgentTarget: %v", err)
	}
	if want := "personal-dotfiles/main"; got != want {
		t.Fatalf("resolveAgentTarget = %q, want %q", got, want)
	}
}

// TestResolveAgentTargetKeepsForeignAliasesExact: the full form always
// resolves to itself, which is what makes cross-machine references written
// into message bodies correct wherever they are read.
func TestResolveAgentTargetKeepsForeignAliasesExact(t *testing.T) {
	d := newTestDaemon(t)
	d.deviceName = "personal"
	registerTestAgent(t, d, "work-dotfiles/main")

	got, err := d.resolveAgentTarget("", "work-dotfiles/main")
	if err != nil {
		t.Fatalf("resolveAgentTarget: %v", err)
	}
	if want := "work-dotfiles/main"; got != want {
		t.Fatalf("resolveAgentTarget = %q, want %q", got, want)
	}
}
```

Match `newTestDaemon` / `registerTestAgent` to whatever helpers this package's existing tests use; if none exist, build the daemon with `New(store, nil)` over a temp store and register through `d.Dispatch`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/daemon/ -run TestResolveAgentTarget -v`
Expected: FAIL — the short alias does not resolve.

- [ ] **Step 3: Plumb the device name into local mode**

In `internal/daemon/daemon.go`, give `New` and `Serve` the name. `New` is also used by Lambda mode, where there is no local device — pass `""` there.

```go
// New builds a Daemon over s with no listener bound. Lambda mode uses this to
// get a Dispatch target without a unix socket; n may be nil, in which case no
// notifications are delivered. deviceName is this machine's name, used to
// expand a short local alias on input; "" disables expansion (Lambda mode,
// which serves no single device).
func New(s store.API, n wake.Notifier, deviceName string) *Daemon {
	return &Daemon{s: s, n: n, deviceName: deviceName, recStop: make(chan struct{})}
}

// Serve binds socketPath (replacing any stale socket) and serves in a
// goroutine. n may be nil, in which case no notifications are delivered.
// deviceName is snapshotted here rather than re-read per request, matching
// ServeRemote: a name change mid-run is not tracked, and auto-adoption makes
// changes rare and deliberate.
func Serve(socketPath string, s store.API, n wake.Notifier, deviceName string) (*Daemon, error) {
	_ = os.Remove(socketPath) // clear a stale socket from a previous run
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	d := New(s, n, deviceName)
	d.ln = ln
	go d.acceptLoop()
	return d, nil
}
```

Update `ServeRemote` (`internal/daemon/remotemode.go:46`) to pass its `deviceName` through `New` rather than assigning the field directly, and update `cmd/muster/main.go:154`:

```go
		d, err = daemon.Serve(paths.SocketPath(), s, notifier, device.Name())
```

Add the `internal/device` import to `cmd/muster/main.go` if it is not already there, and fix every other `daemon.New(`/`daemon.Serve(` call site the compiler flags (including tests and Lambda mode, which passes `""`).

- [ ] **Step 4: Expand in `resolveAgentTarget`**

In `internal/daemon/resolve.go`, expand `given` against the roster already in hand, before building candidates:

```go
	// Expand a short local name to the stored alias. This is the ONLY place a
	// model-supplied target is checked: MCP passes to_target straight through
	// with no client-side resolution, so without this a model that read a
	// short alias off the roster could not address it at all.
	if d.deviceName != "" {
		if seeded := device.Seed(d.deviceName, given); seeded != given {
			for _, ag := range agents {
				if ag.Alias == seeded {
					given = seeded
					break
				}
			}
		}
	}
```

Add the `internal/device` import.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/daemon/ ./cmd/... -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/ cmd/muster/
git commit -m "feat(daemon): expand short local aliases, including for MCP callers"
```

---

### Task 8: The `@muster_agent` badge carries the stripped form

This is the surface that started the whole investigation. The operator's `.tmux.conf` renders the alias into the terminal title only when it differs from the tmux session name; seeding broke that dedupe and put a machine name in every title on the machine it names. Stripping restores it with no dotfiles change.

**Files:**
- Modify: `internal/daemon/daemon.go:490-499` (`liveAliasesFor`)
- Test: `internal/daemon/daemon_test.go` (append)

**Interfaces:**
- Consumes: `device.Strip` (Task 1), `Daemon.deviceName` (Task 7).
- Produces: nothing new — `liveAliasesFor` is where the badge rule is already written, so both the local and remote paths inherit the change.

- [ ] **Step 1: Write the failing test**

Append to `internal/daemon/daemon_test.go`:

```go
// TestLiveAliasesForStripsTheLocalPrefix pins the fix for the symptom that
// started this: the dotfiles title renders @muster_agent only when it differs
// from the tmux session name, so a seeded alias broke the dedupe and put the
// device name in every window title.
func TestLiveAliasesForStripsTheLocalPrefix(t *testing.T) {
	agents := []store.Agent{
		{Alias: "personal-dotfiles/main", SocketPath: "/s", SessionID: "$1"},
		{Alias: "work-dotfiles/main", SocketPath: "/s", SessionID: "$1"},
	}
	got := liveAliasesFor(agents, "/s", "$1", "personal")
	want := []string{"dotfiles/main", "work-dotfiles/main"}
	if len(got) != len(want) {
		t.Fatalf("liveAliasesFor = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("liveAliasesFor = %v, want %v", got, want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/daemon/ -run TestLiveAliasesFor -v`
Expected: FAIL — too many arguments to `liveAliasesFor`.

- [ ] **Step 3: Implement**

In `internal/daemon/daemon.go`, take the device name and strip before sorting — sorting after the strip so the badge is ordered by what is actually displayed:

```go
func liveAliasesFor(agents []store.Agent, socketPath, sessionID, deviceName string) []string {
	aliases := []string{}
	for _, ag := range agents {
		if ag.SocketPath == socketPath && ag.SessionID == sessionID && !ag.Departed {
			// The badge is a human surface — it renders into the terminal
			// title. Strip here rather than in wake.SetAgents, which joins
			// verbatim by contract, so the local and remote badge paths (both
			// of which come through this one filter) cannot disagree.
			aliases = append(aliases, device.Strip(deviceName, ag.Alias))
		}
	}
	sort.Strings(aliases)
	return compactStrings(aliases)
}
```

Update both callers — `sessionAliasesFor` (line 478, pass `d.deviceName`) and the remote-mode reconcile path in `internal/daemon/remotemode.go` — and add the `internal/device` import.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/daemon/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/
git commit -m "feat(daemon): strip the local device prefix from the agent badge"
```

---

### Task 9: Pin the human/model split on one fixture

Every other task strips something. Nothing so far asserts what must **not** be stripped, and that is the decision most likely to erode: a later contributor tidying up "inconsistent" alias rendering would make model surfaces short and reintroduce silent cross-machine misdelivery. One test, one fixture, both directions.

**Files:**
- Create: `internal/mcpserver/aliasfull_test.go`

**Interfaces:**
- Consumes: `device.Strip` (Task 1), the CLI/station display helpers (Tasks 4–5), the MCP views (`internal/mcpserver/call.go:95,116,134`).
- Produces: nothing — a guard test only.

- [ ] **Step 1: Write the test**

Create `internal/mcpserver/aliasfull_test.go`:

```go
package mcpserver

import (
	"strings"
	"testing"
)

// TestModelSurfacesKeepTheFullAlias is the counterweight to every stripping
// test in this change. Human surfaces render this machine's aliases short;
// model surfaces must not.
//
// The reason is an asymmetry in failure modes. A human who types a short name
// that does not resolve gets an error and retries. A model writes aliases into
// message bodies and task descriptions that are read on the OTHER machine,
// where a bare name re-resolves against that device and lands on a different,
// real agent — a silent misroute that nothing reports.
//
// If you are here because this test failed while you were making alias
// rendering consistent: that inconsistency is the feature.
func TestModelSurfacesKeepTheFullAlias(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")

	view := agentViewFor(rosterRow{Alias: "personal-dotfiles/main"})
	if view.Alias != "personal-dotfiles/main" {
		t.Fatalf("AgentView.Alias = %q, want the full stored alias", view.Alias)
	}
	if strings.HasPrefix(view.Alias, "dotfiles/") {
		t.Fatal("AgentView.Alias was stripped; model surfaces must carry the full alias")
	}
}
```

Match `agentViewFor` / `rosterRow` to the actual constructor in `internal/mcpserver/call.go` — the assertion is what matters, not the helper's name. If the view is built inline rather than by a named function, construct it the way `list_agents` does.

- [ ] **Step 2: Run the test**

Run: `go test ./internal/mcpserver/ -run TestModelSurfacesKeepTheFullAlias -v`
Expected: PASS immediately — no task ever stripped these. This test exists to fail later, if someone makes them consistent.

- [ ] **Step 3: Add the same guard for the hook text**

Append to `internal/humancli/hook_test.go`:

```go
// TestHookOutputKeepsTheFullAlias: hook text is injected into an agent's
// context, so it is a model surface. See
// mcpserver.TestModelSurfacesKeepTheFullAlias for why the split exists.
func TestHookOutputKeepsTheFullAlias(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	// The reconnect line must carry the alias the model should call
	// get_inbox with — the stored one.
	line := reconnectLine("personal-dotfiles/main", 3)
	if !strings.Contains(line, "personal-dotfiles/main") {
		t.Fatalf("hook line %q dropped the full alias", line)
	}
}
```

Match `reconnectLine` to the actual helper behind `internal/humancli/hook.go:285`; if the message is built inline, extract it into a small named function first so it can be tested.

- [ ] **Step 4: Run both packages**

Run: `go test ./internal/mcpserver/ ./internal/humancli/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/aliasfull_test.go internal/humancli/hook_test.go internal/humancli/hook.go
git commit -m "test: pin that model surfaces keep the full alias"
```

---

### Task 10: Documentation

**Files:**
- Modify: `internal/humancli/humancli.go:75-81` (the device-is-never-identity comment)
- Modify: `internal/humancli/help.go` (the `device` command's help text)
- Modify: `README.md` (device naming section, if present)

- [ ] **Step 1: Revise the invariant comment**

`internal/humancli/humancli.go:75-81` currently asserts that an alias means the same agent from every device on the bus. Replace with:

```go
	// DeviceID is location, never identity: nothing addressable is scoped by
	// it, and internal/resolve takes no device argument. What device DOES
	// affect is presentation. The STORED alias is globally unique and carries
	// this machine's name; the DISPLAYED alias drops that prefix on the
	// machine that minted it, and a short name typed there expands back before
	// resolution. So an alias still means the same agent from every device —
	// it is just written two ways, short at home and in full abroad.
```

- [ ] **Step 2: Update the `device` help text**

`muster help device` currently says setting a name is "the expected gesture" and that existing rows keep their alias. Add that a name is now adopted automatically on first registration, that it prefixes every alias this machine mints, and that the prefix is hidden on this machine and visible from others.

- [ ] **Step 3: Document the human/model split**

Add a short section (README or `docs/`) recording that an agent reports itself by its full alias while the operator's terminal shows the short one, and why: a model writes aliases into message bodies read on other machines, where a bare name would re-resolve locally and reach a different, real agent. This is the one part of the design that reads as a bug on first encounter.

- [ ] **Step 4: Commit**

```bash
git add internal/humancli/ README.md
git commit -m "docs: device-relative aliases — stored in full, displayed short"
```

---

### Task 11: Retire the stale "(primary clone)" cue in dotfiles

Unrelated to the alias work — a leftover from the worktree era. `proj` retired its worktree machinery in `df5c5b2`/`d7780ce` and now creates every session in the same directory, so "primary clone" no longer distinguishes anything. Commit `b3b2ae4` already retired the matching `⚠ primary` status-bar warning; the picker row and docs were missed.

**Files:**
- Modify: `~/dotfiles/config/zsh/04-aliases.zsh:334`
- Modify: `~/dotfiles/docs/terminal-setup.md:165`, `~/dotfiles/docs/terminal-usage.md:83`
- Test: `~/dotfiles/tests/proj-picker.test.zsh`

**Note:** separate repo, separate branch, separate commit. Do not mix with the muster work.

- [ ] **Step 1: Check what the picker test asserts**

Run: `grep -n 'home base' ~/dotfiles/tests/proj-picker.test.zsh`
The plan-time expectation is that it matches on `🏠 <project>`; if it matches the full string including "(primary clone)", update it in step 3.

- [ ] **Step 2: Change the picker row**

In `config/zsh/04-aliases.zsh:334`:

```zsh
  print -r -- "🏠 ${project} — home base"
```

- [ ] **Step 3: Update the docs and test**

In `docs/terminal-setup.md:165` and `docs/terminal-usage.md:83`, drop "(primary clone)" from the rendered row. Update the picker test if step 1 showed it matching the old string.

- [ ] **Step 4: Run the tests**

Run: `cd ~/dotfiles && zsh tests/proj-picker.test.zsh`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd ~/dotfiles
git add config/zsh/04-aliases.zsh docs/terminal-setup.md docs/terminal-usage.md tests/proj-picker.test.zsh
git commit -m "docs(proj): drop the stale primary-clone cue from the home base row"
```

---

### Task 12: One mint helper, shared by every client — closes the MCP hole

Found during Task 3's review: `internal/mcpserver` mints aliases the same way `internal/humancli` does and seeds neither — `registerAgentHandler` passes the model-supplied `in.Alias` straight through at `tools_registry.go:41` (the `become` claim) and `:81` (`register_agent`). An agent registering itself via MCP from two machines still collides, which is the exact failure this feature exists to prevent, on the path models use.

The fix is not to seed in a second place. `humancli.seedAlias` is already "adopt, then seed"; that composite moves into `internal/device` so there is ONE function every client calls, rather than each package carrying its own copy of the rule. Rejected: seeding defensively in the daemon as a backstop — clients must seed anyway (see below), so a daemon seed is redundant work that reads as duplication and invites deletion.

**Why clients must seed rather than the daemon:** `allocPanelessAlias` (`internal/humancli/paneless.go:39-60`) decides each candidate by looking it up first (`hookGetAgent(cand)`) and inspecting the row's tuple to tell "my own row (resume)" from "a tombstone (revive)" from "a live owner (next suffix)". If the daemon rewrote aliases on write while clients read by the pre-rewrite key, a restarting session would miss its own row, suffix past itself, and mint `foo-2`, `foo-3`, … on every launch — orphaning its inbox each time.

**Files:**
- Modify: `internal/device/alias.go` (add the composite), `internal/device/alias_test.go`
- Modify: `internal/humancli/aliasseed.go` (delegate), `internal/mcpserver/tools_registry.go:41,81`
- Test: `internal/mcpserver/tools_registry_test.go`

**Interfaces:**
- Consumes: `device.Seed` (Task 1), `device.Adopt` (Task 2).
- Produces: `device.SeedMinted(alias string) string` — adopts a device name if none is configured, then seeds. The one function every mint site calls.

- [ ] **Step 1: Write the failing tests**

Append to `internal/device/alias_test.go`:

```go
// TestSeedMintedAdoptsThenSeeds pins the composite every client mints through.
// The ordering is the point: adoption runs first, so there is always a name to
// seed with and no caller needs its own "is a name configured" gate.
func TestSeedMintedAdoptsThenSeeds(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	if got, want := SeedMinted("researcher"), "personal-researcher"; got != want {
		t.Fatalf("SeedMinted = %q, want %q", got, want)
	}
	if got := SeedMinted("personal-researcher"); got != "personal-researcher" {
		t.Fatalf("SeedMinted not idempotent: %q", got)
	}
	if got := SeedMinted(""); got != "" {
		t.Fatalf("SeedMinted(\"\") = %q, want \"\"", got)
	}
}
```

Append to `internal/mcpserver/tools_registry_test.go` (check the file's package clause first and qualify to match):

```go
// TestMCPRegistrationSeedsTheModelSuppliedAlias closes the hole this task
// exists for: a model registering the same name from two machines would
// otherwise take the other machine's row, and its inbox with it.
func TestMCPRegistrationSeedsTheModelSuppliedAlias(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	if got, want := device.SeedMinted("researcher"), "personal-researcher"; got != want {
		t.Fatalf("mint seeding = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/device/ -run TestSeedMinted -v` and `go test ./internal/mcpserver/ -run TestMCPRegistration -v`
Expected: FAIL — `undefined: SeedMinted`.

- [ ] **Step 3: Add the composite**

Append to `internal/device/alias.go`:

```go
// SeedMinted is the one call every client makes when it MINTS an alias:
// adopt a device name if the operator has not chosen one, then seed.
//
// It lives here rather than in each client package because a rule enforced at
// N call sites is a rule that eventually is not. Four mint sites were missed
// while this feature was built — three in the session hooks, one in the MCP
// server — each of them a path where two machines could silently claim the
// same roster row, and the row IS the identity, so the loser's inbox goes with
// it.
//
// This is for MINTS only. A LOOKUP of an existing alias must not be seeded:
// seeding a lookup makes an existing alias unfindable, and the paneless
// allocator in particular reads a candidate's row before deciding to resume,
// revive, or suffix past it.
//
// An adoption failure degrades to the unseeded alias rather than blocking:
// a machine that cannot name itself must still be able to register.
func SeedMinted(alias string) string {
	if alias == "" {
		return alias
	}
	name, _, err := Adopt()
	if err != nil {
		return alias
	}
	return Seed(name, alias)
}
```

- [ ] **Step 4: Delegate from humancli and apply in mcpserver**

In `internal/humancli/aliasseed.go`, `seedAlias` becomes a one-line delegation to `device.SeedMinted`, keeping its existing doc comment's explanation of WHY every minted alias is seeded. Do not change any call site — they keep calling `seedAlias`.

In `internal/mcpserver/tools_registry.go`, seed both mint sites:
- line ~41: the `become` claim — `"to": device.SeedMinted(in.Alias)`
- line ~81: `register_agent` — `"alias": device.SeedMinted(in.Alias)`

The detail strings returned to the model must report the **seeded** alias, not `in.Alias`. Model-facing surfaces carry the full stored alias by design (a model writes aliases into message bodies read on other machines), so a reply saying `registered as 'researcher'` when the row is `personal-researcher` would hand the model an address that resolves to the wrong agent when quoted elsewhere. Bind the seeded value to a local variable and use it for both the daemon call and the reply text.

Leave `paneRegistration`'s lookup at `tools_registry.go:39` alone — it reads an existing row and is not a mint.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/device/ ./internal/mcpserver/ ./internal/humancli/` then `go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/device/ internal/humancli/aliasseed.go internal/mcpserver/
git commit -m "feat(alias): one shared mint helper, seeding MCP registration too"
```

---

## Final verification

- [ ] `go test ./...` passes in the muster worktree — including `internal/storetest`'s conformance suite, which exercises rows carrying no device name and must keep passing unchanged.
- [ ] `go vet ./...` is clean.
- [ ] Manual: `muster agents` shows this machine's rows short; `muster device` reports a configured name; opening a new `proj` session leaves the terminal title free of a device prefix.
- [ ] Manual: `muster send <short-name> "hi"` from another session reaches the local agent.
