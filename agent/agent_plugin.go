package agent

// agent_plugin.go launches this host's driver as an external go-plugin
// process. The cgo Apple-VZ datapath (and the pure-Go QEMU one) now live in
// separate executables — weft-driver-vz / weft-driver-qemu — so the agent
// binary itself is pure Go. Platform files pick the executable + label.

import (
	"context"
	"fmt"
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
// handles, the hypervisor label, and a closer that stops the plugin. Used by
// the legacy single-driver path (no Options.Drivers, falls back to the
// build-tag default).
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

// buildLocalHandlesMulti launches one weft-driver-<kind> subprocess per
// Options.Drivers entry. Returns a map keyed by kind ("vz" / "qemu") and a
// combined closer that stops every plugin on shutdown ; an error from any
// individual launch aborts the whole set + closes whatever was already up so
// the partial-startup case doesn't leak subprocesses.
//
// The primary kind (the first map key on a deterministic walk — "vz" before
// "qemu") becomes the legacy `Agent.handles` + `Agent.hypervisor` so existing
// dispatch paths that read those fields keep working until the per-kind
// dispatch table lands as the follow-on milestone.
func buildLocalHandlesMulti(opts Options, uuid, hostname string) (map[string]DriverHandles, string, io.Closer, error) {
	if len(opts.Drivers) == 0 {
		// Defensive — caller should pick buildLocalHandles for the
		// legacy path. Return a clear error rather than silently
		// launching the build-tag default.
		return nil, "", nil, fmt.Errorf("agent: buildLocalHandlesMulti called with no Drivers in Options")
	}
	out := make(map[string]DriverHandles, len(opts.Drivers))
	closers := make([]io.Closer, 0, len(opts.Drivers))
	cleanup := func() {
		// Reverse order — last started, first stopped. Mirrors
		// systemd unit teardown.
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i].Close()
		}
	}
	var primary string
	// Walk kinds in a stable order so the "primary" choice is
	// deterministic across restarts. vz comes first when present —
	// arm64 native is faster than QEMU/TCG.
	for _, kind := range []string{"vz", "qemu"} {
		spec, ok := findDriverSpec(opts.Drivers, kind)
		if !ok {
			continue
		}
		exec, err := executableFor(spec.Kind)
		if err != nil {
			cleanup()
			return nil, "", nil, err
		}
		set, closer, err := launchDriverPlugin(weftplugin.LaunchOptions{
			Executable: exec,
			HostUUID:   uuid,
			Hostname:   hostname,
			AZ:         opts.AZ,
			StateDir:   opts.StateDir,
		})
		if err != nil {
			cleanup()
			return nil, "", nil, fmt.Errorf("launch driver %q : %w", spec.Kind, err)
		}
		closers = append(closers, closer)
		out[spec.Kind] = DriverHandles{
			Hypervisor: set.Hypervisor,
			Network:    set.Network,
			Volume:     set.Volume,
			Image:      set.Image,
		}
		if primary == "" {
			primary = labelFor(spec.Kind)
		}
	}
	if len(out) == 0 {
		return nil, "", nil, fmt.Errorf("agent: Drivers contained no recognised kinds (want vz / qemu)")
	}
	return out, primary, closerFunc(cleanup), nil
}

// findDriverSpec is a linear scan over Options.Drivers ; the slice
// is at most 2 entries, so big-O is moot.
func findDriverSpec(specs []DriverSpec, kind string) (DriverSpec, bool) {
	for _, s := range specs {
		if s.Kind == kind {
			return s, true
		}
	}
	return DriverSpec{}, false
}

// executableFor maps a driver kind to its plugin binary name.
// Centralised so a future rename (or a per-arch suffix scheme) lands
// in one place.
func executableFor(kind string) (string, error) {
	switch kind {
	case "vz":
		return "weft-driver-vz", nil
	case "qemu":
		return "weft-driver-qemu", nil
	default:
		return "", fmt.Errorf("agent: unknown driver kind %q (want vz / qemu)", kind)
	}
}

// labelFor maps a driver kind to its public hypervisor label —
// what the host registry stores in Host.Hypervisor for backward-compat
// with single-driver dispatch.
func labelFor(kind string) string {
	switch kind {
	case "vz":
		return "apple-vz"
	case "qemu":
		return "qemu"
	default:
		return kind
	}
}
