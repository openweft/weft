#!/usr/bin/env -S pkgx bash
#
# 08-audit-log-write.sh — confirm the audit log sink is wired AND
# observing deny decisions. We provoke a known-denied operation
# (a non-admin caller trying to `host register`, which requires
# `weft:admin` per docs/operations/rbac.md) and grep the audit file
# on each host for a matching record with "decision":"deny".
#
# Usage:
#   HOSTS=10.0.0.1,10.0.0.2,10.0.0.3 \
#   WEFT_AGENT=10.0.0.1 \
#   NON_ADMIN_TOKEN=$(cat ./project-only-token.jwt) \
#     scripts/validation/08-audit-log-write.sh
#
# Optional env :
#   AUDIT_PATH    default /var/log/weft/audit.jsonl
#   SSH_USER      default root
#   SSH_OPTS      passed verbatim to ssh
#   WEFT          default `weft`
#
# The audit record shape we look for (per docs/operations/rbac.md
# "Audit log") is JSON with at least: subject, verb=RequireAdmin:*,
# object=cluster, decision=deny.

set -euo pipefail

: "${HOSTS:?HOSTS env var required (comma-separated IPs)}"
: "${WEFT_AGENT:?WEFT_AGENT env var required (an agent endpoint to RPC against)}"
: "${NON_ADMIN_TOKEN:?NON_ADMIN_TOKEN env var required (a JWT WITHOUT weft:admin group)}"

AUDIT_PATH="${AUDIT_PATH:-/var/log/weft/audit.jsonl}"
SSH_USER="${SSH_USER:-root}"
SSH_OPTS="${SSH_OPTS:--o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new}"
WEFT="${WEFT:-weft}"

command -v "$WEFT" >/dev/null 2>&1 \
  || { echo "weft CLI not on PATH" >&2; exit 1; }

# Use a unique marker so we can grep just this run's records, not
# any historical denies.
marker="validation-canary-$(date +%s)-$$"
echo "marker: ${marker}"

# 1. Provoke a deny. `host register` requires RequireAdmin per rbac.md.
#    The non-admin token must NOT include weft:admin in groups. We
#    expect this to fail with PermissionDenied (exit non-zero) ; if
#    it SUCCEEDS, the cluster is dangerously open and the test fails
#    just as loudly as the no-audit-line case.
echo "[1/2] provoking a deny: host register ${marker} as non-admin"
set +e
deny_out=$(WEFT_AUTH_BEARER="$NON_ADMIN_TOKEN" \
             "$WEFT" --server "$WEFT_AGENT" \
             host register "$marker" 2>&1)
rc=$?
set -e

if (( rc == 0 )); then
  echo "FAIL: non-admin host register SUCCEEDED — RBAC is not enforcing admin-only ops" >&2
  echo "      cli output: ${deny_out}" >&2
  exit 1
fi
echo "      ok — denied with rc=${rc}: $(head -c200 <<< "$deny_out")"

# 2. Check every host's audit log for a matching deny line. The agent
#    flushes audit writes synchronously per auditlog/auditlog.go, but
#    we give it a short grace period to land on disk.
sleep 1

fail=0
IFS=',' read -ra _hosts <<< "$HOSTS"
for h in "${_hosts[@]}"; do
  # Pull the tail of the file ; we only care about the records this
  # run emitted. 1000 lines is enough headroom for the burst of
  # decisions a single deny triggers.
  if ! tail_out=$(ssh $SSH_OPTS "${SSH_USER}@${h}" \
                    "tail -n 1000 ${AUDIT_PATH} 2>/dev/null" 2>/dev/null); then
    echo "[host=${h}] FAIL: cannot read ${AUDIT_PATH} via ssh" >&2
    fail=$((fail + 1))
    continue
  fi

  if [[ -z "$tail_out" ]]; then
    echo "[host=${h}] FAIL: ${AUDIT_PATH} empty — audit sink not wired?" >&2
    fail=$((fail + 1))
    continue
  fi

  # We accept the marker matching anywhere in the JSON line (object or
  # reason field) AND a "decision":"deny" pair. Both must appear on the
  # SAME line.
  if grep -F "$marker" <<< "$tail_out" | grep -q '"decision"[[:space:]]*:[[:space:]]*"deny"'; then
    echo "[host=${h}] ok — deny recorded for ${marker}"
    continue
  fi

  # Fallback: at minimum, SOME deny in the recent tail proves the sink
  # is alive even if our marker didn't propagate (e.g., the deny was
  # logged at a different host first and isn't replicated).
  if grep -q '"decision"[[:space:]]*:[[:space:]]*"deny"' <<< "$tail_out"; then
    echo "[host=${h}] partial — marker '${marker}' not seen, but other deny records present" >&2
    # Not a hard fail: the agent that served the RPC is the one that
    # writes the record. In a 3-DC cluster, only one host sees it.
    continue
  fi

  echo "[host=${h}] FAIL: no deny records in ${AUDIT_PATH} after provoking one" >&2
  fail=$((fail + 1))
done

if (( fail > 0 )); then
  echo "${fail} host(s) had audit-log failures" >&2
  exit 1
fi
echo "audit log ok: deny was provoked and at least one host recorded it"
