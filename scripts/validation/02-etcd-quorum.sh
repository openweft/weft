#!/usr/bin/env -S pkgx bash
#
# 02-etcd-quorum.sh — assert every etcd member in the cluster is healthy
# and the quorum holds. Wraps `etcdctl endpoint health --cluster` ; we
# also check `endpoint status` afterward to make sure leader election is
# stable (no two members claiming leadership).
#
# Usage:
#   ETCD_ENDPOINTS=10.0.0.1:2379,10.0.0.2:2379,10.0.0.3:2379 \
#     scripts/validation/02-etcd-quorum.sh
#
# Optional env :
#   ETCDCTL              path to etcdctl ; default: `pkgx etcdctl`
#                        if not on PATH (see feedback_prefer_pkgx).
#   ETCD_CACERT          mTLS root CA bundle
#   ETCD_CERT, ETCD_KEY  mTLS client material
#
# Exit 0 iff every endpoint reports healthy AND exactly one leader.

set -euo pipefail

: "${ETCD_ENDPOINTS:?ETCD_ENDPOINTS env var required (comma-separated host:port)}"

if [[ -n "${ETCDCTL:-}" ]]; then
  :
elif command -v etcdctl >/dev/null 2>&1; then
  ETCDCTL="etcdctl"
elif command -v pkgx >/dev/null 2>&1; then
  ETCDCTL="pkgx etcdctl"
else
  echo "etcdctl not on PATH and pkgx not available ; install pkgx or etcdctl" >&2
  exit 1
fi

export ETCDCTL_API=3
export ETCDCTL_ENDPOINTS="$ETCD_ENDPOINTS"
[[ -n "${ETCD_CACERT:-}" ]] && export ETCDCTL_CACERT="$ETCD_CACERT"
[[ -n "${ETCD_CERT:-}"   ]] && export ETCDCTL_CERT="$ETCD_CERT"
[[ -n "${ETCD_KEY:-}"    ]] && export ETCDCTL_KEY="$ETCD_KEY"

n_endpoints=$(awk -F, '{print NF}' <<< "$ETCD_ENDPOINTS")
echo "checking ${n_endpoints} endpoint(s): ${ETCD_ENDPOINTS}"

# 1. endpoint health --cluster — talks to every member, returns one
#    line per member. Any "unhealthy" or non-zero exit = fail.
if ! health_out=$($ETCDCTL endpoint health --cluster -w json 2>&1); then
  echo "etcdctl endpoint health failed:" >&2
  echo "$health_out" >&2
  exit 1
fi

# Parse with jq when present, fall back to grep otherwise. jq is the
# right answer (etcdctl JSON is well-formed) but the script must keep
# working on operator laptops without jq.
if command -v jq >/dev/null 2>&1; then
  unhealthy=$(jq -r '.[] | select(.health != true) | .endpoint' <<< "$health_out")
else
  unhealthy=$(grep -E '"health"[[:space:]]*:[[:space:]]*false' <<< "$health_out" || true)
fi

if [[ -n "$unhealthy" ]]; then
  echo "unhealthy etcd member(s):" >&2
  echo "$unhealthy" >&2
  exit 1
fi

# 2. endpoint status — confirm exactly one leader. A split-brain returns
#    >1 ; an election in progress returns 0.
if ! status_out=$($ETCDCTL endpoint status --cluster -w json 2>&1); then
  echo "etcdctl endpoint status failed:" >&2
  echo "$status_out" >&2
  exit 1
fi

if command -v jq >/dev/null 2>&1; then
  # member.leader == member.ID iff this endpoint sees itself as the leader.
  leaders=$(jq -r '[.[] | select(.Status.leader == .Status.header.member_id)] | length' <<< "$status_out")
else
  leaders=$(grep -cE '"leader"[[:space:]]*:[[:space:]]*[1-9]' <<< "$status_out" || true)
fi

if [[ "$leaders" != "1" ]]; then
  echo "expected exactly 1 etcd leader, found ${leaders}" >&2
  echo "$status_out" >&2
  exit 1
fi

echo "etcd quorum healthy: ${n_endpoints} endpoint(s), 1 leader"
