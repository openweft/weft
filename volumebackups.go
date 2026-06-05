package weft

// volumebackups.go : the Adapter-side surface for off-host backups of
// block-backend volumes. File-backend volumes have no backup story yet
// (the reflink CoW is host-local) ; every method here returns a clear
// "block-only" error when the parent volume isn't block-backed.
//
// Targets are the same Target URL grammar weft-block speaks :
//   - "oci://<registry>/<repo>:<tag>"     ← default for openweft (content-addressed, mirror-friendly)
//   - "s3://<bucket>@<region>/<prefix>"   ← versitygw / CubeFS objectnode
//   - "sftp://<user>@<host>:<port>/<path>"← sftpgo / OpenSSH sshd
//   - "fs://<absolute_path>"              ← dev / tests only
//
// Encryption / incremental-chain bookkeeping lives inside weft-block —
// the Adapter only passes through (snapshotUUID, target) and trusts the
// driver to honour the encryption env vars + parent URL it reads from
// the sibling metadata. See pkg/backupcrypto + pkg/backuptarget in
// weft-block for the on-the-wire format.

import (
	"context"
	"fmt"

	drivers "github.com/openweft/weft-drivers"
)

// CreateVolumeBackup ships the snapshot at snapshotUUID to target,
// returning the driver-issued Backup descriptor. The snapshot must
// already exist (taken via RegisterVolumeSnapshot) and live on a
// block-backend parent volume.
func (a *Adapter) CreateVolumeBackup(ctx context.Context, snapshotUUID, target string) (drivers.Backup, error) {
	if snapshotUUID == "" {
		return drivers.Backup{}, fmt.Errorf("empty snapshot uuid")
	}
	if target == "" {
		return drivers.Backup{}, fmt.Errorf("empty target url")
	}
	snap, ok := a.VolumeSnapshotByUUID(snapshotUUID)
	if !ok {
		return drivers.Backup{}, fmt.Errorf("snapshot %q not found", snapshotUUID)
	}
	parent, ok := a.VolumeByUUID(snap.VolumeUUID)
	if !ok {
		return drivers.Backup{}, fmt.Errorf("parent volume %q not found", snap.VolumeUUID)
	}
	if parent.Backend != VolumeBackendBlock {
		return drivers.Backup{}, fmt.Errorf("backup is only supported on block-backend volumes (volume %s is %q)", parent.UUID, parent.Backend)
	}
	vd, err := a.blockVolumeDriver()
	if err != nil {
		return drivers.Backup{}, err
	}
	bk, err := vd.CreateBackup(ctx, drivers.BackupSpec{
		VolumeUUID:   parent.UUID,
		SnapshotName: snapshotDriverName(snap),
		Target:       target,
		Labels: map[string]string{
			"weft.project_uuid":  parent.ProjectUUID,
			"weft.volume_name":   parent.Name,
			"weft.snapshot_uuid": snap.UUID,
			"weft.snapshot_name": snap.Name,
		},
	})
	if err != nil {
		return drivers.Backup{}, fmt.Errorf("weft-block CreateBackup %s/%s → %s: %w", parent.UUID, snap.Name, target, err)
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "volume_backup.created",
		Subject:     bk.URL,
		ProjectUUID: parent.ProjectUUID,
		Meta: map[string]string{
			"volume_uuid":   parent.UUID,
			"snapshot_uuid": snap.UUID,
			"target":        target,
			"size_bytes":    fmt.Sprintf("%d", bk.SizeBytes),
		},
	})
	return bk, nil
}

// ListVolumeBackups enumerates backups stored at target. Empty
// volumeUUID lists every backup at the target ; non-empty filters
// to that one volume. Backups are driver-keyed, not registry-keyed
// — there's no weft-side backup row, the source of truth is the
// target store itself.
func (a *Adapter) ListVolumeBackups(ctx context.Context, target, volumeUUID string) ([]drivers.Backup, error) {
	if target == "" {
		return nil, fmt.Errorf("empty target url")
	}
	vd, err := a.blockVolumeDriver()
	if err != nil {
		return nil, err
	}
	return vd.ListBackups(ctx, target, volumeUUID)
}

// DeleteVolumeBackup removes one backup. The URL is the full
// addressing key (as returned in drivers.Backup.URL). Idempotent —
// deleting a missing backup is a no-op driver-side.
func (a *Adapter) DeleteVolumeBackup(ctx context.Context, backupURL string) error {
	if backupURL == "" {
		return fmt.Errorf("empty backup url")
	}
	vd, err := a.blockVolumeDriver()
	if err != nil {
		return err
	}
	if err := vd.DeleteBackup(ctx, backupURL); err != nil {
		return fmt.Errorf("weft-block DeleteBackup %s: %w", backupURL, err)
	}
	a.bus.Publish(PlatformEvent{
		Kind:    "volume_backup.deleted",
		Subject: backupURL,
	})
	return nil
}

// RestoreVolumeBackup creates a NEW Volume row + materialises it
// from the backup at backupURL. The new volume lives in
// projectUUID (which must be one the caller can write to — the
// gRPC layer's AuthorizeProject is the gate). Size is discovered
// from the backup's on-target metadata via ListBackups+filter,
// so the caller doesn't have to know it upfront.
//
// Ordering : weft Volume row first (so a half-failed restore
// surfaces as an empty volume the operator can retry), then
// driver-side EnsureVolume + RestoreBackup. On driver failure we
// roll the row back.
func (a *Adapter) RestoreVolumeBackup(ctx context.Context, backupURL, newVolumeName, projectUUID string) (Volume, error) {
	if backupURL == "" {
		return Volume{}, fmt.Errorf("empty backup url")
	}
	if newVolumeName == "" {
		return Volume{}, fmt.Errorf("empty new_volume_name")
	}
	if projectUUID == "" {
		return Volume{}, fmt.Errorf("empty project uuid")
	}
	vd, err := a.blockVolumeDriver()
	if err != nil {
		return Volume{}, err
	}
	// Discover size + original volume UUID. We don't know the
	// target prefix from backupURL alone, so pass the URL itself
	// as the "target" — weft-block's ListBackups walks the prefix
	// and we filter to the exact URL match.
	allEntries, err := vd.ListBackups(ctx, backupURL, "")
	if err != nil {
		return Volume{}, fmt.Errorf("inspect backup %s: %w", backupURL, err)
	}
	var origin drivers.Backup
	var found bool
	for _, e := range allEntries {
		if e.URL == backupURL {
			origin = e
			found = true
			break
		}
	}
	if !found {
		return Volume{}, fmt.Errorf("backup %s not found on its target", backupURL)
	}
	sizeGiB := bytesToGiBCeil(origin.SizeBytes)
	if sizeGiB <= 0 {
		return Volume{}, fmt.Errorf("backup %s has unknown size — refusing to restore into a sizeless volume", backupURL)
	}
	v, err := a.volumeReg.create(CreateVolumeSpec{
		ProjectUUID: projectUUID,
		Name:        newVolumeName,
		SizeGiB:     sizeGiB,
		Format:      VolumeFormatRaw,
		Backend:     VolumeBackendBlock,
	})
	if err != nil {
		return Volume{}, fmt.Errorf("register restored volume: %w", err)
	}
	spec := drivers.VolumeSpec{
		UUID:        v.UUID,
		ProjectUUID: v.ProjectUUID,
		Name:        v.Name,
		SizeGiB:     v.SizeGiB,
	}
	if err := vd.EnsureVolume(ctx, spec); err != nil {
		if delErr := a.volumeReg.delete(v.UUID); delErr != nil {
			return Volume{}, fmt.Errorf("ensure restored volume: %w (rollback failed: %v)", err, delErr)
		}
		return Volume{}, fmt.Errorf("ensure restored volume: %w", err)
	}
	if err := vd.RestoreBackup(ctx, backupURL, spec); err != nil {
		// EnsureVolume already created backing storage ;
		// DestroyVolume cleans it up so the operator can retry.
		_ = vd.DestroyVolume(ctx, v.UUID)
		if delErr := a.volumeReg.delete(v.UUID); delErr != nil {
			return Volume{}, fmt.Errorf("restore backup: %w (rollback failed: %v)", err, delErr)
		}
		return Volume{}, fmt.Errorf("restore backup %s: %w", backupURL, err)
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "volume_backup.restored",
		Subject:     v.UUID,
		ProjectUUID: v.ProjectUUID,
		Meta: map[string]string{
			"name":         v.Name,
			"backup_url":   backupURL,
			"size_gib":     fmt.Sprintf("%d", v.SizeGiB),
			"size_bytes":   fmt.Sprintf("%d", origin.SizeBytes),
			"origin_uuid":  origin.VolumeUUID,
			"origin_state": origin.State,
		},
	})
	return v, nil
}

// bytesToGiBCeil rounds bytes up to whole GiB. A backup that's
// 5.1 GiB plaintext needs a 6 GiB volume to land in ; truncating
// down would corrupt the restore.
func bytesToGiBCeil(b int64) int {
	const gib = int64(1) << 30
	if b <= 0 {
		return 0
	}
	return int((b + gib - 1) / gib)
}
