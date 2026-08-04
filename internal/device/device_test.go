package device_test

import (
	"os"
	"path/filepath"
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
