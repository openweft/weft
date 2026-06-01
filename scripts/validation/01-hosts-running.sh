#!/usr/bin/env -S pkgx bash
#
# 01-hosts-running.sh — assert every cluster host answers SSH/ICMP, has
# weft-agent.service active, and exposes /metrics on the documented port.
#
# Usage:
#   HOSTS=10.0.0.1,10.0.0.2,10.0.0.3 scripts/validation/01-hosts-running.sh
#
# Optional env :
#   METRICS_PORT   default 9101  (see docs/operations/observability.md)
#   SSH_USER       default root
#   SSH_OPTS       passed verbatim to ssh ; defaults to "-o BatchMode=yes
#                  -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new"
#
# Exit 0 iff every host passes every check. Per-host failures stream to
# stderr with a "[host=X.X.X.X check=Y] reason" prefix so a downstream
# log shipper can pick them up.

set -euo pipefail

: "${HOSTS:?HOSTS env var required (comma-separated IPs or hostnames)}"
METRICS_PORT="${METRICS_PORT:-9101}"
SSH_USER="${SSH_USER:-root}"
SSH_OPTS="${SSH_OPTS:--o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new}"

fail=0
note_fail() {
  local host="$1" check="$2" reason="$3"
  echo "[host=${host} check=${check}] ${reason}" >&2
  fail=$((fail + 1))
}

check_host() {
  local host="$1"

  # 1. Reachability — a single ICMP echo is cheap and survives most
  #    firewall policies that allow SSH.
  if ! ping -c1 -W2 "$host" >/dev/null 2>&1; then
    note_fail "$host" reachable "ICMP echo request failed"
    return
  fi

  # 2. weft-agent.service active — surfaced via systemctl over SSH. We
  #    accept either "active" or "running" because some distros report
  #    "running" for the sub-state instead.
  local svc_state
  if ! svc_state=$(ssh $SSH_OPTS "${SSH_USER}@${host}" \
        "systemctl is-active weft-agent.service" 2>/dev/null); then
    note_fail "$host" service "ssh+systemctl failed (auth, unit missing, host down)"
    return
  fi
  if [[ "$svc_state" != "active" ]]; then
    note_fail "$host" service "weft-agent.service is ${svc_state}, want active"
    return
  fi

  # 3. /metrics returns 200 — we don't parse the body here (07-metrics-shape.sh
  #    does that). This is a liveness probe only.
  local code
  code=$(curl -sS -o /dev/null -w '%{http_code}' \
          --max-time 5 "http://${host}:${METRICS_PORT}/metrics" || echo "000")
  if [[ "$code" != "200" ]]; then
    note_fail "$host" metrics "GET /metrics returned HTTP ${code}, want 200"
    return
  fi

  echo "[host=${host}] ok"
}

IFS=',' read -ra _hosts <<< "$HOSTS"
for h in "${_hosts[@]}"; do
  check_host "$h"
done

if (( fail > 0 )); then
  echo "${fail} host check(s) failed" >&2
  exit 1
fi
echo "all ${#_hosts[@]} host(s) green"
