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
