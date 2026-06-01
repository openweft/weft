// keys.go : helpers around the snapshot-key shape the volume
// snapshot uploader uses :
//
//	<project_uuid>/<volume_uuid>/<snapshot_uuid>.qcow2
//
// The Backend interface (backup.go) treats keys as opaque ; this
// file owns the convention so the cmd/weft volume CLI + any
// future restore tooling share a single source of truth.
//
// Why a parser at all : the off-host restore path takes a raw
// backend-key from the operator (`weft volume snapshot restore
// --from-backup <key>`) and needs to (a) sanity-check the shape
// before talking to the backend, (b) recover the original
// snapshot UUID so the restored volume can be tagged with its
// provenance. Both jobs want the parsed triple, not a substring
// match.
//
// Convention shape : we use three '/'-separated UUID-like
// segments, terminated by ".qcow2". We don't validate that the
// segments are RFC-4122 UUIDs — the registry on the agent side
// uses opaque strings too (weft.VolumeSnapshot.UUID), and forcing
// canonical UUIDs here would needlessly break re-imports of
// snapshots captured before this convention.

package backup

import (
	"fmt"
	"path"
	"strings"
)

// SnapshotKeyExt is the suffix the uploader appends to a snapshot
// blob. Picked to be format-evocative even though the agent's on-
// disk shape is a raw reflink (.bin) — at upload time we know the
// blob is restorable as qcow2 and the operator wants the
// extension to hint at that.
const SnapshotKeyExt = ".qcow2"

// SnapshotKey returns the canonical key shape for an
// off-host-archived snapshot. It is the inverse of ParseSnapshotKey
// (round-tripping is exercised in keys_test.go).
//
// Empty inputs return "" rather than building a malformed key —
// callers that want strict validation should ParseSnapshotKey on
// the result before passing it to Backend.Upload.
func SnapshotKey(projectUUID, volumeUUID, snapshotUUID string) string {
	if projectUUID == "" || volumeUUID == "" || snapshotUUID == "" {
		return ""
	}
	return path.Join(projectUUID, volumeUUID, snapshotUUID) + SnapshotKeyExt
}

// SnapshotKeyParts is the parsed triple from a backup key. Names
// match the proto VolumeSnapshotInfo fields so callers can fan-out
// into a registry row without renaming.
type SnapshotKeyParts struct {
	ProjectUUID  string
	VolumeUUID   string
	SnapshotUUID string
}

// ParseSnapshotKey splits a backup key into its three UUID
// segments. Returns an error if the shape does not match the
// "<p>/<v>/<s>.qcow2" convention. Strict on the .qcow2 suffix so
// a stray prefix listing (where prefixes are themselves "keys" in
// a few S3-compatibles' minds) bubbles up loudly rather than
// silently parsing into an empty SnapshotUUID.
func ParseSnapshotKey(key string) (SnapshotKeyParts, error) {
	if !strings.HasSuffix(key, SnapshotKeyExt) {
		return SnapshotKeyParts{}, fmt.Errorf("backup: key %q lacks %q suffix", key, SnapshotKeyExt)
	}
	trimmed := strings.TrimSuffix(key, SnapshotKeyExt)
	parts := strings.Split(trimmed, "/")
	if len(parts) != 3 {
		return SnapshotKeyParts{}, fmt.Errorf("backup: key %q is not <project>/<volume>/<snapshot>.qcow2 (got %d segments)", key, len(parts))
	}
	for i, p := range parts {
		if p == "" {
			return SnapshotKeyParts{}, fmt.Errorf("backup: key %q has empty segment %d", key, i)
		}
	}
	return SnapshotKeyParts{
		ProjectUUID:  parts[0],
		VolumeUUID:   parts[1],
		SnapshotUUID: parts[2],
	}, nil
}
