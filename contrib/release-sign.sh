#!/bin/sh
# Sign and notarize the macOS binaries of an existing muster release, then
# replace the release's darwin assets (and checksums) with the signed ones.
# CI attaches unsigned binaries; run this once per release from a Mac with a
# Developer ID certificate in the keychain and a stored notarytool profile:
#
#   contrib/release-sign.sh v0.3.1
#
# Knobs (env): MUSTER_SIGN_IDENTITY (default: first "Developer ID Application"
# identity in the keychain), MUSTER_NOTARY_PROFILE (default: muster-notary).
set -eu
tag="${1:?usage: release-sign.sh <tag>}"
repo="${MUSTER_REPO_SLUG:-schuettc/muster}"
profile="${MUSTER_NOTARY_PROFILE:-muster-notary}"
identity="${MUSTER_SIGN_IDENTITY:-$(security find-identity -v -p codesigning \
  | sed -n 's/.*"\(Developer ID Application:[^"]*\)".*/\1/p' | head -1)}"
[ -n "$identity" ] || { echo "no Developer ID Application identity found" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
echo "signing as: $identity"

# rebuild the darwin binaries from the tag so we sign exactly the released code
src="$work/src"
git clone --quiet --depth 1 --branch "$tag" "https://github.com/$repo" "$src"
for arch in arm64 amd64; do
  dir="$work/muster_${tag}_darwin_${arch}"
  mkdir -p "$dir"
  (cd "$src" && CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w" -o "$dir/muster" ./cmd/muster)
  codesign --sign "$identity" --options runtime --timestamp --force "$dir/muster"
  ditto -c -k --keepParent "$dir/muster" "$work/notarize_${arch}.zip"
  xcrun notarytool submit "$work/notarize_${arch}.zip" \
    --keychain-profile "$profile" --wait | grep -E "status:" | tail -1
  tar -C "$dir" -czf "$work/muster_${tag}_darwin_${arch}.tar.gz" muster
done

# recompute checksums across the full asset set (signed darwin + CI's linux)
(cd "$work" && \
  gh release download "$tag" --repo "$repo" --pattern "muster_${tag}_linux_*.tar.gz" && \
  shasum -a 256 muster_${tag}_*.tar.gz > checksums.txt && \
  gh release upload "$tag" muster_${tag}_darwin_*.tar.gz checksums.txt --clobber --repo "$repo")
echo "done: darwin assets for $tag are signed + notarized"
