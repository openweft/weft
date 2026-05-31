#!/usr/bin/env -S pkgx bash
#
# etcd-write-rate.sh — stress weft's etcd state with bounded-rate writes
# and report req/s + P99 latency. Wrap as a one-liner before a cluster
# reshuffle to confirm the quorum can absorb the upcoming burst.
#
# Usage:
#   scripts/perf/etcd-write-rate.sh [--total 10000] [--parallel 16] \
#                                   [--key-prefix /weft/perf] \
#                                   [--endpoints 127.0.0.1:2379]
#
# Output:
#   wrote=<n>  duration=<s>  rate=<req/s>  P50=<ms>  P99=<ms>
#
# Target P99 < 50ms on a healthy 3-node etcd quorum on local SSD.
# >100ms P99 generally means: slow disk (rotational, NFS, virtio-fs on
# the host), network jitter across the DCs, or quorum membership flap.
#
# Cleans up the test prefix on exit. Idempotent — safe to re-run.

set -euo pipefail

TOTAL=10000
PARALLEL=16
PREFIX="/weft/perf/$(date +%s)"
ENDPOINTS="${ETCDCTL_ENDPOINTS:-127.0.0.1:2379}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --total)      TOTAL="$2"; shift 2 ;;
    --parallel)   PARALLEL="$2"; shift 2 ;;
    --key-prefix) PREFIX="$2"; shift 2 ;;
    --endpoints)  ENDPOINTS="$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

ETCDCTL="${ETCDCTL:-etcdctl}"
command -v "$ETCDCTL" >/dev/null || { echo "etcdctl not on PATH"; exit 1; }
export ETCDCTL_API=3
export ETCDCTL_ENDPOINTS="$ENDPOINTS"

LAT_FILE=$(mktemp -t etcd-rate.XXXXXX)
trap 'rm -f "$LAT_FILE"; "$ETCDCTL" del --prefix "$PREFIX" >/dev/null 2>&1 || true' EXIT

write_one() {
  local idx="$1"
  local key="$PREFIX/k-$idx"
  local val
  val=$(head -c 256 /dev/urandom | base64 | head -c 256)
  local t0
  t0=$(date +%s%3N)
  # Wrap in a txn so we exercise the same code path the weft state uses
  # (compare-revision + put), not raw put.
  printf 'mod("%s") < 100\n\nput %s "%s"\n\n' "$key" "$key" "$val" \
    | "$ETCDCTL" txn >/dev/null
  printf '%d\n' $(( $(date +%s%3N) - t0 ))
}
export -f write_one
export PREFIX ETCDCTL ETCDCTL_API ETCDCTL_ENDPOINTS

WALL_START=$(date +%s%3N)
seq 1 "$TOTAL" \
  | xargs -n1 -P "$PARALLEL" -I{} bash -c 'write_one "$@"' _ {} >> "$LAT_FILE"
WALL_END=$(date +%s%3N)

DUR_MS=$((WALL_END - WALL_START))
DUR_S=$(awk -v d="$DUR_MS" 'BEGIN{printf "%.2f", d/1000}')
WROTE=$(wc -l < "$LAT_FILE" | tr -d ' ')
RATE=$(awk -v w="$WROTE" -v d="$DUR_MS" 'BEGIN{printf "%.0f", (w*1000)/d}')

SORTED=$(sort -n "$LAT_FILE")
pct() {
  local p="$1"
  echo "$SORTED" | awk -v p="$p" -v n="$WROTE" '
    BEGIN { i = int((p/100) * n + 0.5); if (i < 1) i = 1 }
    NR == i { print; exit }'
}
P50=$(pct 50)
P99=$(pct 99)

printf 'wrote=%d  duration=%ss  rate=%s/s  P50=%sms  P99=%sms\n' \
  "$WROTE" "$DUR_S" "$RATE" "$P50" "$P99"
