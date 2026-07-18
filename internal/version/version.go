// Package version holds muster's build-time version stamp. It exists so
// exactly one pair of variables gets the -ldflags -X treatment — cmd/muster,
// the justfile, and .github/workflows/release.yml all target
// github.com/schuettc/muster/internal/version.{version,commit} rather than
// three copies scattered across main packages.
package version

// version and commit are overwritten at build time via:
//
//	-ldflags "-X github.com/schuettc/muster/internal/version.version=$(cat VERSION) \
//	          -X github.com/schuettc/muster/internal/version.commit=$(git rev-parse --short HEAD)"
//
// A plain `go build` / `go run` (no ldflags — local dev, `go test`, an
// unstamped checkout) leaves them at these defaults, so `muster version`
// still prints something sane: "muster dev (none)".
var (
	version = "dev"
	commit  = "none"
)

// Version returns the stamped version ("dev" if the binary wasn't built with
// the ldflags above).
func Version() string { return version }

// Commit returns the stamped short commit hash ("none" if unstamped).
func Commit() string { return commit }

// Line formats muster's canonical one-line version banner, e.g.
// "muster 0.6.0 (a1b2c3d)". This is the single formatting function so both
// `muster version` and `muster --version` render identically.
func Line() string {
	return "muster " + version + " (" + commit + ")"
}
