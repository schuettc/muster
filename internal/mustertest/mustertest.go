// Package mustertest provides shared test helpers for muster.
package mustertest

import "os"

// ShortHome creates a short-path temp directory suitable for use as MUSTER_HOME
// in tests. The daemon's unix socket lives at <home>/sock and must stay under
// the ~104-char sun_path limit; macOS t.TempDir() paths (under $TMPDIR) are too
// long, so tests use this /tmp-based dir instead. Returns the dir and a cleanup
// func the caller should defer/t.Cleanup.
func ShortHome() (dir string, cleanup func(), err error) {
	dir, err = os.MkdirTemp("/tmp", "muster-test-")
	if err != nil {
		return "", func() {}, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}
