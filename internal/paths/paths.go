// Package paths resolves muster's on-disk locations.
package paths

import (
	"os"
	"path/filepath"
)

// HomeSuffix is Home's path relative to the user's home directory — exported
// so documentation (the man page's FILES section) can describe the default
// location without duplicating the literal path segments Home() joins.
func HomeSuffix() string { return filepath.Join(".local", "share", "muster") }

// Home is the muster data directory (~/.local/share/muster, or $MUSTER_HOME).
func Home() string {
	if h := os.Getenv("MUSTER_HOME"); h != "" {
		return h
	}
	base, err := os.UserHomeDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, HomeSuffix())
}

// DBPath is the SQLite database path.
func DBPath() string { return filepath.Join(Home(), "bus.db") }

// SocketPath is the daemon's unix socket path.
func SocketPath() string { return filepath.Join(Home(), "sock") }
