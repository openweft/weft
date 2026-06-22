package weft

// host_self.go owns weft-control's "I am running here too" logic:
//
//   1. Load (or create + persist) the stable host UUID from
//      `<stateDir>/host-uuid`. The file's content survives
//      restarts so the same physical host always shows up as the
//      same Host registry entry, even after a weft binary upgrade.
//
//   2. On startup, register (or idempotently refresh) the Host
//      entry for the local machine using:
//        - persisted UUID
//        - os.Hostname()
//        - runtime.GOARCH for the Architecture
//        - "apple-vz" as the Hypervisor (this is the macOS
//          single-host build; future multi-host weft-agent binaries
//          will report their own hypervisor here)
//
// Today weft-control and weft-agent are the same process: control
// plane + local driver bundle. The Host entry gets created
// regardless. When weft-agent splits into its own binary, this
// same logic moves there — the control-plane weft loses the
// self-registration step and just consumes the registry.
//
// Errors here are non-fatal by design: if the host-uuid file
// can't be written (e.g. read-only stateDir during a test), we
// log + continue without self-registration. The control plane
// still serves requests; the registry just doesn't list this
// host until a later attempt succeeds.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// hostUUIDFile returns the absolute path to the persisted
// host-UUID file. Kept as its own function so tests can override
// stateDir without leaking the path-shape elsewhere.
func (a *Adapter) hostUUIDFile() string {
	return filepath.Join(a.stateDir, "host-uuid")
}

// LocalHostUUID returns the persisted UUID for the host this
// Adapter is running on. Empty when self-registration hasn't
// run (the file isn't materialised until the Adapter's bootstrap
// step writes it). Callers use it to short-circuit local-vs-
// remote dispatch decisions : a RegisterMicroVM request whose
// `host_uuid` equals this value should land on the in-process
// Adapter rather than going through the AgentDispatch stream.
//
// Cheap to call — just a file read. The Adapter doesn't cache
// the value because in practice the file is read once at server
// startup (see cmd/weft/main.go's `weftServer.localHostUUID`).
func (a *Adapter) LocalHostUUID() string {
	if a == nil {
		return ""
	}
	b, err := os.ReadFile(a.hostUUIDFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// loadOrCreateHostUUID reads the persisted UUID; if absent,
// generates one and writes it atomically. The file is 0o600 —
// the UUID is identity, not a secret, but there's no reason for
// other users on the host to read it.
func (a *Adapter) loadOrCreateHostUUID() (string, error) {
	path := a.hostUUIDFile()
	if b, err := os.ReadFile(path); err == nil {
		uuid := strings.TrimSpace(string(b))
		if uuid != "" {
			return uuid, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create stateDir for host-uuid: %w", err)
	}
	uuid := newUUID()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".host-uuid-*")
	if err != nil {
		return "", fmt.Errorf("create temp host-uuid: %w", err)
	}
	if _, err := tmp.WriteString(uuid + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("write temp host-uuid: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("rename host-uuid into place: %w", err)
	}
	return uuid, nil
}

// selfRegisterHost ensures the Host registry has an entry for
// the local machine. Idempotent across restarts thanks to the
// persisted UUID — the second call simply refreshes LastSeenAt
// and any drifted capabilities.
//
// The hypervisor label comes from $WEFT_HYPERVISOR (set by the
// agent's --hypervisor flag), falling back to a per-OS default
// (qemu on linux, apple-vz elsewhere). NetworkTypes + VolumeBackends
// still reflect the single-host macOS build's truth; future commits
// derive them from the driver Bundle so the capability list matches
// what the drivers actually do.
func (a *Adapter) selfRegisterHost() error {
	if a.hostReg == nil {
		return fmt.Errorf("host registry not initialised")
	}
	uuid, err := a.loadOrCreateHostUUID()
	if err != nil {
		return fmt.Errorf("host-uuid: %w", err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	hv := os.Getenv("WEFT_HYPERVISOR")
	if hv == "" {
		if runtime.GOOS == "linux" {
			hv = "qemu"
		} else {
			hv = "apple-vz"
		}
	}

	// Host-level WireGuard mesh identity (cluster prereq #3). The node mints
	// its WG private key locally and registers ONLY the public half; the
	// private key never leaves the host. Best-effort + additive: a mint
	// failure (e.g. read-only stateDir under test) leaves wgPub empty, so the
	// host registers exactly as before and simply doesn't join the host mesh
	// — the legacy path is byte-for-byte unaffected.
	wgPub, wgErr := EnsureHostWGKey(a.stateDir)
	wgIndex := 0
	if wgErr == nil && wgPub != "" {
		wgIndex = hostWGIndexFromUUID(uuid)
	}

	_, err = a.RegisterHost(RegisterHostSpec{
		UUID:           uuid,
		Hostname:       hostname,
		AZ:             os.Getenv("WEFT_AZ"),
		Rack:           os.Getenv("WEFT_RACK"),
		Hypervisor:     hv,
		Architecture:   runtime.GOARCH,
		NetworkTypes:   []string{"nat", "bridged", "isolated", "mesh"},
		VolumeBackends: []string{"file"},
		// GPUs are populated by the agent-side detector. Today
		// detectGPUs() returns nil — real detection (nvidia-smi
		// + /sys/class/drm walk) is a follow-up documented in
		// docs/operations/gpu-scheduling.md. Operators staging GPU
		// hosts today seed inventory statically via cluster.hcl.
		GPUs: detectGPUs(),
		// PCIDevices : non-GPU passthrough inventory (NICs, NVMe,
		// FPGAs, …). detectPCI() walks /sys/bus/pci/devices on
		// Linux ; returns nil on every other GOOS. See
		// docs/operations/pci-passthrough.md for the operator
		// workflow + the "no exclusivity today" gotcha.
		PCIDevices: detectPCI(),
		// Host-mesh identity — public key + stable overlay index only. Empty
		// when minting failed, in which case the registry records no WG
		// identity for this host and it stays out of the mesh.
		WGPublicKey:    wgPub,
		WGOverlayIndex: wgIndex,
		// AgentVersion picks up the env-passed stamp ($WEFT_VERSION) so
		// embedded self-registration (the same-process control-plane
		// path that doesn't go through the agent's HostRegistration)
		// still surfaces the version on the registry. The wrapping
		// `weft agent` exports WEFT_VERSION = its main.version at
		// startup so this stays in lockstep with the agent-path stamp.
		AgentVersion: os.Getenv("WEFT_VERSION"),
	})
	return err
}
