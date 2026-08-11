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
