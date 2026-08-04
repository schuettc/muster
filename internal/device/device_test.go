package device_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/device"
	"github.com/schuettc/muster/internal/mustertest"
)

func TestIDIsStableAcrossCalls(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)

	first, err := device.ID()
	if err != nil {
		t.Fatalf("ID: %v", err)
	}
	if first == "" {
		t.Fatal("ID returned empty string")
	}
	second, err := device.ID()
	if err != nil {
		t.Fatalf("second ID: %v", err)
	}
	if first != second {
		t.Fatalf("ID not stable: %q then %q", first, second)
	}
}

func TestIDHonorsEnvOverride(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	t.Setenv("MUSTER_DEVICE_ID", "laptop-fixed")

	got, err := device.ID()
	if err != nil {
		t.Fatalf("ID: %v", err)
	}
	if got != "laptop-fixed" {
		t.Fatalf("override ignored: got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "device-id")); !os.IsNotExist(err) {
		t.Fatal("override should not write the device-id file")
	}
}

// TestIDFailsRatherThanRotatingOnAReadError pins the ENOENT narrowing.
//
// The device id is the wake-routing key. Every agent registered from this
// machine carries it, and both ReconcileLocalSessions and DevicePoll filter
// the hosted roster down to rows matching it — so ROTATING it does not degrade
// anything gracefully: every agent on the machine instantly looks like another
// device's, every tmux badge goes dark until each one re-registers, and
// SessionUnread's sibling exclusion starts counting the session's own past
// writes as unread. Treating any os.ReadFile error as "not generated yet" made
// a transient EACCES or EIO do exactly that, silently.
//
// The file is made write-only rather than replaced with a directory: a
// directory would ALSO make the subsequent WriteFile fail, so the test would
// still see an error with the guard removed and pin nothing. Write-only is the
// shape that actually separates the two — the read fails, the regenerating
// write would happily succeed — which is exactly the EACCES case in the wild.
func TestIDFailsRatherThanRotatingOnAReadError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny a read, so the fixture cannot fail")
	}
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	t.Setenv("MUSTER_DEVICE_ID", "")

	first, err := device.ID()
	if err != nil {
		t.Fatalf("ID: %v", err)
	}

	p := filepath.Join(dir, device.FileName)
	if err := os.Chmod(p, 0o222); err != nil {
		t.Fatalf("chmod the device id file write-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	// The fixture is only meaningful if a regenerating write WOULD succeed.
	if f, err := os.OpenFile(p, os.O_WRONLY, 0); err != nil {
		t.Fatalf("fixture is not write-open-able, so an error proves nothing: %v", err)
	} else {
		_ = f.Close()
	}

	got, err := device.ID()
	if err == nil {
		t.Fatalf("ID() = %q with no error on an unreadable device-id file; "+
			"want a failure, because the alternative is rotating the wake-routing key "+
			"(and here it rotated: first was %q)", got, first)
	}
	if got != "" {
		t.Fatalf("ID() = %q with an error; want the empty string", got)
	}
	// The file must be left alone: a failed read is not a license to overwrite
	// the id this machine's agents are already registered under.
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.TrimSpace(string(b)) != first {
		t.Fatalf("device id file now holds %q, want the original %q — ID() rotated it",
			strings.TrimSpace(string(b)), first)
	}
}
