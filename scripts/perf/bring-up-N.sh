#!/usr/bin/env -S pkgx bash
#
# bring-up-N.sh — bring up N microVMs against a running weft cluster
# and record per-phase wall-clock timings into a CSV.
#
# Usage:
#   scripts/perf/bring-up-N.sh <N> [--image <ref>] [--parallel <P>] [--out <csv>]
#
# Phases recorded per VM:
#   register : `weft microvm register` returns (state persisted in etcd).
#   start    : `weft microvm start` returns (driver plugin acknowledged).
#   running  : `weft microvm wait --state=running` returns (guest agent up).
#
# Output CSV columns:
#   vm_id,register_ms,start_ms,running_ms,total_ms
#
# Aim for batches of 10, 100, 1000. Above 1000 you are usually bottlenecked
# on etcd write rate or kernel image pull bandwidth — run `etcd-write-rate.sh`
# first and confirm the host has the kernel image cached locally.
#
# This script is a **measurement tool**, not a CI gate. Numbers are inherently
# noisy ; reading them against a baseline is an operator judgement call.

set -euo pipefail

N="${1:-}"
if [[ -z "$N" || ! "$N" =~ ^[0-9]+$ ]]; then
  echo "usage: $0 <N> [--image <ref>] [--parallel <P>] [--out <csv>]" >&2
  exit 2
fi
shift

IMAGE="weft-test-fixture:latest"
PARALLEL=16
OUT="perf-bringup-${N}-$(date +%Y%m%d-%H%M%S).csv"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --image)    IMAGE="$2"; shift 2 ;;
    --parallel) PARALLEL="$2"; shift 2 ;;
    --out)      OUT="$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

WEFT="${WEFT:-weft}"
command -v "$WEFT" >/dev/null || { echo "weft CLI not on PATH"; exit 1; }

echo "vm_id,register_ms,start_ms,running_ms,total_ms" > "$OUT"
PREFIX="perf-$(date +%s)"

bring_up_one() {
  local idx="$1"
  local vm="${PREFIX}-${idx}"
  local t0 t1 t2 t3
  t0=$(date +%s%3N)
  "$WEFT" microvm register --name "$vm" --image "$IMAGE" >/dev/null
  t1=$(date +%s%3N)
  "$WEFT" microvm start "$vm" >/dev/null
  t2=$(date +%s%3N)
  "$WEFT" microvm wait "$vm" --state=running --timeout=120s >/dev/null
  t3=$(date +%s%3N)
  printf '%s,%d,%d,%d,%d\n' \
    "$vm" $((t1 - t0)) $((t2 - t1)) $((t3 - t2)) $((t3 - t0)) >> "$OUT"
}
export -f bring_up_one
export WEFT IMAGE OUT PREFIX

WALL_START=$(date +%s%3N)
seq 1 "$N" | xargs -n1 -P "$PARALLEL" -I{} bash -c 'bring_up_one "$@"' _ {}
WALL_END=$(date +%s%3N)

WALL_MS=$((WALL_END - WALL_START))
echo
echo "N=$N parallel=$PARALLEL wall-clock=${WALL_MS}ms csv=$OUT"
echo
echo "Per-phase summary (ms):"
awk -F, 'NR>1 {
  for (i=2;i<=5;i++) { s[i]+=$i; if ($i>m[i]) m[i]=$i }
  n++
}
END {
  printf "  register  avg=%d  max=%d\n", s[2]/n, m[2]
  printf "  start     avg=%d  max=%d\n", s[3]/n, m[3]
  printf "  running   avg=%d  max=%d\n", s[4]/n, m[4]
  printf "  total     avg=%d  max=%d\n", s[5]/n, m[5]
}' "$OUT"
