#!/usr/bin/env -S pkgx bash
#
# mesh-fanout.sh — measure time-to-mesh-connectivity for a fresh microVM.
#
# Starts a probe microVM in the cluster, then for each peer in the existing
# fleet checks `nc -z` (TCP reachability) on its overlay IP until success.
# Reports P50/P95/P99 of the per-peer reach time. The aggregate "fanout"
# wall-clock is the slowest peer to come into range.
#
# Usage:
#   scripts/perf/mesh-fanout.sh <N> [--peer-port 22] [--timeout 60s]
#
# N is the expected fleet size — used for sanity-checking that the
# discovered peer count matches what the operator thinks is deployed.
#
# Output (stdout):
#   N=<n>  reachable=<r>  P50=<ms>  P95=<ms>  P99=<ms>  max=<ms>
#
# Target: P95 < 30s for N=100. Beyond that, suspect WireGuard handshake
# saturation (one wg device per host) or pubkey gossip lag.

set -euo pipefail

N="${1:-}"
if [[ -z "$N" || ! "$N" =~ ^[0-9]+$ ]]; then
  echo "usage: $0 <N> [--peer-port 22] [--timeout 60s]" >&2
  exit 2
fi
shift

PORT=22
TIMEOUT=60
while [[ $# -gt 0 ]]; do
  case "$1" in
    --peer-port) PORT="$2"; shift 2 ;;
    --timeout)   TIMEOUT="${2%s}"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

WEFT="${WEFT:-weft}"
PROBE="mesh-probe-$(date +%s)"

echo "registering probe VM: $PROBE"
"$WEFT" microvm register --name "$PROBE" --image weft-test-fixture:latest >/dev/null
"$WEFT" microvm start "$PROBE" >/dev/null
"$WEFT" microvm wait "$PROBE" --state=running --timeout=120s >/dev/null

# Pull peer overlay IPs from the agent's view of the mesh.
mapfile -t PEERS < <("$WEFT" microvm list --format=json \
  | "${JQ:-jq}" -r --arg p "$PROBE" '.[] | select(.name != $p) | .overlay_ip // empty')

PEER_COUNT="${#PEERS[@]}"
echo "discovered $PEER_COUNT peers (expected ~$N)"

TS_FILE=$(mktemp -t mesh-fanout.XXXXXX)
trap 'rm -f "$TS_FILE"' EXIT

probe_one() {
  local peer="$1"
  local deadline=$(( $(date +%s) + TIMEOUT ))
  local t0
  t0=$(date +%s%3N)
  while (( $(date +%s) < deadline )); do
    if "$WEFT" microvm exec "$PROBE" -- nc -z -w1 "$peer" "$PORT" 2>/dev/null; then
      printf '%d\n' $(( $(date +%s%3N) - t0 ))
      return 0
    fi
    sleep 0.25
  done
  printf 'TIMEOUT\n'
  return 1
}
export -f probe_one
export PROBE PORT TIMEOUT WEFT

printf '%s\n' "${PEERS[@]}" \
  | xargs -n1 -P 32 -I{} bash -c 'probe_one "$@"' _ {} >> "$TS_FILE" || true

REACHED=$(grep -v TIMEOUT "$TS_FILE" | wc -l | tr -d ' ')
SORTED=$(grep -v TIMEOUT "$TS_FILE" | sort -n)

pct() {
  local p="$1"
  echo "$SORTED" | awk -v p="$p" -v n="$REACHED" '
    BEGIN { i = int((p/100) * n + 0.5); if (i < 1) i = 1 }
    NR == i { print; exit }'
}

if (( REACHED > 0 )); then
  P50=$(pct 50); P95=$(pct 95); P99=$(pct 99)
  MAX=$(echo "$SORTED" | tail -n1)
  printf 'N=%d  reachable=%d  P50=%sms  P95=%sms  P99=%sms  max=%sms\n' \
    "$PEER_COUNT" "$REACHED" "$P50" "$P95" "$P99" "$MAX"
else
  echo "no peers reachable within ${TIMEOUT}s — check mesh state, wg pubkey gossip" >&2
  exit 1
fi

"$WEFT" microvm delete "$PROBE" >/dev/null || true
