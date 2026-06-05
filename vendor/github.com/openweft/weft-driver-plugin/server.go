package weftdriverplugin

// server.go holds the plugin-side gRPC adapters: each wraps one concrete
// drivers.*Driver (the cgo Apple-VZ bundle, the pure-Go QEMU bundle, …) and
// serves the matching driverpb service. They run inside the plugin process;
// the host never links them.

import (
	"context"

	drivers "github.com/openweft/weft-drivers"
	"github.com/openweft/weft-driver-plugin/driverpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

var empty = &emptypb.Empty{}

// ----- Hypervisor -----

type hypervisorServer struct {
	driverpb.UnimplementedHypervisorServer
	impl drivers.HypervisorDriver
}

func (s *hypervisorServer) HostInfo(ctx context.Context, _ *emptypb.Empty) (*driverpb.HostInfoResponse, error) {
	hi, err := s.impl.HostInfo(ctx)
	if err != nil {
		return nil, encodeErr(err)
	}
	return &driverpb.HostInfoResponse{HostInfo: hostInfoToPB(hi)}, nil
}

func (s *hypervisorServer) CreateVM(ctx context.Context, r *driverpb.CreateVMRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.CreateVM(ctx, vmSpecFromPB(r.Spec)))
}

func (s *hypervisorServer) StartVM(ctx context.Context, r *driverpb.VMUUIDRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.StartVM(ctx, r.VmUuid))
}

func (s *hypervisorServer) StopVM(ctx context.Context, r *driverpb.VMUUIDRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.StopVM(ctx, r.VmUuid))
}

func (s *hypervisorServer) DeleteVM(ctx context.Context, r *driverpb.VMUUIDRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.DeleteVM(ctx, r.VmUuid))
}

func (s *hypervisorServer) AttachDisk(ctx context.Context, r *driverpb.AttachDiskRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.AttachDisk(ctx, r.VmUuid, diskSpecFromPB(r.Disk)))
}

func (s *hypervisorServer) DetachDisk(ctx context.Context, r *driverpb.DetachDiskRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.DetachDisk(ctx, r.VmUuid, r.VolumeUuid))
}

func (s *hypervisorServer) AttachNIC(ctx context.Context, r *driverpb.AttachNICRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.AttachNIC(ctx, r.VmUuid, nicHandleFromPB(r.Nic)))
}

func (s *hypervisorServer) DetachNIC(ctx context.Context, r *driverpb.DetachNICRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.DetachNIC(ctx, r.VmUuid, r.NicDevice))
}

// ----- Network -----

type networkServer struct {
	driverpb.UnimplementedNetworkServer
	impl drivers.NetworkDriver
}

func (s *networkServer) HostInfo(ctx context.Context, _ *emptypb.Empty) (*driverpb.HostInfoResponse, error) {
	hi, err := s.impl.HostInfo(ctx)
	if err != nil {
		return nil, encodeErr(err)
	}
	return &driverpb.HostInfoResponse{HostInfo: hostInfoToPB(hi)}, nil
}

func (s *networkServer) EnsureNetwork(ctx context.Context, r *driverpb.EnsureNetworkRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.EnsureNetwork(ctx, networkSpecFromPB(r.Spec)))
}

func (s *networkServer) DestroyNetwork(ctx context.Context, r *driverpb.DestroyNetworkRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.DestroyNetwork(ctx, r.NetworkUuid))
}

func (s *networkServer) AttachPort(ctx context.Context, r *driverpb.AttachPortRequest) (*driverpb.AttachPortResponse, error) {
	h, err := s.impl.AttachPort(ctx, portSpecFromPB(r.Spec))
	if err != nil {
		return nil, encodeErr(err)
	}
	return &driverpb.AttachPortResponse{Handle: nicHandleToPB(h)}, nil
}

func (s *networkServer) DetachPort(ctx context.Context, r *driverpb.DetachPortRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.DetachPort(ctx, r.PortUuid))
}

func (s *networkServer) RotateMeshPeer(ctx context.Context, r *driverpb.RotateMeshPeerRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.RotateMeshPeer(ctx, portSpecFromPB(r.Spec)))
}

// ----- Volume -----

type volumeServer struct {
	driverpb.UnimplementedVolumeServer
	impl drivers.VolumeDriver
}

func (s *volumeServer) Name(_ context.Context, _ *emptypb.Empty) (*driverpb.NameResponse, error) {
	return &driverpb.NameResponse{Name: s.impl.Name()}, nil
}

func (s *volumeServer) Local(_ context.Context, _ *emptypb.Empty) (*driverpb.LocalResponse, error) {
	return &driverpb.LocalResponse{Local: s.impl.Local()}, nil
}

func (s *volumeServer) HostInfo(ctx context.Context, _ *emptypb.Empty) (*driverpb.HostInfoResponse, error) {
	hi, err := s.impl.HostInfo(ctx)
	if err != nil {
		return nil, encodeErr(err)
	}
	return &driverpb.HostInfoResponse{HostInfo: hostInfoToPB(hi)}, nil
}

func (s *volumeServer) EnsureVolume(ctx context.Context, r *driverpb.EnsureVolumeRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.EnsureVolume(ctx, volumeSpecFromPB(r.Spec)))
}

func (s *volumeServer) DestroyVolume(ctx context.Context, r *driverpb.DestroyVolumeRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.DestroyVolume(ctx, r.VolumeUuid))
}

func (s *volumeServer) AttachVolume(ctx context.Context, r *driverpb.AttachVolumeRequest) (*driverpb.AttachVolumeResponse, error) {
	av, err := s.impl.AttachVolume(ctx, r.VolumeUuid, r.HostUuid)
	if err != nil {
		return nil, encodeErr(err)
	}
	return &driverpb.AttachVolumeResponse{Volume: attachedVolumeToPB(av)}, nil
}

func (s *volumeServer) DetachVolume(ctx context.Context, r *driverpb.DetachVolumeRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.DetachVolume(ctx, r.VolumeUuid, r.HostUuid))
}

func (s *volumeServer) CreateSnapshot(ctx context.Context, r *driverpb.CreateSnapshotRequest) (*driverpb.CreateSnapshotResponse, error) {
	snap, err := s.impl.CreateSnapshot(ctx, snapshotSpecFromPB(r.Spec))
	if err != nil {
		return nil, encodeErr(err)
	}
	return &driverpb.CreateSnapshotResponse{Snapshot: snapshotToPB(snap)}, nil
}

func (s *volumeServer) ListSnapshots(ctx context.Context, r *driverpb.ListSnapshotsRequest) (*driverpb.ListSnapshotsResponse, error) {
	snaps, err := s.impl.ListSnapshots(ctx, r.VolumeUuid)
	if err != nil {
		return nil, encodeErr(err)
	}
	out := make([]*driverpb.Snapshot, 0, len(snaps))
	for _, sn := range snaps {
		out = append(out, snapshotToPB(sn))
	}
	return &driverpb.ListSnapshotsResponse{Snapshots: out}, nil
}

func (s *volumeServer) DeleteSnapshot(ctx context.Context, r *driverpb.DeleteSnapshotRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.DeleteSnapshot(ctx, r.VolumeUuid, r.SnapshotName))
}

func (s *volumeServer) RevertSnapshot(ctx context.Context, r *driverpb.RevertSnapshotRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.RevertSnapshot(ctx, r.VolumeUuid, r.SnapshotName))
}

func (s *volumeServer) CreateBackup(ctx context.Context, r *driverpb.CreateBackupRequest) (*driverpb.CreateBackupResponse, error) {
	bk, err := s.impl.CreateBackup(ctx, backupSpecFromPB(r.Spec))
	if err != nil {
		return nil, encodeErr(err)
	}
	return &driverpb.CreateBackupResponse{Backup: backupToPB(bk)}, nil
}

func (s *volumeServer) ListBackups(ctx context.Context, r *driverpb.ListBackupsRequest) (*driverpb.ListBackupsResponse, error) {
	backups, err := s.impl.ListBackups(ctx, r.Target, r.VolumeUuid)
	if err != nil {
		return nil, encodeErr(err)
	}
	out := make([]*driverpb.Backup, 0, len(backups))
	for _, b := range backups {
		out = append(out, backupToPB(b))
	}
	return &driverpb.ListBackupsResponse{Backups: out}, nil
}

func (s *volumeServer) DeleteBackup(ctx context.Context, r *driverpb.DeleteBackupRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.DeleteBackup(ctx, r.BackupUrl))
}

func (s *volumeServer) RestoreBackup(ctx context.Context, r *driverpb.RestoreBackupRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.RestoreBackup(ctx, r.BackupUrl, volumeSpecFromPB(r.Spec)))
}

// ----- Image -----

type imageServer struct {
	driverpb.UnimplementedImageServer
	impl drivers.ImageDriver
}

func (s *imageServer) HostInfo(ctx context.Context, _ *emptypb.Empty) (*driverpb.HostInfoResponse, error) {
	hi, err := s.impl.HostInfo(ctx)
	if err != nil {
		return nil, encodeErr(err)
	}
	return &driverpb.HostInfoResponse{HostInfo: hostInfoToPB(hi)}, nil
}

func (s *imageServer) Pull(ctx context.Context, r *driverpb.RefRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.Pull(ctx, r.Ref))
}

func (s *imageServer) LocalPath(ctx context.Context, r *driverpb.RefRequest) (*driverpb.LocalPathResponse, error) {
	p, err := s.impl.LocalPath(ctx, r.Ref)
	if err != nil {
		return nil, encodeErr(err)
	}
	return &driverpb.LocalPathResponse{Path: p}, nil
}

func (s *imageServer) Delete(ctx context.Context, r *driverpb.RefRequest) (*emptypb.Empty, error) {
	return empty, encodeErr(s.impl.Delete(ctx, r.Ref))
}

func (s *imageServer) InCache(ctx context.Context, r *driverpb.RefRequest) (*driverpb.InCacheResponse, error) {
	ok, err := s.impl.InCache(ctx, r.Ref)
	if err != nil {
		return nil, encodeErr(err)
	}
	return &driverpb.InCacheResponse{InCache: ok}, nil
}
