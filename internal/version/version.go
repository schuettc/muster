// Package version holds muster's build-time version stamp. It exists so
// exactly one pair of variables gets the -ldflags -X treatment — cmd/muster,
// the justfile, and .github/workflows/release.yml all target
// github.com/schuettc/muster/internal/version.{version,commit} rather than
// three copies scattered across main packages.
package version

import tools "github.com/schuettc/tools-common"

// version, commit, and date are overwritten at build time via:
//
//	-ldflags "-X github.com/schuettc/muster/internal/version.version=$(cat VERSION) \
//	          -X github.com/schuettc/muster/internal/version.commit=$(git rev-parse --short HEAD) \
//	          -X github.com/schuettc/muster/internal/version.date=$(date -u +%Y-%m-%d)"
//
// A plain `go build` / `go run` (no ldflags — local dev, `go test`, an
// unstamped checkout) leaves them at these defaults, so `muster version`
// still prints something sane: "muster dev (none)".
var (
	version = "dev"
	commit  = "none"
	date    = ""
)

// Version returns the stamped version ("dev" if the binary wasn't built with
// the ldflags above).
func Version() string { return version }

// Commit returns the stamped short commit hash ("none" if unstamped).
func Commit() string { return commit }

// Date returns the stamped build date ("" if unstamped).
func Date() string { return date }

// Line formats muster's canonical one-line version banner, e.g.
// "muster 0.6.0 (a1b2c3d, 2026-01-02)". The FORMAT is delegated to the
// shared tools-common module so every .tools-family binary renders its
// version identically (elides an empty commit/date); only the "muster "
// prefix is muster's own. This is the single formatting function so both
// `muster version` and `muster --version` render identically.
func Line() string {
	return "muster " + tools.Version{Number: version, Commit: commit, Date: date}.String()
}
