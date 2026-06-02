package agent

// plugin_seam_test.go installs an in-process driver set so the agent's tests
// (New → Start → buildLocalHandles) don't spawn the external weft-driver-*
// plugin. The real launcher is smoked in the weft-driver-plugin module.

import (
	"context"
	"io"
	"os"
	"testing"

	weftplugin "github.com/openweft/weft-driver-plugin"
	drivers "github.com/openweft/weft-drivers"
)

func TestMain(m *testing.M) {
	launchDriverPlugin = func(weftplugin.LaunchOptions) (*weftplugin.DriverSet, io.Closer, error) {
		return &weftplugin.DriverSet{
			Hypervisor: seamHypervisor{},
			Network:    seamNetwork{},
			Volume:     seamVolume{},
			Image:      seamImage{},
		}, seamCloser{}, nil
	}
	os.Exit(m.Run())
}

type seamCloser struct{}

func (seamCloser) Close() error { return nil }

type seamHypervisor struct{}

func (seamHypervisor) HostInfo(context.Context) (drivers.HostInfo, error) {
	return drivers.HostInfo{Hypervisor: "seam"}, nil
}
func (seamHypervisor) CreateVM(context.Context, drivers.VMSpec) error             { return nil }
func (seamHypervisor) StartVM(context.Context, string) error                      { return nil }
func (seamHypervisor) StopVM(context.Context, string) error                       { return nil }
func (seamHypervisor) DeleteVM(context.Context, string) error                     { return nil }
func (seamHypervisor) AttachDisk(context.Context, string, drivers.DiskSpec) error { return nil }
func (seamHypervisor) DetachDisk(context.Context, string, string) error           { return nil }
func (seamHypervisor) AttachNIC(context.Context, string, drivers.NICHandle) error { return nil }
func (seamHypervisor) DetachNIC(context.Context, string, string) error            { return nil }

type seamNetwork struct{}

func (seamNetwork) HostInfo(context.Context) (drivers.HostInfo, error) { return drivers.HostInfo{}, nil }
func (seamNetwork) EnsureNetwork(context.Context, drivers.NetworkSpec) error { return nil }
func (seamNetwork) DestroyNetwork(context.Context, string) error            { return nil }
func (seamNetwork) AttachPort(context.Context, drivers.PortSpec) (drivers.NICHandle, error) {
	return drivers.NICHandle{}, nil
}
func (seamNetwork) DetachPort(context.Context, string) error               { return nil }
func (seamNetwork) RotateMeshPeer(context.Context, drivers.PortSpec) error { return nil }

type seamVolume struct{}

func (seamVolume) Name() string { return "seam" }
func (seamVolume) Local() bool  { return true }
func (seamVolume) HostInfo(context.Context) (drivers.HostInfo, error) { return drivers.HostInfo{}, nil }
func (seamVolume) EnsureVolume(context.Context, drivers.VolumeSpec) error { return nil }
func (seamVolume) DestroyVolume(context.Context, string) error           { return nil }
func (seamVolume) AttachVolume(context.Context, string, string) (drivers.AttachedVolume, error) {
	return drivers.AttachedVolume{}, nil
}
func (seamVolume) DetachVolume(context.Context, string, string) error { return nil }

// Snapshot + backup surface — added to the VolumeDriver contract for
// Longhorn / file-image parity. The seam doesn't store anything, so
// every method is a no-op returning the zero value ; tests that
// exercise these paths set up a different fake.
func (seamVolume) CreateSnapshot(context.Context, drivers.SnapshotSpec) (drivers.Snapshot, error) {
	return drivers.Snapshot{}, nil
}
func (seamVolume) ListSnapshots(context.Context, string) ([]drivers.Snapshot, error) {
	return nil, nil
}
func (seamVolume) DeleteSnapshot(context.Context, string, string) error { return nil }
func (seamVolume) RevertSnapshot(context.Context, string, string) error { return nil }
func (seamVolume) CreateBackup(context.Context, drivers.BackupSpec) (drivers.Backup, error) {
	return drivers.Backup{}, nil
}
func (seamVolume) ListBackups(context.Context, string, string) ([]drivers.Backup, error) {
	return nil, nil
}
func (seamVolume) DeleteBackup(context.Context, string) error                       { return nil }
func (seamVolume) RestoreBackup(context.Context, string, drivers.VolumeSpec) error { return nil }

type seamImage struct{}

func (seamImage) HostInfo(context.Context) (drivers.HostInfo, error) { return drivers.HostInfo{}, nil }
func (seamImage) Pull(context.Context, string) error                { return nil }
func (seamImage) LocalPath(context.Context, string) (string, error) { return "", nil }
func (seamImage) Delete(context.Context, string) error              { return nil }
func (seamImage) InCache(context.Context, string) (bool, error)     { return false, nil }
