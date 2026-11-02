#!/bin/sh
set -eu

repo="${WR_REPO:-stormovermars/wr}"
dest="${WR_INSTALL_DIR:-$HOME/.local/bin}"
version="${WR_VERSION:-latest}"

case "$(uname -s)" in
  Darwin) os=Darwin ;;
  Linux) os=Linux ;;
  *) echo "use install.ps1 on Windows" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64) arch=arm64 ;;
  aarch64) arch=arm64 ;;
  x86_64) arch=x86_64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

# fetch REPO ASSET VERSION DIR: download a release asset, check it against the release's
# checksums.txt and unpack it into DIR. Returns non-zero on any failure.
fetch() {
  if [ "$3" = latest ]; then
    u="https://github.com/$1/releases/latest/download/$2"
  else
    u="https://github.com/$1/releases/download/$3/$2"
  fi
  mkdir -p "$4" || return 1
  curl -fsSL "$u" -o "$4/$2" || return 1
  curl -fsSL "${u%/*}/checksums.txt" -o "$4/checksums.txt" || return 1
  expected="$(awk -v file="$2" '$2 == file || $2 == "*" file { print $1 }' "$4/checksums.txt")"
  [ -n "$expected" ] || { echo "release checksum not found for $2" >&2; return 1; }
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$4/$2" | awk '{print $1}')"
  else
    actual="$(shasum -a 256 "$4/$2" | awk '{print $1}')"
  fi
  [ "$actual" = "$expected" ] || { echo "release checksum mismatch for $2" >&2; return 1; }
  tar -xzf "$4/$2" -C "$4"
}

mkdir -p "$dest"
fetch "$repo" "wr_${os}_${arch}.tar.gz" "$version" "$tmp" || exit 1
install -m 0755 "$tmp/wr" "$dest/wr"
echo "installed wr to $dest/wr"

# mw installs with wr, always its latest release. Best effort: a failure is a note.
mw_repo="${MINIWAVES_REPO:-stormovermars/miniwaves}"
if fetch "$mw_repo" "miniwaves_${os}_${arch}.tar.gz" latest "$tmp/mw" && install -m 0755 "$tmp/mw/mw" "$dest/mw"; then
  echo "installed mw to $dest/mw"
  # mw's fanfare defaults and Claude Code plugins, as mw's own installer sets them (best effort).
  "$dest/mw" update --plugins || true
else
  echo "mw: not installed (download failed); retry: wr update"
fi
case ":$PATH:" in *":$dest:"*) ;; *) echo "add $dest to PATH: export PATH=\"$dest:\$PATH\"" ;; esac
