#!/usr/bin/env -S pkgx bash
#
# run-all.sh — drive 01..08 in order against a freshly-converged cluster.
# Each step is short ; the whole sweep should finish in under 5 minutes
# on a healthy 3-DC cluster. Failures don't short-circuit — every gate
# runs so the operator sees the full picture in one log.
#
# Usage:
#   scripts/validation/run-all.sh <host1> [<host2> <host3> ...]
#
# Or equivalently:
#   HOSTS=10.0.0.1,10.0.0.2,10.0.0.3 scripts/validation/run-all.sh
#
# Other env that downstream scripts need (set before invoking):
#   For 02 :   ETCD_ENDPOINTS
#   For 03 :   OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET
#   For 06 :   PROXY_HOST
#   For 08 :   WEFT_AGENT, NON_ADMIN_TOKEN
#
# Exit 0 iff every step exits 0. Otherwise prints a per-step summary
# and exits with the number of failed steps (capped at 125 to stay
# within POSIX exit-status range).

set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"

# Accept positional args OR HOSTS env. CLI wins.
if (( $# > 0 )); then
  HOSTS=$(IFS=,; echo "$*")
  export HOSTS
fi
: "${HOSTS:?provide hosts as positional args OR HOSTS env (comma-separated)}"

steps=(
  "01-hosts-running.sh"
  "02-etcd-quorum.sh"
  "03-oidc-login.sh"
  "04-vm-roundtrip.sh"
  "05-snapshot-roundtrip.sh"
  "06-proxy-acme.sh"
  "07-metrics-shape.sh"
  "08-audit-log-write.sh"
)

declare -a results=()
fails=0

for s in "${steps[@]}"; do
  echo ""
  echo "============================================================"
  echo "  ${s}"
  echo "============================================================"
  if "${here}/${s}"; then
    results+=("PASS  ${s}")
  else
    rc=$?
    results+=("FAIL  ${s} (rc=${rc})")
    fails=$((fails + 1))
  fi
done

echo ""
echo "============================================================"
echo "  validation summary (HOSTS=${HOSTS})"
echo "============================================================"
for r in "${results[@]}"; do
  echo "  $r"
done
echo "------------------------------------------------------------"
if (( fails == 0 )); then
  echo "  PASS — ${#steps[@]}/${#steps[@]} steps green"
  exit 0
fi
echo "  FAIL — ${fails}/${#steps[@]} step(s) red"
# Cap exit code so callers can still tell pass from fail.
(( fails > 125 )) && fails=125
exit "$fails"
