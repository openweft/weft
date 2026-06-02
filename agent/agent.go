package agent

// agent.go implements the Agent lifecycle:
//
//   1. Persist the host UUID at <StateDir>/host-uuid (same
//      mechanism weft-control uses for self-registration).
//   2. Build the local driver Bundle from
//      pkg/openweft/weft-driver-vz/builtin.
//   3. Register with the ControlPlane + attach the driver
//      handles.
//   4. Heartbeat on a timer until Stop().
//
// Today this is the in-process integration target: an
// embedded ControlPlane (single-process weft-control) calls
// agent.Start() during boot, the agent registers itself, and
// the dispatch table now has the local Bundle wired via the
// same code path a remote gRPC agent will use.
//
// In a future commit the same Start() is what runs inside the
// standalone `weft-agent` binary, with the ControlPlane being a
// gRPC client stub instead of an in-process shim.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Options bundles the agent's construction inputs.
type Options struct {
	// StateDir is the per-agent on-disk root. host-uuid lives
	// directly under it; per-VM state under <StateDir>/vms (the
	// driver Bundle owns that path).
	StateDir string
	// Hostname overrides os.Hostname() in the agent's
	// HostRegistration. Useful when:
	//   - the agent runs embedded in weft-control's process and
	//     the os hostname is already taken by weft-control's
	//     self-registration (embedded integration tests),
	//   - the operator wants a logical name independent of DNS.
	// Empty means "use os.Hostname()".
	Hostname string
	// AZ / Rack / Endpoint are operator-supplied placement
	// metadata. Empty AZ/Rack are allowed (single-rack dev
	// clusters). Endpoint is meaningful only when the agent
	// runs as its own process; empty for embedded use.
	AZ       string
	Rack     string
	Endpoint string
	// Labels are operator-set free-form tags propagated to the
	// host registry.
	Labels map[string]string
	// HeartbeatInterval defaults to 30s when zero.
	HeartbeatInterval time.Duration
	// ControlPlane is the surface the agent registers with.
	// Required.
	ControlPlane ControlPlane
	// LocalHandles, when non-nil, supplies the host's driver handles directly
	// instead of launching the driver plugin (weft-driver-vz / weft-driver-qemu).
	// Use it when the agent is embedded in a process that already holds a local
	// driver — the in-process control plane — to avoid launching a second
	// plugin, and in tests to stay subprocess-free.
	LocalHandles *DriverHandles
	// Drivers enables multi-plugin mode. Each entry names a driver kind
	// ("vz" | "qemu") + the guest architectures that driver handles on
	// this host. The agent launches one weft-driver-<kind> subprocess per
	// entry ; the scheduler matches a microVM's arch against the union to
	// pick the right driver (cluster.HostDriver carries the same shape in
	// the HCL ; this Options field is the runtime mirror).
	//
	// Empty → single-driver legacy mode : the agent launches the
	// build-tag-default driver (weft-driver-vz on darwin, weft-driver-qemu
	// on linux) and registers it as the host's only capability. Existing
	// deployments don't have to opt in.
	Drivers []DriverSpec
}

// DriverSpec describes one driver to launch on this host : its kind
// ("vz" or "qemu") and the set of guest architectures it can run.
// Empty Arches defaults to runtime.GOARCH at registration time.
type DriverSpec struct {
	Kind   string
	Arches []string
}

// Agent is the per-host worker.
type Agent struct {
	opts       Options
	hostUUID   string
	hostname   string
	// handles + hypervisor describe the PRIMARY driver. In single-plugin
	// mode they're the only handles ; in multi-plugin mode they mirror
	// driverSet[primaryKind] so legacy dispatch keeps working until the
	// per-kind table lands.
	handles      DriverHandles
	hypervisor   string
	// driverSet is non-nil only in multi-plugin mode (Options.Drivers
	// non-empty). Keys are driver kinds ("vz" / "qemu") ; values are
	// per-driver handles. Reserved for the per-arch dispatch table
	// — the per-RPC routing logic that picks one entry per microVM
	// based on its required architecture.
	driverSet    map[string]DriverHandles
	pluginCloser io.Closer // kills the local driver plugin on Stop
	cancel       context.CancelFunc
	doneCh       chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
	startedErr error
}

// New constructs an Agent without starting it. Returns an
// error when required Options fields are missing.
func New(opts Options) (*Agent, error) {
	if opts.ControlPlane == nil {
		return nil, fmt.Errorf("weft-agent: ControlPlane is required")
	}
	if opts.StateDir == "" {
		return nil, fmt.Errorf("weft-agent: StateDir is required")
	}
	if opts.HeartbeatInterval == 0 {
		opts.HeartbeatInterval = 30 * time.Second
	}
	return &Agent{opts: opts}, nil
}

// Start runs the full agent lifecycle:
//
//  1. Resolve host UUID + hostname.
//  2. Build the driver Bundle.
//  3. Call ControlPlane.RegisterHost + AttachDrivers.
//  4. Spin up the heartbeat goroutine.
//
// Idempotent: re-calling is a no-op. Errors from step 1-3 are
// fatal; the heartbeat goroutine logs but doesn't propagate
// transient failures (the next tick retries).
func (a *Agent) Start(ctx context.Context) error {
	a.startOnce.Do(func() {
		a.startedErr = a.start(ctx)
	})
	return a.startedErr
}

func (a *Agent) start(ctx context.Context) error {
	uuid, err := loadOrCreateHostUUID(a.opts.StateDir)
	if err != nil {
		return fmt.Errorf("weft-agent: host-uuid: %w", err)
	}
	a.hostUUID = uuid
	hn := a.opts.Hostname
	if hn == "" {
		hn, _ = os.Hostname()
	}
	if hn == "" {
		hn = "unknown"
	}
	a.hostname = hn

	// Acquire this host's driver handles. Three paths :
	//
	//   1. LocalHandles set : injected externally (embedded reuse / tests).
	//      No plugin launch.
	//   2. Drivers slice set : multi-plugin mode. One weft-driver-<kind>
	//      subprocess per entry ; the primary (first by deterministic
	//      ordering — vz before qemu) populates the legacy singletons.
	//      a.driverSet holds the full map for the per-kind dispatch
	//      table once that lands.
	//   3. Otherwise : single-driver legacy via build-tag default.
	var driverCaps []HostDriverCapability
	switch {
	case a.opts.LocalHandles != nil:
		a.handles, a.hypervisor = *a.opts.LocalHandles, localHypervisorLabel
		driverCaps = []HostDriverCapability{{Kind: a.hypervisor, Arches: []string{runtime.GOARCH}}}
	case len(a.opts.Drivers) > 0:
		set, primary, closer, err := buildLocalHandlesMulti(a.opts, uuid, hn)
		if err != nil {
			return fmt.Errorf("weft-agent: launch driver plugins : %w", err)
		}
		// Primary = first by stable order. Until the per-kind dispatch
		// table lands, the singleton handles point at it.
		primaryKind := ""
		for _, k := range []string{"vz", "qemu"} {
			if _, ok := set[k]; ok {
				primaryKind = k
				break
			}
		}
		a.handles = set[primaryKind]
		a.driverSet = set
		a.hypervisor = primary
		a.pluginCloser = closer
		// Build the capability list from Options.Drivers so the
		// scheduler sees the same arch coverage the operator declared
		// in cluster.hcl.
		driverCaps = make([]HostDriverCapability, 0, len(a.opts.Drivers))
		for _, d := range a.opts.Drivers {
			arches := append([]string(nil), d.Arches...)
			if len(arches) == 0 {
				arches = []string{runtime.GOARCH}
			}
			driverCaps = append(driverCaps, HostDriverCapability{Kind: d.Kind, Arches: arches})
		}
	default:
		handles, hv, closer, err := buildLocalHandles(a.opts, uuid, hn)
		if err != nil {
			return fmt.Errorf("weft-agent: launch driver plugin: %w", err)
		}
		a.handles, a.hypervisor, a.pluginCloser = handles, hv, closer
		driverCaps = []HostDriverCapability{{Kind: a.hypervisor, Arches: []string{runtime.GOARCH}}}
	}

	reg := HostRegistration{
		UUID:           uuid,
		Hostname:       hn,
		AZ:             a.opts.AZ,
		Rack:           a.opts.Rack,
		Endpoint:       a.opts.Endpoint,
		Hypervisor:     a.hypervisor,
		Architecture:   runtime.GOARCH,
		// Drivers carries the full capability list — one entry per
		// driver plugin this agent has launched, with the set of
		// guest archs each can run. Single-plugin agents publish a
		// 1-entry list mirroring the legacy singletons ; multi-plugin
		// agents (Apple Silicon dual VZ + QEMU) publish one entry per
		// kind so the scheduler can match arch → driver.
		Drivers:        driverCaps,
		NetworkTypes:   []string{"nat", "bridged", "isolated", "mesh"},
		VolumeBackends: []string{"file"},
		Labels:         a.opts.Labels,
	}
	if _, err := a.opts.ControlPlane.RegisterHost(ctx, reg); err != nil {
		return fmt.Errorf("weft-agent: RegisterHost: %w", err)
	}
	// Multi-plugin agents push their per-kind set ; CP routes RPCs
	// to the right driver via HostHandleOnArch. On legacy CP impls
	// (ErrAttachSetUnsupported) we fall back to AttachDrivers with
	// the primary entry — the host still works in single-plugin
	// dispatch mode until the CP catches up.
	if a.driverSet != nil {
		if err := a.opts.ControlPlane.AttachDriverSet(ctx, uuid, a.driverSet); err != nil {
			if !errors.Is(err, ErrAttachSetUnsupported) {
				return fmt.Errorf("weft-agent: AttachDriverSet: %w", err)
			}
			// Fall through to the legacy attach below.
		} else {
			goto heartbeat
		}
	}
	if err := a.opts.ControlPlane.AttachDrivers(ctx, uuid, a.handles); err != nil {
		return fmt.Errorf("weft-agent: AttachDrivers: %w", err)
	}
heartbeat:

	// Spin up the heartbeat goroutine.
	hbCtx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.doneCh = make(chan struct{})
	go a.heartbeatLoop(hbCtx)
	return nil
}

// Stop releases agent resources. Cancels the heartbeat loop +
// waits for it to drain. Idempotent.
func (a *Agent) Stop(ctx context.Context) error {
	a.stopOnce.Do(func() {
		if a.cancel != nil {
			a.cancel()
		}
		if a.doneCh != nil {
			select {
			case <-a.doneCh:
			case <-ctx.Done():
			}
		}
		if a.pluginCloser != nil {
			_ = a.pluginCloser.Close()
		}
	})
	return nil
}

// HostUUID returns the UUID this agent registered under. Empty
// before Start.
func (a *Agent) HostUUID() string { return a.hostUUID }

// Handles returns the in-process driver handles the agent built. Used by
// integration tests that want to inspect the same handles the control plane
// received via AttachDrivers.
func (a *Agent) Handles() DriverHandles { return a.handles }

// heartbeatLoop ticks the ControlPlane every HeartbeatInterval
// until ctx is canceled. Errors are logged to stderr; the loop
// keeps running so a transient control-plane outage doesn't
// kill the agent.
func (a *Agent) heartbeatLoop(ctx context.Context) {
	defer close(a.doneCh)
	t := time.NewTicker(a.opts.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			hbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := a.opts.ControlPlane.Heartbeat(hbCtx, a.hostUUID); err != nil {
				fmt.Fprintf(os.Stderr, "weft-agent: heartbeat failed: %v\n", err)
			}
			cancel()
		}
	}
}

// loadOrCreateHostUUID reads <stateDir>/host-uuid; generates +
// persists when absent. Same recipe as weft-control's
// host_self.go — duplicated here so the agent module doesn't
// import weft.
func loadOrCreateHostUUID(stateDir string) (string, error) {
	if stateDir == "" {
		return "", fmt.Errorf("empty state dir")
	}
	path := filepath.Join(stateDir, "host-uuid")
	if b, err := os.ReadFile(path); err == nil {
		uuid := strings.TrimSpace(string(b))
		if uuid != "" {
			return uuid, nil
		}
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir state dir: %w", err)
	}
	uuid := newAgentUUID()
	tmp, err := os.CreateTemp(stateDir, ".host-uuid-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.WriteString(uuid + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	_ = os.Chmod(tmp.Name(), 0o600)
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return uuid, nil
}

// newAgentUUID is a UUIDv4 generator local to this module. We
// don't import weft's `newUUID` because that would create a
// dependency in the wrong direction (the agent must stand
// alone).
func newAgentUUID() string {
	var b [16]byte
	// crypto/rand-style fallback to time-derived bytes — we
	// don't ship until the test sweep guarantees this UUID is
	// unique-enough for the agent identity use case.
	// On macOS / Linux, /dev/urandom is always present.
	f, err := os.Open("/dev/urandom")
	if err == nil {
		_, _ = f.Read(b[:])
		_ = f.Close()
	} else {
		// Time-fallback; non-cryptographic but never colliding
		// within a single host's wall clock.
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(now >> (i * 8))
		}
	}
	// RFC 4122 variant + version 4 bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
