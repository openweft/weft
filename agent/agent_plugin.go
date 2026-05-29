package agent

// agent_plugin.go launches this host's driver as an external go-plugin
// process. The cgo Apple-VZ datapath (and the pure-Go QEMU one) now live in
// separate executables — weft-driver-vz / weft-driver-qemu — so the agent
// binary itself is pure Go. Platform files pick the executable + label.

import (
	"context"
	"io"
	"path/filepath"

	weftplugin "github.com/openweft/weft-driver-plugin"
	"github.com/openweft/weft/driverplugins"
)

// launchDriverPlugin is the seam tests override with an in-process fake so the
// agent's unit tests don't spawn a subprocess. Default: resolve the plugin
// binary (local-first, then OCI pull — see driverplugins) and launch it.
var launchDriverPlugin = func(opts weftplugin.LaunchOptions) (*weftplugin.DriverSet, io.Closer, error) {
	cacheDir := filepath.Join(opts.StateDir, "plugins")
	path, err := driverplugins.Resolve(context.Background(), opts.Executable, cacheDir, driverplugins.FromEnv())
	if err != nil {
		return nil, nil, err
	}
	opts.Executable = path
	set, client, err := weftplugin.Launch(opts)
	if err != nil {
		return nil, nil, err
	}
	return set, closerFunc(client.Kill), nil
}

type closerFunc func()

func (f closerFunc) Close() error { f(); return nil }

// buildLocalHandles launches the host's driver plugin and returns its driver
// handles, the hypervisor label, and a closer that stops the plugin.
func buildLocalHandles(opts Options, uuid, hostname string) (DriverHandles, string, io.Closer, error) {
	set, closer, err := launchDriverPlugin(weftplugin.LaunchOptions{
		Executable: localDriverPlugin,
		HostUUID:   uuid,
		Hostname:   hostname,
		AZ:         opts.AZ,
		StateDir:   opts.StateDir,
	})
	if err != nil {
		return DriverHandles{}, localHypervisorLabel, nil, err
	}
	return DriverHandles{
		Hypervisor: set.Hypervisor,
		Network:    set.Network,
		Volume:     set.Volume,
		Image:      set.Image,
	}, localHypervisorLabel, closer, nil
}
