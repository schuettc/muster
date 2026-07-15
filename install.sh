#!/bin/sh
# muster installer — downloads the latest release binary for this machine,
# verifies its checksum, and installs it to ~/.local/bin (override with
# MUSTER_INSTALL_DIR). Usage:
#
#   curl -fsSL https://muster.tools/install.sh | sh
#
set -eu

repo="schuettc/muster"
install_dir="${MUSTER_INSTALL_DIR:-$HOME/.local/bin}"
base="https://github.com/$repo/releases/latest/download"

fail() { printf 'muster install: %s\n' "$*" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar  >/dev/null 2>&1 || fail "tar is required"

os="$(uname -s | tr 'A-Z' 'a-z')"
case "$os" in
  darwin|linux) ;;
  *) fail "unsupported OS '$os' — muster runs on macOS and Linux (on Windows, install inside WSL2)" ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64)  arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) fail "unsupported architecture '$arch' (need amd64 or arm64)" ;;
esac

asset="muster_${os}_${arch}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

printf 'downloading %s …\n' "$asset"
curl -fsSL "$base/$asset" -o "$tmp/$asset" || fail "download failed: $base/$asset"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" || fail "download failed: checksums.txt"

# verify the checksum (shasum on macOS, sha256sum on Linux)
expected="$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
[ -n "$expected" ] || fail "no checksum found for $asset"
if command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)"
else
  actual="$(sha256sum "$tmp/$asset" | cut -d' ' -f1)"
fi
[ "$actual" = "$expected" ] || fail "checksum mismatch for $asset (expected $expected, got $actual)"

mkdir -p "$install_dir"
tar -xzf "$tmp/$asset" -C "$tmp"
mv "$tmp/muster" "$install_dir/muster"
chmod +x "$install_dir/muster"

# confirm the binary runs on this machine
"$install_dir/muster" >/dev/null 2>&1 || [ $? -eq 2 ] || fail "installed binary failed to run"

printf 'installed muster to %s\n' "$install_dir/muster"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) printf 'note: %s is not on your PATH — add:  export PATH="%s:$PATH"\n' "$install_dir" "$install_dir" ;;
esac
printf 'next: register it with your agent, e.g.  claude mcp add muster -s user -- muster mcp\n'
