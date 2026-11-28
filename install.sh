#!/bin/sh
# Installs the Differently CLI and server from a GitHub release.
#
#   curl -fsSL https://different-ai.github.io/differently/install.sh | sh
#
# It downloads the release archive for this machine, checks it against the
# release's checksums.txt, and copies `differently` and `differently-server`
# into a directory. It never uses sudo and changes nothing else.
#
# Settings (environment variables):
#   DIFFERENTLY_VERSION       release to install, e.g. v0.1.0-alpha.9
#                             (default: the newest release, pre-releases included)
#   DIFFERENTLY_INSTALL_DIR   where the two binaries go (default: ~/.local/bin)
#   DIFFERENTLY_DOWNLOAD_URL  where releases are served from, for a mirror
#                             (default: https://github.com/different-ai/differently/releases/download)
set -eu

REPO="different-ai/differently"

say() { printf '%s\n' "$*"; }
fail() {
  printf 'install: %s\n' "$*" >&2
  exit 1
}

# fetch URL writes the response body to stdout.
fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- "$1"
  else
    fail "curl or wget is needed to download the release"
  fi
}

# newest_version prints the tag of the newest release. Every release so far is
# a pre-release, which GitHub's "latest" link skips, so the list is read
# instead: the API first, then the releases feed when the API does not answer
# (its unauthenticated limit is shared by everyone behind one address).
newest_version() {
  tag=$(fetch "https://api.github.com/repos/$REPO/releases?per_page=1" 2>/dev/null |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1) || true
  if [ -z "${tag:-}" ]; then
    tag=$(fetch "https://github.com/$REPO/releases.atom" 2>/dev/null |
      sed -n 's|.*/releases/tag/\([^"]*\)".*|\1|p' | head -n 1) || true
  fi
  printf '%s' "${tag:-}"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d ' ' -f 1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d ' ' -f 1
  else
    fail "sha256sum or shasum is needed to check the download"
  fi
}

main() {
  command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 ||
    fail "curl or wget is needed to download the release"
  command -v tar >/dev/null 2>&1 || fail "tar is needed to unpack the release"

  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) fail "no release for $(uname -s). Releases cover Linux and macOS; on Windows use WSL, or build from source with 'make build'" ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) fail "no release for $(uname -m) processors. Build from source with 'make build'" ;;
  esac

  version="${DIFFERENTLY_VERSION:-}"
  if [ -z "$version" ]; then
    version=$(newest_version)
    [ -n "$version" ] || fail "could not find the newest release. Set DIFFERENTLY_VERSION, for example DIFFERENTLY_VERSION=v0.1.0-alpha.9 (see https://github.com/$REPO/releases)"
  fi
  case "$version" in
    v*) ;;
    *) version="v$version" ;;
  esac
  case "$version" in
    v[0-9]*) ;;
    *) fail "'$version' is not a release version such as v0.1.0-alpha.9" ;;
  esac
  case "$version" in
    *[!A-Za-z0-9.+-]*) fail "'$version' is not a release version such as v0.1.0-alpha.9" ;;
  esac

  base="${DIFFERENTLY_DOWNLOAD_URL:-https://github.com/$REPO/releases/download}"
  name="differently_${version}_${os}_${arch}"
  dir="${DIFFERENTLY_INSTALL_DIR:-$HOME/.local/bin}"

  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT INT TERM

  say "Downloading Differently $version for $os/$arch"
  fetch "$base/$version/$name.tar.gz" >"$tmp/$name.tar.gz" ||
    fail "could not download $base/$version/$name.tar.gz"
  fetch "$base/$version/checksums.txt" >"$tmp/checksums.txt" ||
    fail "could not download $base/$version/checksums.txt"

  want=$(awk -v f="$name.tar.gz" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/checksums.txt")
  [ -n "$want" ] || fail "checksums.txt does not list $name.tar.gz; nothing was installed"
  got=$(sha256_of "$tmp/$name.tar.gz")
  [ "$want" = "$got" ] || fail "checksum mismatch for $name.tar.gz (expected $want, got $got); nothing was installed"

  tar -xzf "$tmp/$name.tar.gz" -C "$tmp"
  for bin in differently differently-server; do
    [ -f "$tmp/$name/$bin" ] || fail "the archive does not contain $bin; nothing was installed"
  done
  mkdir -p "$dir"
  for bin in differently differently-server; do
    # Write beside the target and rename, so a running copy is never half-written.
    cp "$tmp/$name/$bin" "$dir/.$bin.new"
    chmod 755 "$dir/.$bin.new"
    mv -f "$dir/.$bin.new" "$dir/$bin"
  done

  say "Installed differently and differently-server $version in $dir"
  case ":$PATH:" in
    *":$dir:"*) ;;
    *)
      say ""
      say "$dir is not on your PATH. Add it for this shell with:"
      say "  export PATH=\"$dir:\$PATH\""
      ;;
  esac
  say ""
  say "Next:"
  say "  differently-server --dev         # a local server; commands run on this machine with no isolation"
  # Releases up to v0.1.0-alpha.5 have no "run": show what works with this one.
  if "$dir/differently" run --help >/dev/null 2>&1; then
    say "  differently run -- echo hello    # in another terminal"
  else
    say "  differently sandbox create --wait                 # in another terminal; prints the sandbox ID"
    say "  differently sandbox exec <id> -- echo hello"
  fi
  say ""
  say "Guide: https://different-ai.github.io/differently/docs/guides/quickstart/"
}

# Nothing runs until the whole script has arrived.
main "$@"
