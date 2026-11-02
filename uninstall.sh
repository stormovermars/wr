#!/bin/sh
set -eu

# wr and mw install together, so they uninstall together.
dest="${WR_INSTALL_DIR:-$HOME/.local/bin}"
for b in wr mw; do
  if [ -f "$dest/$b" ]; then
    rm -f "$dest/$b"
    echo "removed $dest/$b"
  else
    echo "$b is not installed at $dest/$b"
  fi
done
