#!/bin/sh
set -eu

dest="${WR_INSTALL_DIR:-$HOME/.local/bin}"
binary="$dest/wr"
if [ -f "$binary" ]; then
  rm -f "$binary"
  echo "removed $binary"
else
  echo "wr is not installed at $binary"
fi
