// Package device resolves this machine's stable muster device identity.
package device

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/schuettc/muster/internal/paths"
)

// FileName is the device id's filename within paths.Home().
const FileName = "device-id"

// ID returns this device's stable identifier: $MUSTER_DEVICE_ID if set,
// otherwise a UUID generated once and persisted at <MUSTER_HOME>/device-id.
//
// SocketPath cannot serve this purpose — two machines can both have
// /tmp/tmux-501/default — so cross-device wake needs an identifier that is
// unique per machine rather than per tmux server.
func ID() (string, error) {
	if v := strings.TrimSpace(os.Getenv("MUSTER_DEVICE_ID")); v != "" {
		return v, nil
	}
	p := filepath.Join(paths.Home(), FileName)
	if b, err := os.ReadFile(p); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v, nil
		}
	}
	if err := os.MkdirAll(paths.Home(), 0o755); err != nil {
		return "", err
	}
	v := uuid.NewString()
	if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
		return "", err
	}
	return v, nil
}
