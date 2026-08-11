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
