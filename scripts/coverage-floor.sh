#!/usr/bin/env bash
#
# Fails if total statement coverage in a Go coverage profile is below the
# floor. The floor guards the hermetic test suite (fakes in place of kind,
# Docker and clusters; see "Seams" in CLAUDE.md): coverage that drops is
# usually code that went back to calling the outside world directly.
#
# Raise FLOOR when coverage rises, to just under what CI measures (CI has no
# Docker; the Docker tests that need a real daemon skip there). Lowering it needs a reason in
# the commit.
#
# Usage: scripts/coverage-floor.sh <coverage.out>

set -euo pipefail

FLOOR=99.3

profile=${1:?usage: $0 <coverage.out>}

total=$(go tool cover -func="$profile" | awk '/^total:/ { sub("%", "", $3); print $3 }')
if [ -z "$total" ]; then
  echo "error: no total in $profile" >&2
  exit 2
fi

echo "Total coverage: ${total}% (floor ${FLOOR}%)"
if awk -v t="$total" -v f="$FLOOR" 'BEGIN { exit !(t < f) }'; then
  echo "error: coverage ${total}% is below the floor of ${FLOOR}%" >&2
  exit 1
fi
