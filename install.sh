#!/bin/sh
# muster installer — downloads the latest release for this machine from
# muster.tools, verifies its checksum FAIL-CLOSED, and installs to
# ~/.local/bin (override with MUSTER_INSTALL_DIR). Usage:
#
#   curl -fsSL https://muster.tools/install.sh | sh
#
# Binaries are served from the muster.tools domain, not GitHub: the URL scheme
# is the family contract —
#   https://muster.tools/dl/muster/<version>/muster_<os>_<arch>.tar.gz(.sha256)
#   https://muster.tools/dl/muster/latest   (plain-text version pointer)
# GitHub releases still exist as an invisible durable backing store; this
# script never touches them.
#
# Pin an exact version with MUSTER_VERSION=0.15.1 (skips the latest pointer).
# Versions are bare semver with no leading v (the family download contract);
# a leading v is tolerated and stripped.
# muster-deploy, which stands up the optional hosted backend in your own AWS
# account, is NOT installed by default — it links the AWS SDK the muster binary
# deliberately omits. Ask for it explicitly:
#
#   curl -fsSL https://muster.tools/install.sh | sh -s -- --with-deploy
#
set -eu

host="${MUSTER_DL_HOST:-https://muster.tools}"
install_dir="${MUSTER_INSTALL_DIR:-$HOME/.local/bin}"
with_deploy=0

fail() { printf 'muster install: %s\n' "$*" >&2; exit 1; }

for arg in "$@"; do
  case "$arg" in
    --with-deploy) with_deploy=1 ;;
    -h|--help)
      printf 'usage: install.sh [--with-deploy]\n'
      printf '  --with-deploy  also install muster-deploy (hosted backend installer)\n'
      printf '  MUSTER_VERSION=X.Y.Z pins an exact version.\n'
      printf '  MUSTER_INSTALL_DIR overrides the install directory.\n'
      exit 0 ;;
    *) fail "unknown option '$arg' (see --help)" ;;
  esac
done

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

# Resolve the version: honor an explicit pin, else read the short-TTL latest
# pointer off the domain.
version="${MUSTER_VERSION:-}"
if [ -z "$version" ]; then
  version="$(curl -fsSL "$host/dl/muster/latest")" || fail "could not read latest pointer at $host/dl/muster/latest"
  version="$(printf '%s' "$version" | tr -d ' \t\r\n')"
  [ -n "$version" ] || fail "latest pointer was empty"
fi
# The download contract keys paths on bare semver; tolerate a pinned leading v.
version="${version#v}"

base="$host/dl/muster/$version"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# sha256 the given file, using whichever tool this OS ships (shasum on macOS,
# sha256sum on Linux).
sha256_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    sha256sum "$1" | cut -d' ' -f1
  fi
}

# fetch_binary <binary-name> — download that binary's tarball and its sha256
# sidecar for this platform, verify FAIL-CLOSED, and install the binary.
fetch_binary() {
  name="$1"
  asset="${name}_${os}_${arch}.tar.gz"
  url="$base/$asset"

  printf 'downloading %s (%s) …\n' "$asset" "$version"
  curl -fsSL "$url"        -o "$tmp/$asset"        || fail "download failed: $url"
  curl -fsSL "$url.sha256" -o "$tmp/$asset.sha256" || fail "download failed: $url.sha256"

  # The sidecar is `shasum -a 256` output (`<hex>  <name>`); take the hex.
  expected="$(cut -d' ' -f1 < "$tmp/$asset.sha256")"
  [ -n "$expected" ] || fail "empty checksum for $asset"
  actual="$(sha256_of "$tmp/$asset")"
  [ "$actual" = "$expected" ] || fail "checksum mismatch for $asset (expected $expected, got $actual)"

  mkdir -p "$install_dir"
  tar -xzf "$tmp/$asset" -C "$tmp"
  mv "$tmp/$name" "$install_dir/$name"
  chmod +x "$install_dir/$name"
  printf 'installed %s to %s\n' "$name" "$install_dir/$name"
}

fetch_binary muster
# Exit 2 is muster's usage code, which a bare invocation returns — so this
# confirms the binary loads and runs on this machine without doing anything.
"$install_dir/muster" >/dev/null 2>&1 || [ $? -eq 2 ] || fail "installed binary failed to run"

if [ "$with_deploy" -eq 1 ]; then
  fetch_binary muster-deploy
  # -version, NOT a bare run: muster-deploy's job is to create real AWS
  # infrastructure, and a smoke test must never be capable of starting one.
  "$install_dir/muster-deploy" -version >/dev/null 2>&1 || fail "installed muster-deploy failed to run"
fi

case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) printf 'note: %s is not on your PATH — add:  export PATH="%s:$PATH"\n' "$install_dir" "$install_dir" ;;
esac
printf 'next: register it with your agent, e.g.  claude mcp add muster -s user -- muster mcp\n'
if [ "$with_deploy" -eq 0 ]; then
  printf 'one bus across several machines? re-run with --with-deploy for the hosted backend installer\n'
fi
