#!/bin/bash
# SessionStart hook: make sure `wr` is installed and at least the plugin's version,
# and that mw >= 0.2.1 is there (wr refuses older mw). Silent when both are fine;
# otherwise installs the matching wr GitHub release into ~/.local/bin.
# On Windows Claude Code runs hooks in Git Bash: uname -s says MINGW*/MSYS*/CYGWIN*, the
# asset is the .zip and the binary wr.exe.
set -u
exe=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) exe=".exe" ;; esac
want="0.2.2"
min_mw="0.2.1"
ver_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]; } # ver_ge HAVE WANT
mw_have=$(mw version -j 2>/dev/null | sed -n 's/.*"version": *"\([0-9.]*\)".*/\1/p' | head -n1)
if [ -z "$mw_have" ] || ! ver_ge "$mw_have" "$min_mw"; then
  echo "wr needs mw >= ${min_mw} (found: ${mw_have:-none}); install it: claude plugin marketplace add stormovermars/miniwaves && claude plugin install mw@miniwaves" >&2
fi
wrbin="$HOME/.local/bin/wr$exe"; [ -x "$wrbin" ] || wrbin=$(command -v wr 2>/dev/null || true)
have=$("$wrbin" version 2>/dev/null | sed -n 's/^wr \([0-9.]*\).*/\1/p')
if [ -n "$have" ] && ver_ge "$have" "$want"; then
  exit 0
fi
case "$(uname -s)" in Darwin) os=Darwin ;; Linux) os=Linux ;; MINGW*|MSYS*|CYGWIN*) os=Windows ;; *) echo "wr: unsupported OS $(uname -s); see github.com/stormovermars/wr" >&2; exit 0 ;; esac
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64) arch=x86_64 ;; *) echo "wr: unsupported CPU $(uname -m)" >&2; exit 0 ;; esac
if [ "$os" = Windows ]; then name="wr_${os}_${arch}.zip"; else name="wr_${os}_${arch}.tar.gz"; fi
base="https://github.com/stormovermars/wr/releases/download/v${want}"
tmp=$(mktemp -d) || exit 0
trap 'rm -rf "$tmp"' EXIT
if ! curl -fsSL "$base/$name" -o "$tmp/$name" || ! curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"; then
  echo "wr: download failed ($base); install manually" >&2; exit 0
fi
expected=$(awk -v f="$name" '$2 == f { print $1 }' "$tmp/checksums.txt")
if command -v sha256sum >/dev/null 2>&1; then actual=$(sha256sum "$tmp/$name" | awk '{print $1}'); else actual=$(shasum -a 256 "$tmp/$name" | awk '{print $1}'); fi
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then echo "wr: checksum mismatch, not installed" >&2; exit 0; fi
if [ "$os" = Windows ]; then
  if command -v unzip >/dev/null 2>&1; then unzip -q -o "$tmp/$name" wr.exe -d "$tmp" || exit 0
  else powershell.exe -NoProfile -Command "Expand-Archive -Force '$(cygpath -w "$tmp/$name" 2>/dev/null || echo "$tmp/$name")' '$(cygpath -w "$tmp" 2>/dev/null || echo "$tmp")'" >/dev/null || exit 0; fi
else
  tar -xzf "$tmp/$name" -C "$tmp" wr || exit 0
fi
mkdir -p "$HOME/.local/bin" && cp "$tmp/wr$exe" "$HOME/.local/bin/wr$exe" && chmod 755 "$HOME/.local/bin/wr$exe" && echo "wr ${want} installed to ~/.local/bin/wr$exe (add it to PATH if needed)"
exit 0
