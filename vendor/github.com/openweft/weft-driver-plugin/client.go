package weftdriverplugin

// client.go holds the host-side stubs: each implements one drivers.*Driver
// interface by RPC-ing to the plugin process. These are what the weft control
// plane / agent actually store in their dispatch tables — call sites depend on
// the drivers.* interfaces, so swapping the in-process bundle for these stubs
// is invisible above the HostHandle/DriverHandles boundary.

import (
	"context"

	drivers "github.com/openweft/weft-drivers"
	"github.com/openweft/weft-driver-plugin/driverpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// DriverSet is the dispensed bundle: the four driver interfaces backed by one
// plugin process. The host maps this onto its own HostHandle/DriverHandles.
type DriverSet struct {
	Hypervisor drivers.HypervisorDriver
	Network    drivers.NetworkDriver
	Volume     drivers.VolumeDriver
	Image      drivers.ImageDriver
}

// Compile-time proof the stubs satisfy the driver interfaces.
var (
	_ drivers.HypervisorDriver = (*hypervisorClient)(nil)
	_ drivers.NetworkDriver    = (*networkClient)(nil)
	_ drivers.VolumeDriver     = (*volumeClient)(nil)
	_ drivers.ImageDriver      = (*imageClient)(nil)
)

// ----- Hypervisor -----

type hypervisorClient struct{ c driverpb.HypervisorClient }

func (h *hypervisorClient) HostInfo(ctx context.Context) (drivers.HostInfo, error) {
	resp, err := h.c.HostInfo(ctx, &emptypb.Empty{})
	if err != nil {
		return drivers.HostInfo{}, decodeErr(err)
	}
	return hostInfoFromPB(resp.HostInfo), nil
}

func (h *hypervisorClient) CreateVM(ctx context.Context, spec drivers.VMSpec) error {
	_, err := h.c.CreateVM(ctx, &driverpb.CreateVMRequest{Spec: vmSpecToPB(spec)})
	return decodeErr(err)
}

func (h *hypervisorClient) StartVM(ctx context.Context, vmUUID string) error {
	_, err := h.c.StartVM(ctx, &driverpb.VMUUIDRequest{VmUuid: vmUUID})
	return decodeErr(err)
}

func (h *hypervisorClient) StopVM(ctx context.Context, vmUUID string) error {
	_, err := h.c.StopVM(ctx, &driverpb.VMUUIDRequest{VmUuid: vmUUID})
	return decodeErr(err)
}

func (h *hypervisorClient) DeleteVM(ctx context.Context, vmUUID string) error {
	_, err := h.c.DeleteVM(ctx, &driverpb.VMUUIDRequest{VmUuid: vmUUID})
	return decodeErr(err)
}

func (h *hypervisorClient) AttachDisk(ctx context.Context, vmUUID string, disk drivers.DiskSpec) error {
	_, err := h.c.AttachDisk(ctx, &driverpb.AttachDiskRequest{VmUuid: vmUUID, Disk: diskSpecToPB(disk)})
	return decodeErr(err)
}

func (h *hypervisorClient) DetachDisk(ctx context.Context, vmUUID, volumeUUID string) error {
	_, err := h.c.DetachDisk(ctx, &driverpb.DetachDiskRequest{VmUuid: vmUUID, VolumeUuid: volumeUUID})
	return decodeErr(err)
}

func (h *hypervisorClient) AttachNIC(ctx context.Context, vmUUID string, nic drivers.NICHandle) error {
	_, err := h.c.AttachNIC(ctx, &driverpb.AttachNICRequest{VmUuid: vmUUID, Nic: nicHandleToPB(nic)})
	return decodeErr(err)
}

func (h *hypervisorClient) DetachNIC(ctx context.Context, vmUUID, nicDevice string) error {
	_, err := h.c.DetachNIC(ctx, &driverpb.DetachNICRequest{VmUuid: vmUUID, NicDevice: nicDevice})
	return decodeErr(err)
}

// ----- Network -----

type networkClient struct{ c driverpb.NetworkClient }

func (n *networkClient) HostInfo(ctx context.Context) (drivers.HostInfo, error) {
	resp, err := n.c.HostInfo(ctx, &emptypb.Empty{})
	if err != nil {
		return drivers.HostInfo{}, decodeErr(err)
	}
	return hostInfoFromPB(resp.HostInfo), nil
}

func (n *networkClient) EnsureNetwork(ctx context.Context, spec drivers.NetworkSpec) error {
	_, err := n.c.EnsureNetwork(ctx, &driverpb.EnsureNetworkRequest{Spec: networkSpecToPB(spec)})
	return decodeErr(err)
}

func (n *networkClient) DestroyNetwork(ctx context.Context, networkUUID string) error {
	_, err := n.c.DestroyNetwork(ctx, &driverpb.DestroyNetworkRequest{NetworkUuid: networkUUID})
	return decodeErr(err)
}

func (n *networkClient) AttachPort(ctx context.Context, spec drivers.PortSpec) (drivers.NICHandle, error) {
	resp, err := n.c.AttachPort(ctx, &driverpb.AttachPortRequest{Spec: portSpecToPB(spec)})
	if err != nil {
		return drivers.NICHandle{}, decodeErr(err)
	}
	return nicHandleFromPB(resp.Handle), nil
}

func (n *networkClient) DetachPort(ctx context.Context, portUUID string) error {
	_, err := n.c.DetachPort(ctx, &driverpb.DetachPortRequest{PortUuid: portUUID})
	return decodeErr(err)
}

func (n *networkClient) RotateMeshPeer(ctx context.Context, spec drivers.PortSpec) error {
	_, err := n.c.RotateMeshPeer(ctx, &driverpb.RotateMeshPeerRequest{Spec: portSpecToPB(spec)})
	return decodeErr(err)
}

// ----- Volume -----

type volumeClient struct{ c driverpb.VolumeClient }

// Name and Local are interface methods without a context. They still RPC —
// the plugin owns the answer (a remote driver may report a different backend
// than any compiled-in default). Errors collapse to the zero value, matching
// the in-process contract where these never fail.
func (v *volumeClient) Name() string {
	resp, err := v.c.Name(context.Background(), &emptypb.Empty{})
	if err != nil {
		return ""
	}
	return resp.Name
}

func (v *volumeClient) Local() bool {
	resp, err := v.c.Local(context.Background(), &emptypb.Empty{})
	if err != nil {
		return false
	}
	return resp.Local
}

func (v *volumeClient) HostInfo(ctx context.Context) (drivers.HostInfo, error) {
	resp, err := v.c.HostInfo(ctx, &emptypb.Empty{})
	if err != nil {
		return drivers.HostInfo{}, decodeErr(err)
	}
	return hostInfoFromPB(resp.HostInfo), nil
}

func (v *volumeClient) EnsureVolume(ctx context.Context, spec drivers.VolumeSpec) error {
	_, err := v.c.EnsureVolume(ctx, &driverpb.EnsureVolumeRequest{Spec: volumeSpecToPB(spec)})
	return decodeErr(err)
}

func (v *volumeClient) DestroyVolume(ctx context.Context, volumeUUID string) error {
	_, err := v.c.DestroyVolume(ctx, &driverpb.DestroyVolumeRequest{VolumeUuid: volumeUUID})
	return decodeErr(err)
}

func (v *volumeClient) AttachVolume(ctx context.Context, volumeUUID, hostUUID string) (drivers.AttachedVolume, error) {
	resp, err := v.c.AttachVolume(ctx, &driverpb.AttachVolumeRequest{VolumeUuid: volumeUUID, HostUuid: hostUUID})
	if err != nil {
		return drivers.AttachedVolume{}, decodeErr(err)
	}
	return attachedVolumeFromPB(resp.Volume), nil
}

func (v *volumeClient) DetachVolume(ctx context.Context, volumeUUID, hostUUID string) error {
	_, err := v.c.DetachVolume(ctx, &driverpb.DetachVolumeRequest{VolumeUuid: volumeUUID, HostUuid: hostUUID})
	return decodeErr(err)
}

func (v *volumeClient) CreateSnapshot(ctx context.Context, spec drivers.SnapshotSpec) (drivers.Snapshot, error) {
	resp, err := v.c.CreateSnapshot(ctx, &driverpb.CreateSnapshotRequest{Spec: snapshotSpecToPB(spec)})
	if err != nil {
		return drivers.Snapshot{}, decodeErr(err)
	}
	return snapshotFromPB(resp.Snapshot), nil
}

func (v *volumeClient) ListSnapshots(ctx context.Context, volumeUUID string) ([]drivers.Snapshot, error) {
	resp, err := v.c.ListSnapshots(ctx, &driverpb.ListSnapshotsRequest{VolumeUuid: volumeUUID})
	if err != nil {
		return nil, decodeErr(err)
	}
	out := make([]drivers.Snapshot, 0, len(resp.Snapshots))
	for _, s := range resp.Snapshots {
		out = append(out, snapshotFromPB(s))
	}
	return out, nil
}

func (v *volumeClient) DeleteSnapshot(ctx context.Context, volumeUUID, snapshotName string) error {
	_, err := v.c.DeleteSnapshot(ctx, &driverpb.DeleteSnapshotRequest{VolumeUuid: volumeUUID, SnapshotName: snapshotName})
	return decodeErr(err)
}

func (v *volumeClient) RevertSnapshot(ctx context.Context, volumeUUID, snapshotName string) error {
	_, err := v.c.RevertSnapshot(ctx, &driverpb.RevertSnapshotRequest{VolumeUuid: volumeUUID, SnapshotName: snapshotName})
	return decodeErr(err)
}

func (v *volumeClient) CreateBackup(ctx context.Context, spec drivers.BackupSpec) (drivers.Backup, error) {
	resp, err := v.c.CreateBackup(ctx, &driverpb.CreateBackupRequest{Spec: backupSpecToPB(spec)})
	if err != nil {
		return drivers.Backup{}, decodeErr(err)
	}
	return backupFromPB(resp.Backup), nil
}

func (v *volumeClient) ListBackups(ctx context.Context, target, volumeUUID string) ([]drivers.Backup, error) {
	resp, err := v.c.ListBackups(ctx, &driverpb.ListBackupsRequest{Target: target, VolumeUuid: volumeUUID})
	if err != nil {
		return nil, decodeErr(err)
	}
	out := make([]drivers.Backup, 0, len(resp.Backups))
	for _, b := range resp.Backups {
		out = append(out, backupFromPB(b))
	}
	return out, nil
}

func (v *volumeClient) DeleteBackup(ctx context.Context, backupURL string) error {
	_, err := v.c.DeleteBackup(ctx, &driverpb.DeleteBackupRequest{BackupUrl: backupURL})
	return decodeErr(err)
}

func (v *volumeClient) RestoreBackup(ctx context.Context, backupURL string, spec drivers.VolumeSpec) error {
	_, err := v.c.RestoreBackup(ctx, &driverpb.RestoreBackupRequest{BackupUrl: backupURL, Spec: volumeSpecToPB(spec)})
	return decodeErr(err)
}

// ----- Image -----

type imageClient struct{ c driverpb.ImageClient }

func (i *imageClient) HostInfo(ctx context.Context) (drivers.HostInfo, error) {
	resp, err := i.c.HostInfo(ctx, &emptypb.Empty{})
	if err != nil {
		return drivers.HostInfo{}, decodeErr(err)
	}
	return hostInfoFromPB(resp.HostInfo), nil
}

func (i *imageClient) Pull(ctx context.Context, ref string) error {
	_, err := i.c.Pull(ctx, &driverpb.RefRequest{Ref: ref})
	return decodeErr(err)
}

func (i *imageClient) LocalPath(ctx context.Context, ref string) (string, error) {
	resp, err := i.c.LocalPath(ctx, &driverpb.RefRequest{Ref: ref})
	if err != nil {
		return "", decodeErr(err)
	}
	return resp.Path, nil
}

func (i *imageClient) Delete(ctx context.Context, ref string) error {
	_, err := i.c.Delete(ctx, &driverpb.RefRequest{Ref: ref})
	return decodeErr(err)
}

func (i *imageClient) InCache(ctx context.Context, ref string) (bool, error) {
	resp, err := i.c.InCache(ctx, &driverpb.RefRequest{Ref: ref})
	if err != nil {
		return false, decodeErr(err)
	}
	return resp.InCache, nil
}
