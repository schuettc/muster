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
