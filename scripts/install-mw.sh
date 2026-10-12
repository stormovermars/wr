#!/bin/sh
# CI helper: install the miniwaves release wr is tested against into $1 (default ~/.local/bin),
# checksum verified. Usage: scripts/install-mw.sh [DIR]
set -eu
want="${MW_VERSION:-0.2.1}"
dest="${1:-$HOME/.local/bin}"
exe=""
case "$(uname -s)" in Darwin) os=Darwin ;; Linux) os=Linux ;; MINGW*|MSYS*|CYGWIN*) os=Windows; exe=".exe" ;; *) echo "unsupported OS" >&2; exit 1 ;; esac
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64) arch=x86_64 ;; *) echo "unsupported CPU" >&2; exit 1 ;; esac
if [ "$os" = Windows ]; then name="miniwaves_${os}_${arch}.zip"; else name="miniwaves_${os}_${arch}.tar.gz"; fi
base="https://github.com/stormovermars/miniwaves/releases/download/v${want}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$base/$name" -o "$tmp/$name"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
expected="$(awk -v f="$name" '$2 == f { print $1 }' "$tmp/checksums.txt")"
if command -v sha256sum >/dev/null 2>&1; then actual="$(sha256sum "$tmp/$name" | awk '{print $1}')"; else actual="$(shasum -a 256 "$tmp/$name" | awk '{print $1}')"; fi
[ -n "$expected" ] && [ "$expected" = "$actual" ] || { echo "mw checksum mismatch" >&2; exit 1; }
if [ "$os" = Windows ]; then unzip -q -o "$tmp/$name" mw.exe -d "$tmp"; else tar -xzf "$tmp/$name" -C "$tmp" mw; fi
mkdir -p "$dest"
cp "$tmp/mw$exe" "$dest/mw$exe" && chmod 0755 "$dest/mw$exe"
echo "installed mw $want to $dest/mw$exe"
