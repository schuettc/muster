package render

import "testing"

// dispTarget must show broadcast targets as-is — never label-resolve them
// through the bare-alias fallthrough. The label planted under the literal
// target string proves the fallthrough is not taken.
func TestDispTargetScopedBroadcastShownAsIs(t *testing.T) {
	r := NewRenderer(nil, map[string]string{"broadcast:web": "WRONG"}, false, false, 120)
	if got := r.dispTarget("broadcast:web"); got != "broadcast:web" {
		t.Fatalf("scoped broadcast target rendered %q, want broadcast:web", got)
	}
	if got := r.dispTarget("broadcast"); got != "broadcast" {
		t.Fatalf("global broadcast target rendered %q, want broadcast", got)
	}
}
