#!/bin/sh
# CI helper: install the miniwaves release wr is tested against into $1 (default ~/.local/bin),
# checksum verified. Usage: scripts/install-mw.sh [DIR]
set -eu
want="${MW_VERSION:-0.2.0}"
dest="${1:-$HOME/.local/bin}"
case "$(uname -s)" in Darwin) os=Darwin ;; Linux) os=Linux ;; *) echo "unsupported OS" >&2; exit 1 ;; esac
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64) arch=x86_64 ;; *) echo "unsupported CPU" >&2; exit 1 ;; esac
name="miniwaves_${os}_${arch}.tar.gz"
base="https://github.com/stormovermars/miniwaves/releases/download/v${want}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$base/$name" -o "$tmp/$name"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
expected="$(awk -v f="$name" '$2 == f { print $1 }' "$tmp/checksums.txt")"
if command -v sha256sum >/dev/null 2>&1; then actual="$(sha256sum "$tmp/$name" | awk '{print $1}')"; else actual="$(shasum -a 256 "$tmp/$name" | awk '{print $1}')"; fi
[ -n "$expected" ] && [ "$expected" = "$actual" ] || { echo "mw checksum mismatch" >&2; exit 1; }
tar -xzf "$tmp/$name" -C "$tmp" mw
mkdir -p "$dest"
install -m 0755 "$tmp/mw" "$dest/mw"
echo "installed mw $want to $dest/mw"
