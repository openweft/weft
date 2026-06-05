package weft

// volumesnapshots_block.go : dispatch helpers for block-backend volume
// snapshots. The file backend (default) keeps its existing reflink path
// in volumesnapshots.go ; block-backend volumes round-trip through the
// `weft-block` driver via the standard drivers.VolumeDriver surface.
//
// Lookup strategy : block-backend volumes are cluster-wide (Longhorn-
// style controller + replica chain backed by etcd), so any host that
// runs the weft-block driver can service any block volume. We walk
// the driver-dispatch table, return the first VolumeDriver whose
// Name() == "block", and call its snapshot RPCs. No host pinning by
// volume UUID — the driver itself routes by UUID against the shared
// state store.
//
// All three helpers (create / delete / revert) return a clear error
// when no block driver is registered, so the gRPC layer can map it
// to FailedPrecondition without guesswork.

import (
	"context"
	"fmt"

	drivers "github.com/openweft/weft-drivers"
)

// blockDriverName matches drivers.VolumeDriver.Name() for the
// weft-block driver. Defined as a constant so renames flow through
// one edit.
const blockDriverName = "block"

// blockVolumeDriver returns any registered VolumeDriver whose
// Name() == "block". For HA installs the dispatch table holds one
// such driver per host running weft-block ; the call is idempotent
// at the driver layer, so picking the first registered one is
// sufficient (the driver routes by volume UUID against etcd, not by
// the host the driver runs on).
//
// Returns a clear ErrUnsupported-shaped error when no block driver
// is registered — the gRPC handler can map that to FailedPrecondition.
func (a *Adapter) blockVolumeDriver() (drivers.VolumeDriver, error) {
	a.driverDispatchMu.RLock()
	defer a.driverDispatchMu.RUnlock()
	for _, h := range a.driverDispatch {
		if h == nil || h.Volume == nil {
			continue
		}
		if h.Volume.Name() == blockDriverName {
			return h.Volume, nil
		}
	}
	// Multi-plugin sets : same lookup against the per-kind table.
	for _, set := range a.driverDispatchSet {
		for _, h := range set {
			if h == nil || h.Volume == nil {
				continue
			}
			if h.Volume.Name() == blockDriverName {
				return h.Volume, nil
			}
		}
	}
	return nil, fmt.Errorf("no %q volume driver registered (block-backend operations require weft-block on at least one host)", blockDriverName)
}

// createBlockSnapshot dispatches CreateSnapshot to the block driver.
// The driver freezes the volume's controller chain under snapshotName
// and writes a snapshot row in its own state store ; weft's snapshot
// registry row is still the durable name → UUID binding.
func (a *Adapter) createBlockSnapshot(ctx context.Context, volumeUUID, snapshotName string) error {
	vd, err := a.blockVolumeDriver()
	if err != nil {
		return err
	}
	if _, err := vd.CreateSnapshot(ctx, drivers.SnapshotSpec{
		VolumeUUID: volumeUUID,
		Name:       snapshotName,
	}); err != nil {
		return fmt.Errorf("weft-block CreateSnapshot %s/%s: %w", volumeUUID, snapshotName, err)
	}
	return nil
}

// deleteBlockSnapshot dispatches DeleteSnapshot to the block driver.
// Idempotent — deleting a missing snapshot is a no-op driver-side.
func (a *Adapter) deleteBlockSnapshot(ctx context.Context, volumeUUID, snapshotName string) error {
	vd, err := a.blockVolumeDriver()
	if err != nil {
		return err
	}
	if err := vd.DeleteSnapshot(ctx, volumeUUID, snapshotName); err != nil {
		return fmt.Errorf("weft-block DeleteSnapshot %s/%s: %w", volumeUUID, snapshotName, err)
	}
	return nil
}

// revertBlockSnapshot dispatches RevertSnapshot to the block driver.
// The driver enforces "volume must be detached" with ErrInUse ;
// callers that want to revert a live volume should detach it first.
func (a *Adapter) revertBlockSnapshot(ctx context.Context, volumeUUID, snapshotName string) error {
	vd, err := a.blockVolumeDriver()
	if err != nil {
		return err
	}
	if err := vd.RevertSnapshot(ctx, volumeUUID, snapshotName); err != nil {
		return fmt.Errorf("weft-block RevertSnapshot %s/%s: %w", volumeUUID, snapshotName, err)
	}
	return nil
}
