#!/usr/bin/env -S pkgx bash
#
# 04-vm-roundtrip.sh — create / start / stop / delete a canary VM end
# to end. The image defaults to a small public Debian cloud image ;
# operators with an air-gapped catalogue should override IMAGE.
#
# Usage:
#   WEFT_HOST=10.0.0.1 scripts/validation/04-vm-roundtrip.sh
#
# Optional env :
#   NAME        default validation-canary-<unix-ts>
#   IMAGE       default ghcr.io/openweft/debian-12-cloud:latest
#   CPU         default 1
#   MEMORY_MIB  default 512
#   TIMEOUT     default 120  (seconds to wait for Running)
#   WEFT        default `weft` ; export to point at a specific binary
#
# Exit 0 iff every verb (start, status==Running, stop, rm) returns 0
# within timeout. On failure, the canary is left in place for triage
# unless KEEP_ON_FAILURE=0, in which case we attempt cleanup.

set -euo pipefail

NAME="${NAME:-validation-canary-$(date +%s)}"
IMAGE="${IMAGE:-ghcr.io/openweft/debian-12-cloud:latest}"
CPU="${CPU:-1}"
MEMORY_MIB="${MEMORY_MIB:-512}"
TIMEOUT="${TIMEOUT:-120}"
WEFT="${WEFT:-weft}"

command -v "$WEFT" >/dev/null 2>&1 \
  || { echo "weft CLI not on PATH (set WEFT=/path/to/weft)" >&2; exit 1; }

cleanup() {
  local rc=$?
  if (( rc != 0 )) && [[ "${KEEP_ON_FAILURE:-1}" == "1" ]]; then
    echo "FAIL — leaving canary '${NAME}' in place for triage (set KEEP_ON_FAILURE=0 to force cleanup)" >&2
    return
  fi
  # Best-effort teardown — never fail the script in trap.
  "$WEFT" instance stop --name "$NAME" >/dev/null 2>&1 || true
  "$WEFT" instance rm   --name "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "[1/4] starting canary ${NAME} from ${IMAGE} (${CPU} CPU / ${MEMORY_MIB} MiB)"
"$WEFT" instance start \
  --name "$NAME" \
  --image "$IMAGE" \
  --cpu "$CPU" \
  --memory "$MEMORY_MIB"

echo "[2/4] waiting up to ${TIMEOUT}s for state=Running"
deadline=$(( $(date +%s) + TIMEOUT ))
while :; do
  state=$("$WEFT" instance status --name "$NAME" 2>/dev/null \
            | awk -F'[: \t]+' '/^[Ss]tate/ {print $2; exit}' \
            || true)
  if [[ "$state" == "Running" || "$state" == "running" ]]; then
    echo "      reached Running"
    break
  fi
  if (( $(date +%s) >= deadline )); then
    echo "FAIL: canary did not reach Running within ${TIMEOUT}s (last state: '${state:-unknown}')" >&2
    exit 1
  fi
  sleep 2
done

echo "[3/4] stopping canary"
"$WEFT" instance stop --name "$NAME"

echo "[4/4] removing canary"
"$WEFT" instance rm --name "$NAME"
trap - EXIT  # success: no triage hold-back needed

echo "VM roundtrip ok: ${NAME} start→Running→stop→rm completed"
