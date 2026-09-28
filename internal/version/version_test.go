package version

import "testing"

// The accessors return the ldflags-stamped values. The rendered line
// ("muster <version> (<commit>, <date>)") is tools.Version.String(), printed
// by tools.App's built-in version command; internal/cli's TestVersionUnchanged
// pins it.
func TestAccessors(t *testing.T) {
	origVersion, origCommit, origDate := version, commit, date
	t.Cleanup(func() { version, commit, date = origVersion, origCommit, origDate })

	version, commit, date = "1.2.3", "abc1234", "2026-09-28"
	if Version() != "1.2.3" || Commit() != "abc1234" || Date() != "2026-09-28" {
		t.Fatalf("accessors = %q %q %q", Version(), Commit(), Date())
	}
}
