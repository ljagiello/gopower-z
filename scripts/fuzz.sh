#!/usr/bin/env bash
# Run every Go fuzz target in the module for FUZZTIME each (default 30s).
# Usage: scripts/fuzz.sh [fuzztime]
# Crashers are written to <pkg>/testdata/fuzz/<Target>/ - commit them as
# regression inputs after fixing the bug.
set -euo pipefail
fuzztime="${1:-${FUZZTIME:-30s}}"
status=0
for pkg in $(go list ./...); do
  for target in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true); do
    echo "=== $pkg $target ($fuzztime)"
    if ! go test -run='^$' -fuzz="^${target}\$" -fuzztime="$fuzztime" "$pkg"; then
      status=1
    fi
  done
done
exit "$status"
