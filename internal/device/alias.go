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
