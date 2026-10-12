#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
out="${WR_BUILD_DIR:-$root/build}"
action="${1:-build}"

case "$action" in
  build)
    mkdir -p "$out"
    (cd "$root" && go build -trimpath -o "$out/wr" .)
    echo "built $out/wr"
    ;;
  clean)
    rm -f "$out/wr" "$out/wr.exe"
    rmdir "$out" 2>/dev/null || true
    echo "cleaned $out"
    ;;
  *)
    echo "usage: ./build.sh [build|clean]" >&2
    exit 2
    ;;
esac
