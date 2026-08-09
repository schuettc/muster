package humancli

import "testing"

// TestSeedAliasOnlyWhenDeviceNamed pins the gate: the overwhelmingly common
// case is one machine on a local bus with no device name ever set, and that
// case must be byte-for-byte unchanged.
func TestSeedAliasOnlyWhenDeviceNamed(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "")
	if got := seedAlias("muster-2"); got != "muster-2" {
		t.Fatalf("seedAlias with no device name = %q, want it untouched", got)
	}
	t.Setenv("MUSTER_DEVICE_NAME", "work-laptop")
	if got, want := seedAlias("muster-2"), "work-laptop-muster-2"; got != want {
		t.Fatalf("seedAlias = %q, want %q", got, want)
	}
}

// TestSeedAliasIsIdempotent is the one that would bite in production: hooks
// re-register on every session start, so seeding an already-seeded alias
// would mint a second identity each time and orphan the previous one's mail.
func TestSeedAliasIsIdempotent(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "work-laptop")
	once := seedAlias("muster-2")
	if twice := seedAlias(once); twice != once {
		t.Fatalf("seedAlias not idempotent: %q then %q", once, twice)
	}
	// A session actually named after the device is left alone rather than
	// doubled.
	if got := seedAlias("work-laptop"); got != "work-laptop" {
		t.Fatalf("seedAlias(%q) = %q, want it untouched", "work-laptop", got)
	}
}

func TestSeedAliasLeavesEmptyAlone(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "work-laptop")
	if got := seedAlias(""); got != "" {
		t.Fatalf("seedAlias(\"\") = %q, want \"\"", got)
	}
}
