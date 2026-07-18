package version

import "testing"

// TestLine exercises the formatting function directly by swapping the
// package-level vars — this is what a real ldflags-stamped build looks like
// without needing a build-tag'd or ldflags'd test binary (which would be
// overkill for a one-line format function).
func TestLine(t *testing.T) {
	origVersion, origCommit := version, commit
	t.Cleanup(func() { version, commit = origVersion, origCommit })

	version, commit = "1.2.3", "abc1234"
	if got, want := Line(), "muster 1.2.3 (abc1234)"; got != want {
		t.Fatalf("Line() = %q, want %q", got, want)
	}
	if Version() != "1.2.3" {
		t.Fatalf("Version() = %q, want 1.2.3", Version())
	}
	if Commit() != "abc1234" {
		t.Fatalf("Commit() = %q, want abc1234", Commit())
	}
}

// TestLineDefaults checks the unstamped-build fallback wording.
func TestLineDefaults(t *testing.T) {
	origVersion, origCommit := version, commit
	t.Cleanup(func() { version, commit = origVersion, origCommit })

	version, commit = "dev", "none"
	if got, want := Line(), "muster dev (none)"; got != want {
		t.Fatalf("Line() = %q, want %q", got, want)
	}
}
