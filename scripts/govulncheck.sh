#!/usr/bin/env bash
#
# Runs govulncheck and fails only on vulnerabilities that are BOTH reachable from
# kraze's own code and not explicitly allowlisted below.
#
# govulncheck has no native suppression mechanism, and kraze depends on modules
# that periodically carry advisories with no fixed version available. Without an
# allowlist the gate would be permanently red and would stop being read. Every
# entry needs a reason and a re-check trigger.
#
# Usage: scripts/govulncheck.sh [-v]

set -euo pipefail

# OSV IDs that do not fail the build. Review these whenever dependencies are
# bumped: an entry whose advisory has since been fixed should be REMOVED so the
# fix actually gets picked up, not left here indefinitely.
#
# Entry format: "# <OSV ID> (<CVE>): why it is not exploitable through kraze, and
# the re-check trigger that should remove it", then the ID in the array.
#
# (Empty: the last entries were Moby daemon advisories matched against the
# github.com/docker/docker module, which kraze no longer depends on.)
ALLOWLIST=()

cd "$(dirname "$0")/.."

if ! command -v jq >/dev/null 2>&1; then
  echo "error: jq is required by $0" >&2
  exit 2
fi

echo "Running govulncheck..."
json=$(go run golang.org/x/vuln/cmd/govulncheck@latest -format json ./...)

module=$(go list -m)

# govulncheck emits findings at three levels of confidence. Module- and
# package-level findings only say the code is somewhere in the dependency graph;
# a SYMBOL-level finding (trace[0].function is set) says the vulnerable function
# is actually reachable. trace[0] is the vulnerable symbol itself and trace[-1]
# is the outermost caller, so requiring that outermost frame to be this module is
# what distinguishes "kraze calls it" from "some other dependency calls it".
mapfile -t reachable < <(
  jq -r --arg mod "$module" '
    select(.finding != null)
    | .finding
    | select(.trace[0].function != null)
    | select(.trace[-1].module == $mod)
    | .osv
  ' <<<"$json" | sort -u
)

fail=0
for osv in "${reachable[@]:-}"; do
  [[ -z "$osv" ]] && continue
  allowed=0
  # The ${var+...} form keeps an empty array safe under set -u on bash < 4.4 (macOS)
  for a in ${ALLOWLIST[@]+"${ALLOWLIST[@]}"}; do
    [[ "$osv" == "$a" ]] && allowed=1 && break
  done

  summary=$(jq -r --arg id "$osv" '
    select(.osv != null) | .osv | select(.id == $id) | .summary
  ' <<<"$json" | head -1)

  if [[ "$allowed" == 1 ]]; then
    echo "  allowlisted: $osv - $summary"
  else
    echo "  VULNERABLE:  $osv - $summary"
    echo "               https://pkg.go.dev/vuln/$osv"
    fail=1
  fi
done

if [[ "$fail" == 1 ]]; then
  echo
  echo "govulncheck found reachable vulnerabilities that are not allowlisted."
  echo "Fix by bumping the affected module, or add an entry with a written"
  echo "justification to the ALLOWLIST in $0 if it genuinely does not apply."
  exit 1
fi

echo "govulncheck: no unallowlisted reachable vulnerabilities."
