package weftdriverplugin

// convert.go translates between the flat structs in weft-drivers/types.go and
// the generated protobuf messages, and maps errors across the gRPC boundary.
//
// Error mapping preserves the weft-drivers public-contract sentinels so that
// callers' errors.Is(...) keep working through the plugin transport — exactly
// what hypervisor.go promised ("the gRPC plugin transport can't surface
// [panics] cleanly"; sentinels travel as gRPC status codes instead).

import (
	"context"
	"errors"
	"fmt"

	drivers "github.com/openweft/weft-drivers"
	"github.com/openweft/weft-driver-plugin/driverpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ----- error mapping -----

// encodeErr is applied on the plugin (server) side: it turns a driver's
// domain error into a gRPC status error whose code identifies the sentinel,
// so decodeErr can reconstruct an errors.Is-comparable error on the host side.
func encodeErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, drivers.ErrNotApplicable):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, drivers.ErrUnsupported):
		return status.Error(codes.Unimplemented, err.Error())
	case errors.Is(err, drivers.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	default:
		return status.Error(codes.Unknown, err.Error())
	}
}

// decodeErr is applied on the host (client) side: it reverses encodeErr,
// re-wrapping the well-known sentinels so errors.Is matches as it would in
// the in-process case.
func decodeErr(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	msg := st.Message()
	switch st.Code() {
	case codes.Canceled:
		return context.Canceled
	case codes.DeadlineExceeded:
		return context.DeadlineExceeded
	case codes.FailedPrecondition:
		return fmt.Errorf("%s: %w", msg, drivers.ErrNotApplicable)
	case codes.Unimplemented:
		return fmt.Errorf("%s: %w", msg, drivers.ErrUnsupported)
	case codes.NotFound:
		return fmt.Errorf("%s: %w", msg, drivers.ErrNotFound)
	default:
		return errors.New(msg)
	}
}

// ----- struct <-> pb conversions -----

func hostInfoToPB(h drivers.HostInfo) *driverpb.HostInfo {
	return &driverpb.HostInfo{
		Uuid:         h.UUID,
		Hostname:     h.Hostname,
		Az:           h.AZ,
		Hypervisor:   h.Hypervisor,
		Architecture: h.Architecture,
	}
}

func hostInfoFromPB(p *driverpb.HostInfo) drivers.HostInfo {
	if p == nil {
		return drivers.HostInfo{}
	}
	return drivers.HostInfo{
		UUID:         p.Uuid,
		Hostname:     p.Hostname,
		AZ:           p.Az,
		Hypervisor:   p.Hypervisor,
		Architecture: p.Architecture,
	}
}

func vmSpecToPB(s drivers.VMSpec) *driverpb.VMSpec {
	return &driverpb.VMSpec{
		Uuid:        s.UUID,
		ProjectUuid: s.ProjectUUID,
		Name:        s.Name,
		CpuCount:    int32(s.CPUCount),
		MemoryMib:   int32(s.MemoryMiB),
		BootKind:    s.BootKind,
		BootRef:     s.BootRef,
		Cmdline:     s.Cmdline,
		VsockCid:    s.VsockCID,
	}
}

func vmSpecFromPB(p *driverpb.VMSpec) drivers.VMSpec {
	if p == nil {
		return drivers.VMSpec{}
	}
	return drivers.VMSpec{
		UUID:        p.Uuid,
		ProjectUUID: p.ProjectUuid,
		Name:        p.Name,
		CPUCount:    int(p.CpuCount),
		MemoryMiB:   int(p.MemoryMib),
		BootKind:    p.BootKind,
		BootRef:     p.BootRef,
		Cmdline:     p.Cmdline,
		VsockCID:    p.VsockCid,
	}
}

func diskSpecToPB(s drivers.DiskSpec) *driverpb.DiskSpec {
	return &driverpb.DiskSpec{
		VolumeUuid:  s.VolumeUUID,
		BackingPath: s.BackingPath,
		Bus:         s.Bus,
		SizeGib:     int32(s.SizeGiB),
		ReadOnly:    s.ReadOnly,
		Boot:        s.Boot,
	}
}

func diskSpecFromPB(p *driverpb.DiskSpec) drivers.DiskSpec {
	if p == nil {
		return drivers.DiskSpec{}
	}
	return drivers.DiskSpec{
		VolumeUUID:  p.VolumeUuid,
		BackingPath: p.BackingPath,
		Bus:         p.Bus,
		SizeGiB:     int(p.SizeGib),
		ReadOnly:    p.ReadOnly,
		Boot:        p.Boot,
	}
}

func nicHandleToPB(h drivers.NICHandle) *driverpb.NICHandle {
	return &driverpb.NICHandle{Device: h.Device, Mac: h.MAC}
}

func nicHandleFromPB(p *driverpb.NICHandle) drivers.NICHandle {
	if p == nil {
		return drivers.NICHandle{}
	}
	return drivers.NICHandle{Device: p.Device, MAC: p.Mac}
}

func networkSpecToPB(s drivers.NetworkSpec) *driverpb.NetworkSpec {
	return &driverpb.NetworkSpec{
		Uuid:           s.UUID,
		ProjectUuid:    s.ProjectUUID,
		Name:           s.Name,
		Cidr:           s.CIDR,
		Gateway:        s.Gateway,
		DnsServers:     s.DNSServers,
		Type:           s.Type,
		MeshListenPort: int32(s.MeshListenPort),
		MeshEndpoint:   s.MeshEndpoint,
	}
}

func networkSpecFromPB(p *driverpb.NetworkSpec) drivers.NetworkSpec {
	if p == nil {
		return drivers.NetworkSpec{}
	}
	return drivers.NetworkSpec{
		UUID:           p.Uuid,
		ProjectUUID:    p.ProjectUuid,
		Name:           p.Name,
		CIDR:           p.Cidr,
		Gateway:        p.Gateway,
		DNSServers:     p.DnsServers,
		Type:           p.Type,
		MeshListenPort: int(p.MeshListenPort),
		MeshEndpoint:   p.MeshEndpoint,
	}
}

func portSpecToPB(s drivers.PortSpec) *driverpb.PortSpec {
	return &driverpb.PortSpec{
		Uuid:                    s.UUID,
		ProjectUuid:             s.ProjectUUID,
		VmUuid:                  s.VMUUID,
		NetworkUuid:             s.NetworkUUID,
		Mac:                     s.MAC,
		Ip:                      s.IP,
		WireguardPubKey:         s.WireguardPubKey,
		MeshEndpoint:            s.MeshEndpoint,
		EffectiveSecurityGroups: s.EffectiveSecurityGroups,
	}
}

func portSpecFromPB(p *driverpb.PortSpec) drivers.PortSpec {
	if p == nil {
		return drivers.PortSpec{}
	}
	return drivers.PortSpec{
		UUID:                    p.Uuid,
		ProjectUUID:             p.ProjectUuid,
		VMUUID:                  p.VmUuid,
		NetworkUUID:             p.NetworkUuid,
		MAC:                     p.Mac,
		IP:                      p.Ip,
		WireguardPubKey:         p.WireguardPubKey,
		MeshEndpoint:            p.MeshEndpoint,
		EffectiveSecurityGroups: p.EffectiveSecurityGroups,
	}
}

func volumeSpecToPB(s drivers.VolumeSpec) *driverpb.VolumeSpec {
	return &driverpb.VolumeSpec{
		Uuid:        s.UUID,
		ProjectUuid: s.ProjectUUID,
		Name:        s.Name,
		SizeGib:     int32(s.SizeGiB),
		Format:      s.Format,
	}
}

func volumeSpecFromPB(p *driverpb.VolumeSpec) drivers.VolumeSpec {
	if p == nil {
		return drivers.VolumeSpec{}
	}
	return drivers.VolumeSpec{
		UUID:        p.Uuid,
		ProjectUUID: p.ProjectUuid,
		Name:        p.Name,
		SizeGiB:     int(p.SizeGib),
		Format:      p.Format,
	}
}

func attachedVolumeToPB(v drivers.AttachedVolume) *driverpb.AttachedVolume {
	return &driverpb.AttachedVolume{BackingPath: v.BackingPath, ReadOnly: v.ReadOnly}
}

func attachedVolumeFromPB(p *driverpb.AttachedVolume) drivers.AttachedVolume {
	if p == nil {
		return drivers.AttachedVolume{}
	}
	return drivers.AttachedVolume{BackingPath: p.BackingPath, ReadOnly: p.ReadOnly}
}

// ----- Snapshot -----

func snapshotSpecToPB(s drivers.SnapshotSpec) *driverpb.SnapshotSpec {
	return &driverpb.SnapshotSpec{
		VolumeUuid: s.VolumeUUID,
		Name:       s.Name,
		Labels:     s.Labels,
	}
}

func snapshotSpecFromPB(p *driverpb.SnapshotSpec) drivers.SnapshotSpec {
	if p == nil {
		return drivers.SnapshotSpec{}
	}
	return drivers.SnapshotSpec{
		VolumeUUID: p.VolumeUuid,
		Name:       p.Name,
		Labels:     p.Labels,
	}
}

func snapshotToPB(s drivers.Snapshot) *driverpb.Snapshot {
	return &driverpb.Snapshot{
		VolumeUuid:      s.VolumeUUID,
		Name:            s.Name,
		Parent:          s.Parent,
		SizeBytes:       s.SizeBytes,
		CreatedAtUnixNs: s.CreatedAtUnixNs,
		Labels:          s.Labels,
		UserCreated:     s.UserCreated,
	}
}

func snapshotFromPB(p *driverpb.Snapshot) drivers.Snapshot {
	if p == nil {
		return drivers.Snapshot{}
	}
	return drivers.Snapshot{
		VolumeUUID:      p.VolumeUuid,
		Name:            p.Name,
		Parent:          p.Parent,
		SizeBytes:       p.SizeBytes,
		CreatedAtUnixNs: p.CreatedAtUnixNs,
		Labels:          p.Labels,
		UserCreated:     p.UserCreated,
	}
}

// ----- Backup -----

func backupEncryptionToPB(e drivers.BackupEncryption) *driverpb.BackupEncryption {
	if e.Algorithm == "" && e.PassphraseEnv == "" && e.KDF == "" && len(e.KDFParams) == 0 {
		return nil
	}
	return &driverpb.BackupEncryption{
		Algorithm:      e.Algorithm,
		PassphraseEnv:  e.PassphraseEnv,
		Kdf:            e.KDF,
		KdfParams:      e.KDFParams,
	}
}

func backupEncryptionFromPB(p *driverpb.BackupEncryption) drivers.BackupEncryption {
	if p == nil {
		return drivers.BackupEncryption{}
	}
	return drivers.BackupEncryption{
		Algorithm:     p.Algorithm,
		PassphraseEnv: p.PassphraseEnv,
		KDF:           p.Kdf,
		KDFParams:     p.KdfParams,
	}
}

func backupEncryptionInfoToPB(e drivers.BackupEncryptionInfo) *driverpb.BackupEncryptionInfo {
	if e.Algorithm == "" && e.KDF == "" && len(e.KDFParams) == 0 && e.SaltHex == "" {
		return nil
	}
	return &driverpb.BackupEncryptionInfo{
		Algorithm: e.Algorithm,
		Kdf:       e.KDF,
		KdfParams: e.KDFParams,
		SaltHex:   e.SaltHex,
	}
}

func backupEncryptionInfoFromPB(p *driverpb.BackupEncryptionInfo) drivers.BackupEncryptionInfo {
	if p == nil {
		return drivers.BackupEncryptionInfo{}
	}
	return drivers.BackupEncryptionInfo{
		Algorithm: p.Algorithm,
		KDF:       p.Kdf,
		KDFParams: p.KdfParams,
		SaltHex:   p.SaltHex,
	}
}

func backupSpecToPB(s drivers.BackupSpec) *driverpb.BackupSpec {
	return &driverpb.BackupSpec{
		VolumeUuid:   s.VolumeUUID,
		SnapshotName: s.SnapshotName,
		Target:       s.Target,
		Labels:       s.Labels,
		ParentUrl:    s.ParentURL,
		Encryption:   backupEncryptionToPB(s.Encryption),
	}
}

func backupSpecFromPB(p *driverpb.BackupSpec) drivers.BackupSpec {
	if p == nil {
		return drivers.BackupSpec{}
	}
	return drivers.BackupSpec{
		VolumeUUID:   p.VolumeUuid,
		SnapshotName: p.SnapshotName,
		Target:       p.Target,
		Labels:       p.Labels,
		ParentURL:    p.ParentUrl,
		Encryption:   backupEncryptionFromPB(p.Encryption),
	}
}

func backupToPB(b drivers.Backup) *driverpb.Backup {
	return &driverpb.Backup{
		VolumeUuid:      b.VolumeUUID,
		SnapshotName:    b.SnapshotName,
		Url:             b.URL,
		ParentUrl:       b.ParentURL,
		Encryption:      backupEncryptionInfoToPB(b.Encryption),
		SizeBytes:       b.SizeBytes,
		CreatedAtUnixNs: b.CreatedAtUnixNs,
		Labels:          b.Labels,
		State:           b.State,
		Error:           b.Error,
	}
}

func backupFromPB(p *driverpb.Backup) drivers.Backup {
	if p == nil {
		return drivers.Backup{}
	}
	return drivers.Backup{
		VolumeUUID:      p.VolumeUuid,
		SnapshotName:    p.SnapshotName,
		URL:             p.Url,
		ParentURL:       p.ParentUrl,
		Encryption:      backupEncryptionInfoFromPB(p.Encryption),
		SizeBytes:       p.SizeBytes,
		CreatedAtUnixNs: p.CreatedAtUnixNs,
		Labels:          p.Labels,
		State:           p.State,
		Error:           p.Error,
	}
}
