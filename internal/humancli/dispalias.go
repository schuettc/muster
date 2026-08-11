package humancli

import (
	"encoding/json"

	"github.com/schuettc/muster/internal/device"
)

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
		// Count DISTINCT aliases, not occurrences: the same alias legitimately
		// appears more than once in one view (a thread's FROM and LAST-FROM
		// often match), and counting every occurrence would make that alias
		// collide with itself, sending it back to full form for no reason.
		if _, seen := short[a]; seen {
			continue
		}
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

// rosterAliasExists fetches the live roster and returns an `exists`
// predicate over it for expandAlias, for the input sites that do not
// already have a roster fetch of their own in hand (resolveVia builds its
// own from the rows it already fetched). A roster fetch failure degrades to
// "nothing exists" rather than blocking the caller — expandAlias then
// returns the operator's literal input unchanged, exactly like an unknown
// name.
func rosterAliasExists() func(string) bool {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return func(string) bool { return false }
	}
	var rows []agentRow
	if json.Unmarshal(raw, &rows) != nil {
		return func(string) bool { return false }
	}
	set := make(map[string]bool, len(rows))
	for _, r := range rows {
		set[r.Alias] = true
	}
	return func(a string) bool { return set[a] }
}
