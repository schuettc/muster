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

// TestDispStripsTheLocalPrefix covers events/watch/station's alias display
// (--aliases mode, or any agent with no label yet): this machine's own
// prefix comes off, a foreign one is left alone. Same human-surface rule as
// internal/humancli's dispAlias.
func TestDispStripsTheLocalPrefix(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	r := NewRenderer(nil, nil, true, false, 120)
	if got, want := r.disp("personal-dotfiles/main"), "dotfiles/main"; got != want {
		t.Fatalf("disp = %q, want %q", got, want)
	}
	if got, want := r.disp("work-dotfiles/main"), "work-dotfiles/main"; got != want {
		t.Fatalf("disp of foreign alias = %q, want %q", got, want)
	}
}

// TestDispTargetStripsAgentTargetsOnly covers dispTarget's three shapes: an
// agent:-prefixed target is an alias and must strip; role: and broadcast
// targets are role/project names, never aliases, and must render untouched
// even when they happen to carry this machine's prefix string.
func TestDispTargetStripsAgentTargetsOnly(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	r := NewRenderer(nil, nil, true, false, 120)
	if got, want := r.dispTarget("agent:personal-dotfiles/main"), "dotfiles/main"; got != want {
		t.Fatalf("agent target = %q, want %q", got, want)
	}
	if got, want := r.dispTarget("role:personal-reviewer"), "role:personal-reviewer"; got != want {
		t.Fatalf("role target must not be stripped, got %q, want %q", got, want)
	}
	if got, want := r.dispTarget("broadcast:personal-proj"), "broadcast:personal-proj"; got != want {
		t.Fatalf("scoped broadcast target must not be stripped, got %q, want %q", got, want)
	}
}
