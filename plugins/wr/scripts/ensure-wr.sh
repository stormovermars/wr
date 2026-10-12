#!/bin/bash
# SessionStart hook: make sure `wr` is installed and at least the plugin's version,
# and that mw >= 0.2.0 is there (wr refuses older mw). Silent when both are fine;
# otherwise installs the matching wr GitHub release into ~/.local/bin.
set -u
want="0.2.0"
min_mw="0.2.0"
ver_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]; } # ver_ge HAVE WANT
mw_have=$(mw version -j 2>/dev/null | sed -n 's/.*"version": *"\([0-9.]*\)".*/\1/p' | head -n1)
if [ -z "$mw_have" ] || ! ver_ge "$mw_have" "$min_mw"; then
  echo "wr needs mw >= ${min_mw} (found: ${mw_have:-none}); install it: claude plugin marketplace add stormovermars/miniwaves && claude plugin install mw@miniwaves" >&2
fi
wrbin="$HOME/.local/bin/wr"; [ -x "$wrbin" ] || wrbin=$(command -v wr 2>/dev/null || true)
have=$("$wrbin" version 2>/dev/null | sed -n 's/^wr \([0-9.]*\).*/\1/p')
if [ -n "$have" ] && ver_ge "$have" "$want"; then
  exit 0
fi
case "$(uname -s)" in Darwin) os=Darwin ;; Linux) os=Linux ;; *) echo "wr: unsupported OS $(uname -s); see github.com/stormovermars/wr" >&2; exit 0 ;; esac
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64) arch=x86_64 ;; *) echo "wr: unsupported CPU $(uname -m)" >&2; exit 0 ;; esac
name="wr_${os}_${arch}.tar.gz"
base="https://github.com/stormovermars/wr/releases/download/v${want}"
tmp=$(mktemp -d) || exit 0
trap 'rm -rf "$tmp"' EXIT
if ! curl -fsSL "$base/$name" -o "$tmp/$name" || ! curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"; then
  echo "wr: download failed ($base); install manually" >&2; exit 0
fi
expected=$(awk -v f="$name" '$2 == f { print $1 }' "$tmp/checksums.txt")
if command -v sha256sum >/dev/null 2>&1; then actual=$(sha256sum "$tmp/$name" | awk '{print $1}'); else actual=$(shasum -a 256 "$tmp/$name" | awk '{print $1}'); fi
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then echo "wr: checksum mismatch, not installed" >&2; exit 0; fi
tar -xzf "$tmp/$name" -C "$tmp" wr || exit 0
mkdir -p "$HOME/.local/bin" && install -m 755 "$tmp/wr" "$HOME/.local/bin/wr" && echo "wr ${want} installed to ~/.local/bin/wr (add it to PATH if needed)"
exit 0
