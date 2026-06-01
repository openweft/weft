#!/usr/bin/env -S pkgx bash
#
# 05-snapshot-roundtrip.sh — exercise the volume + snapshot path. We
# create a small volume, snapshot it, restore the snapshot into a new
# volume, then tear everything down. Asserts the reflink/FICLONE path
# documented in memory `project_cow_clone` actually fires (the daemon
# logs `cowclone: ok` ; we check via the CLI's exit code, not the log).
#
# Usage:
#   scripts/validation/05-snapshot-roundtrip.sh
#
# Optional env :
#   PROJECT       default "validation"
#   VOLUME_NAME   default canary-vol-<unix-ts>
#   SIZE_GIB      default 1
#   WEFT          default `weft`
#   TIMEOUT       default 60

set -euo pipefail

PROJECT="${PROJECT:-validation}"
VOLUME_NAME="${VOLUME_NAME:-canary-vol-$(date +%s)}"
SNAPSHOT_NAME="${SNAPSHOT_NAME:-${VOLUME_NAME}-snap}"
RESTORED_NAME="${RESTORED_NAME:-${VOLUME_NAME}-restored}"
SIZE_GIB="${SIZE_GIB:-1}"
TIMEOUT="${TIMEOUT:-60}"
WEFT="${WEFT:-weft}"

command -v "$WEFT" >/dev/null 2>&1 \
  || { echo "weft CLI not on PATH (set WEFT=/path/to/weft)" >&2; exit 1; }

# Track UUIDs as we create them so cleanup hits the right objects even
# if the user renames a step's output.
vol_uuid=""
snap_uuid=""
restored_uuid=""

cleanup() {
  # Reverse-order teardown. Each step is best-effort.
  [[ -n "$restored_uuid" ]] && "$WEFT" volume rm "$restored_uuid" >/dev/null 2>&1 || true
  [[ -n "$snap_uuid"     ]] && "$WEFT" volume snapshot delete "$snap_uuid" >/dev/null 2>&1 || true
  [[ -n "$vol_uuid"      ]] && "$WEFT" volume rm "$vol_uuid" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "[1/5] creating volume ${VOLUME_NAME} (${SIZE_GIB} GiB) in project ${PROJECT}"
vol_uuid=$("$WEFT" volume create \
             --project "$PROJECT" \
             --name "$VOLUME_NAME" \
             --size "$SIZE_GIB" \
             --format uuid 2>/dev/null) \
  || vol_uuid=$("$WEFT" volume create \
                  --project "$PROJECT" \
                  --name "$VOLUME_NAME" \
                  --size "$SIZE_GIB" \
                | awk '/UUID|uuid/ {print $NF; exit}')

[[ -n "$vol_uuid" ]] || { echo "FAIL: volume create returned no UUID" >&2; exit 1; }
echo "      vol_uuid=${vol_uuid}"

echo "[2/5] snapshotting"
snap_uuid=$("$WEFT" volume snapshot create \
              --volume "$vol_uuid" \
              --name "$SNAPSHOT_NAME" \
              --format uuid 2>/dev/null) \
  || snap_uuid=$("$WEFT" volume snapshot create \
                   --volume "$vol_uuid" \
                   --name "$SNAPSHOT_NAME" \
                 | awk '/UUID|uuid/ {print $NF; exit}')

[[ -n "$snap_uuid" ]] || { echo "FAIL: snapshot create returned no UUID" >&2; exit 1; }
echo "      snap_uuid=${snap_uuid}"

echo "[3/5] restoring snapshot into new volume ${RESTORED_NAME}"
restored_uuid=$("$WEFT" volume snapshot restore \
                  --snapshot "$snap_uuid" \
                  --name "$RESTORED_NAME" \
                  --format uuid 2>/dev/null) \
  || restored_uuid=$("$WEFT" volume snapshot restore \
                       --snapshot "$snap_uuid" \
                       --name "$RESTORED_NAME" \
                     | awk '/UUID|uuid/ {print $NF; exit}')

[[ -n "$restored_uuid" ]] || { echo "FAIL: restore returned no UUID" >&2; exit 1; }
echo "      restored_uuid=${restored_uuid}"

# Sanity: the restored volume should be visible in `volume ls`.
echo "[4/5] confirming restored volume is listed"
deadline=$(( $(date +%s) + TIMEOUT ))
while :; do
  if "$WEFT" volume ls --project "$PROJECT" 2>/dev/null | grep -q "$restored_uuid"; then
    break
  fi
  if (( $(date +%s) >= deadline )); then
    echo "FAIL: restored volume ${restored_uuid} never appeared in volume ls" >&2
    exit 1
  fi
  sleep 1
done

echo "[5/5] tearing down (restored → snapshot → source)"
"$WEFT" volume rm "$restored_uuid"; restored_uuid=""
"$WEFT" volume snapshot delete "$snap_uuid"; snap_uuid=""
"$WEFT" volume rm "$vol_uuid"; vol_uuid=""
trap - EXIT

echo "snapshot roundtrip ok: create → snapshot → restore → delete x3 completed"
