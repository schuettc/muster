package cli

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
//
// This is a delegation to device.SeedMinted, the one mint helper every
// client — humancli and mcpserver alike — calls, so the rule lives in one
// place rather than in each client's own copy.
func seedAlias(alias string) string {
	return device.SeedMinted(alias)
}
