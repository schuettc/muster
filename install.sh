#!/bin/sh
# muster installer — downloads the latest release binaries for this machine,
# verifies their checksums, and installs them to ~/.local/bin (override with
# MUSTER_INSTALL_DIR). Usage:
#
#   curl -fsSL https://muster.tools/install.sh | sh
#
# muster-deploy, which stands up the optional hosted backend in your own AWS
# account, is NOT installed by default. It is needed on one machine, once, and
# only if you want a bus spanning several devices — and it links the AWS SDK,
# which the muster binary deliberately does not. Ask for it explicitly:
#
#   curl -fsSL https://muster.tools/install.sh | sh -s -- --with-deploy
#
set -eu

repo="schuettc/muster"
install_dir="${MUSTER_INSTALL_DIR:-$HOME/.local/bin}"
base="https://github.com/$repo/releases/latest/download"
with_deploy=0

fail() { printf 'muster install: %s\n' "$*" >&2; exit 1; }

for arg in "$@"; do
  case "$arg" in
    --with-deploy) with_deploy=1 ;;
    -h|--help)
      printf 'usage: install.sh [--with-deploy]\n'
      printf '  --with-deploy  also install muster-deploy (hosted backend installer)\n'
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

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" || fail "download failed: checksums.txt"

# sha256 the given file, using whichever tool this OS ships (shasum on macOS,
# sha256sum on Linux).
sha256_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    sha256sum "$1" | cut -d' ' -f1
  fi
}

# fetch_binary <binary-name> — download that binary's tarball for this
# platform, verify it against checksums.txt, and install the binary.
fetch_binary() {
  name="$1"
  asset="${name}_${os}_${arch}.tar.gz"

  printf 'downloading %s …\n' "$asset"
  curl -fsSL "$base/$asset" -o "$tmp/$asset" || fail "download failed: $base/$asset"

  expected="$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
  [ -n "$expected" ] || fail "no checksum found for $asset"
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
