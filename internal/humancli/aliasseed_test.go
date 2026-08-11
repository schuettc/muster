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
