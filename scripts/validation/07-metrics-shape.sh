#!/usr/bin/env -S pkgx bash
#
# 07-metrics-shape.sh — scrape /metrics from each host and assert the
# metric families the Grafana dashboard references are present (see
# docs/operations/grafana/README.md, "Panel references"). A missing
# family is a silent dashboard-blank — surface it loudly here.
#
# Usage:
#   HOSTS=10.0.0.1,10.0.0.2,10.0.0.3 scripts/validation/07-metrics-shape.sh
#
# Optional env :
#   METRICS_PORT  default 9101
#
# Required families (per docs/operations/grafana/README.md):
#   grpc_server_started_total
#   grpc_server_handled_total
#   grpc_server_handling_seconds_bucket
#   grpc_server_msg_received_total
#   grpc_server_msg_sent_total
#   process_cpu_seconds_total
#   process_resident_memory_bytes
#   process_open_fds
#
# Exit 0 iff every host serves every family.

set -euo pipefail

: "${HOSTS:?HOSTS env var required (comma-separated IPs)}"
METRICS_PORT="${METRICS_PORT:-9101}"

# Keep in sync with docs/operations/grafana/README.md "Panel references".
REQUIRED_FAMILIES=(
  grpc_server_started_total
  grpc_server_handled_total
  grpc_server_handling_seconds_bucket
  grpc_server_msg_received_total
  grpc_server_msg_sent_total
  process_cpu_seconds_total
  process_resident_memory_bytes
  process_open_fds
)

fail=0

check_host() {
  local host="$1"
  local body
  if ! body=$(curl -sS --fail --max-time 10 "http://${host}:${METRICS_PORT}/metrics"); then
    echo "[host=${host}] FAIL: /metrics unreachable" >&2
    fail=$((fail + 1))
    return
  fi

  local missing=()
  local fam
  for fam in "${REQUIRED_FAMILIES[@]}"; do
    # Prometheus format puts the metric name at column 1 (or after `# HELP`
    # / `# TYPE`). Anchor with ^ so we don't accept the name appearing
    # only inside a label value.
    if ! grep -qE "^(# (HELP|TYPE) )?${fam}([{ ]|\$)" <<< "$body"; then
      missing+=("$fam")
    fi
  done

  if (( ${#missing[@]} > 0 )); then
    echo "[host=${host}] FAIL: missing metric families: ${missing[*]}" >&2
    fail=$((fail + 1))
    return
  fi
  echo "[host=${host}] ok (${#REQUIRED_FAMILIES[@]} families present)"
}

IFS=',' read -ra _hosts <<< "$HOSTS"
for h in "${_hosts[@]}"; do
  check_host "$h"
done

if (( fail > 0 )); then
  echo "${fail} host(s) had a metrics shape miss" >&2
  exit 1
fi
echo "metrics shape ok on all ${#_hosts[@]} host(s)"
