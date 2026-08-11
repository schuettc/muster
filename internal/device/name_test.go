package device

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeNameHandlesRealHostnames(t *testing.T) {
	// The inputs that matter are hostnames, which is why this is lossy rather
	// than validating: a Mac's default name has an apostrophe, spaces, capitals
	// and a .local suffix, and all of it has to survive into something usable
	// as an alias prefix.
	for in, want := range map[string]string{
		"Courts-MacBook-Pro.local": "courts-macbook-pro",
		"Court's MacBook Pro":      "court-s-macbook-pro",
		"work-laptop":              "work-laptop",
		"  Work Laptop  ":          "work-laptop",
		"UPPER":                    "upper",
		"a..b":                     "a",
		"--x--":                    "x",
		"---":                      "",
		"":                         "",
	} {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeNameIsBounded(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "abcdefghij"
	}
	if got := SanitizeName(long); len(got) > maxNameLen {
		t.Fatalf("SanitizeName returned %d chars, want <= %d", len(got), maxNameLen)
	}
}

// TestNameConfiguredDistinguishesChosenFromDerived pins what NameConfigured
// still distinguishes now that seeding no longer depends on it: nothing
// persisted yet versus a name pinned to disk, by an operator or by Adopt().
// That distinction is what devicename.go's source=hostname|configured display
// reads; it no longer decides whether an alias gets seeded, which happens
// unconditionally.
func TestNameConfiguredDistinguishesChosenFromDerived(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MUSTER_HOME", home)
	t.Setenv(NameEnv, "")

	if got := NameConfigured(); got != "" {
		t.Fatalf("NameConfigured() = %q with nothing set, want \"\"", got)
	}
	// Name still answers, falling back to the hostname.
	if got := Name(); got == "" {
		t.Fatal("Name() = \"\" with no hostname fallback available")
	}

	if _, err := SetName("Work Laptop"); err != nil {
		t.Fatal(err)
	}
	if got := NameConfigured(); got != "work-laptop" {
		t.Fatalf("NameConfigured() after SetName = %q, want work-laptop", got)
	}
	if got := Name(); got != "work-laptop" {
		t.Fatalf("Name() after SetName = %q, want work-laptop", got)
	}

	// The environment overrides the file, matching how MUSTER_DEVICE_ID works.
	t.Setenv(NameEnv, "desktop")
	if got := Name(); got != "desktop" {
		t.Fatalf("Name() with %s set = %q, want desktop", NameEnv, got)
	}
}

func TestSetNameRejectsUnusableInput(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	if _, err := SetName("!!!"); err == nil {
		t.Fatal("SetName(\"!!!\") succeeded; a name that sanitizes to nothing must be an error, not a silent no-op")
	}
}

func TestSetNamePersistsForTheNextProcess(t *testing.T) {
	// The whole point of the file over an export: it is still there after a
	// reboot, with no shell configuration involved.
	home := t.TempDir()
	t.Setenv("MUSTER_HOME", home)
	t.Setenv(NameEnv, "")
	if _, err := SetName("work-laptop"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, NameFileName))
	if err != nil {
		t.Fatalf("device-name file not written: %v", err)
	}
	if string(b) != "work-laptop\n" {
		t.Fatalf("device-name file = %q, want %q", b, "work-laptop\n")
	}
}
