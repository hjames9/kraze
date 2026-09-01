#!/usr/bin/env bash
#
# Runs govulncheck and fails only on vulnerabilities that are BOTH reachable from
# kraze's own code and not explicitly allowlisted below.
#
# govulncheck has no native suppression mechanism, and kraze depends on modules
# (notably github.com/docker/docker) that periodically carry advisories with no
# fixed version available. Without an allowlist the gate would be permanently red
# and would stop being read. Every entry needs a reason and a re-check trigger.
#
# Usage: scripts/govulncheck.sh [-v]

set -euo pipefail

# OSV IDs that do not fail the build. Review these whenever dependencies are
# bumped: an entry whose advisory has since been fixed should be REMOVED so the
# fix actually gets picked up, not left here indefinitely.
#
# GO-2026-4887 (CVE-2026-34040) / GO-2026-4883 (CVE-2026-33997):
#   Moby AuthZ plugin bypass and plugin-privilege off-by-one. Both are flaws in
#   the Docker *daemon*, fixed in Docker Engine 29.3.1. kraze links
#   github.com/docker/docker only as an API client (inspect containers, manage
#   networks) and never executes daemon-side plugin code, so neither is
#   exploitable through this binary. Whoever runs the daemon fixes them by
#   upgrading Docker Engine; bumping this module cannot.
#
#   They are unfixable *here* because the fix landed under Moby's renamed module
#   path: OSV lists github.com/moby/moby/v2 as fixed in 2.0.0-beta.8, while the
#   github.com/docker/docker path kraze imports carries an open-ended
#   "introduced: 0" range with no fixed event -- hence "Fixed in: N/A". Escaping
#   these would mean migrating to the github.com/moby/moby/v2 import path, not a
#   version bump.
#
#   They also match at MODULE level, not symbol level: the OSV records carry no
#   affected-symbol list, so govulncheck cannot narrow them and flags ordinary
#   client calls (client.init, Ping, Close) as reachable. That is why they are
#   reachable-but-not-applicable rather than simply unreachable.
#
#   Verify the claim that no vulnerable code is linked with:
#     go list -deps ./cmd/kraze | grep -E "docker.*(plugin|authorization|daemon)"
#   That must stay empty -- kraze should link only api/types/* and client. If it
#   ever prints something, this allowlist entry is void and needs re-review.
#
#   Re-check trigger: drop these if kraze migrates to github.com/moby/moby/v2,
#   or if the docker/docker range ever gains a fixed event.
ALLOWLIST=(
  GO-2026-4887
  GO-2026-4883
)

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
  for a in "${ALLOWLIST[@]}"; do
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
