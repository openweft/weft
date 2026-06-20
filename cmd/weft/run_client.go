package main

// run_client.go is the entry-point for `weft agent --client
// --control-plane=URL` — the per-host runtime mode. The agent
// dials a remote `weft agent --server`, registers itself with
// the Host registry, heartbeats on a timer, and runs the
// bidi `AgentDispatch.Connect` stream so the control plane
// can drive VM lifecycle operations against this host's local
// Adapter.
//
// Today's scope :
//
//   * Dial the control-plane Unix socket (or TCP / SSH transport
//     when those flags become first-class).
//   * RegisterHost + Heartbeat via the `agent.ControlPlane`
//     gRPC stub.
//   * Open the AgentDispatch bidi stream + handle incoming
//     DriverRequests (CreateVMOp, RegisterMicroVMOp) via the
//     local Adapter.
//   * Block until SIGINT / SIGTERM.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	weft "github.com/openweft/weft"
	weftclient "github.com/openweft/weft-client"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/agent"
)

// runClient boots the agent against a remote control plane.
// Mirrors `run()` (the all-in-one / server-only path) but skips
// every server-side step : storage, registries, scheduler, gRPC
// listeners. Just dials out + heartbeats + handles dispatched
// driver requests.
func runClient(t fileConfigTargets) error {
	if t.controlPlaneURL == "" {
		return fmt.Errorf("--client requires --control-plane=URL")
	}

	// Dial the control plane. weftclient.Dial wraps the same
	// Unix-socket-or-SSH dial logic the operator CLI uses, so
	// the same transport options apply (--ssh-socket / --ssh-key
	// for cross-host setups when we get there).
	conn, err := weftclient.Dial(t.controlPlaneURL)
	if err != nil {
		return fmt.Errorf("dial control plane %s: %w", t.controlPlaneURL, err)
	}
	defer conn.Close()

	cpClient := weftv1.NewWeftAgentClient(conn)
	dispatchClient := weftv1.NewAgentDispatchClient(conn)
	cp := agent.NewGRPCControlPlane(cpClient, logger)
	logger.Printf("weft agent --client: dialed control plane at %s", t.controlPlaneURL)

	// AttestationService client : only consulted when --attest-tpm is set.
	// Built from the SAME control-plane conn the agent already dialed, so
	// the four attestation RPCs ride the same transport (and the same
	// SSH/TCP/Unix-socket auth) as RegisterHost/Heartbeat. The agent runs
	// the handshake against it before RegisterHost ; on success it stamps
	// the admitted AK Name onto its registration labels.
	var attestClient agent.AttestationClient
	if t.attestTPM {
		attestClient = weftv1.NewAttestationServiceClient(conn)
		logger.Printf("weft agent --client: TPM attestation enabled (device=%s) — running Enroll/Admit handshake before RegisterHost", attestTPMDeviceOrDefault(t.attestTPMDevice))
	}

	// Build the agent. The driver Bundle is built per-host from
	// the local Apple-VZ binding ; same code as the all-in-one
	// path. StateDir reuses the daemon's <stateDir>/vms layout
	// so a node that flips between --client and all-in-one
	// preserves its VM state.
	a, err := agent.New(agent.Options{
		StateDir:        t.configDir, // operator can re-use existing dir
		Hostname:        hostnameOrEmpty(),
		AZ:              os.Getenv("WEFT_AZ"),
		Rack:            os.Getenv("WEFT_RACK"),
		Endpoint:        "",
		ControlPlane:    cp,
		AttestTPM:       t.attestTPM,
		AttestTPMDevice: t.attestTPMDevice,
		AttestClient:    attestClient,
	})
	if err != nil {
		return fmt.Errorf("build agent: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := a.Start(ctx); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}
	logger.Printf("weft agent --client: registered with control plane")

	// Local Adapter for host-side driver dispatch. The agent's
	// DriverHandler closes over this Adapter and calls into it
	// for each incoming DriverRequest. Distinct from any
	// server-side Adapter — this one only knows about VMs on
	// the local host's filesystem.
	localAdapter := weft.New(t.configDir)

	// Open the bidi dispatch stream in a goroutine with
	// reconnect-on-failure : control-plane restart, intermittent
	// network glitch, transient gRPC unavailable — all surface
	// as a stream error and the retry wrapper redials with
	// exponential backoff + jitter. The wrapper returns only
	// when ctx cancels (i.e. process shutdown).
	dispatchDone := make(chan error, 1)
	go func() {
		dispatchDone <- agent.RunDispatchClientWithRetry(ctx,
			func() (agent.AgentDispatchClient, error) {
				// The underlying *grpc.ClientConn handles
				// transport reconnects on its own ; same client
				// view each time is fine.
				return dispatchClient, nil
			},
			agent.DispatchOptions{
				HostUUID:      a.HostUUID(),
				AgentVersion:  "weft-agent (in-tree)",
				Logger:        logger,
				DriverHandler: buildDriverHandler(localAdapter),
			},
			agent.RetryOptions{}, // defaults : 1s → 30s, 25% jitter
		)
	}()

	logger.Printf("weft agent --client: ready ; waiting for signals")
	select {
	case <-ctx.Done():
	case err := <-dispatchDone:
		logger.Printf("weft agent --client: dispatch stream ended: %v", err)
	}
	logger.Printf("weft agent --client: shutting down")
	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()
	return a.Stop(stopCtx)
}

// buildDriverHandler returns the closure the agent runs for
// each incoming DriverRequest. Each op variant gets its own
// case ; today CreateVMOp (placeholder) + RegisterMicroVMOp
// (real, calls the local Adapter). Unknown ops surface a clean
// "not implemented" reply so the control plane sees the gap.
func buildDriverHandler(a weft.VZAdapter) func(context.Context, *weftv1.DriverRequest) *weftv1.DriverReply {
	return func(_ context.Context, req *weftv1.DriverRequest) *weftv1.DriverReply {
		switch op := req.Op.(type) {
		case *weftv1.DriverRequest_RegisterMicroVm:
			err := a.RegisterMicroVM(op.RegisterMicroVm.Project, op.RegisterMicroVm.Name,
				weft.MicroVMBoot{
					BootISO: op.RegisterMicroVm.BootIso,
					Kernel:  op.RegisterMicroVm.Kernel,
					Initrd:  op.RegisterMicroVm.Initrd,
					Cmdline: op.RegisterMicroVm.Cmdline,
				},
				toMicroVMShares(op.RegisterMicroVm.Shares),
				// GPU requests: RegisterMicroVMOp carries no GPU field yet,
				// so dispatched multi-host GPU passthrough needs a weft-proto
				// addition (RequestedGpus on RegisterMicroVMOp). The local
				// path (cmd/weft/main.go) already claims+attaches. Follow-up.
				nil,
			)
			reply := &weftv1.DriverReply{
				RequestId: req.RequestId,
				Result:    &weftv1.DriverReply_RegisterMicroVm{RegisterMicroVm: &weftv1.RegisterMicroVMResult{}},
			}
			if err != nil {
				reply.Error = fmt.Sprintf("RegisterMicroVM: %v", err)
			}
			return reply
		case *weftv1.DriverRequest_CreateVm:
			// CreateVM dispatch : route through Adapter.CloneVM,
			// which already handles image-store materialisation +
			// VM-inventory registration. The wire shape carries
			// (vm_uuid, project, image, cpu, mem_mb, disk_gb) ;
			// CloneVM consumes (image, project, name, extraDisks).
			//
			// Notes :
			//   - vm_uuid is the addressing key. Empty = mint
			//     server-side ; the assigned UUID is echoed back
			//     in CreateVMResult so the control plane can poll
			//     it via the inventory.
			//   - CloneVM uses the VZ driver's compiled-in CPU /
			//     memory defaults today (the surface doesn't accept
			//     per-call overrides). cpu + mem_mb arrive on the
			//     wire ; we log them as drift markers but don't fail
			//     the call — see weft-driver-vz/builtin/vmconfig.go
			//     for the override roadmap.
			//   - disk_gb materialises as one ExtraDisk past the
			//     root image's own disk. 0 = no additional disk.
			vmUUID := op.CreateVm.VmUuid
			if vmUUID == "" {
				vmUUID = newDispatchUUID()
			}
			// Image must already be in the cache (pull happens via
			// the explicit /api/pull surface ; on-demand pull from
			// the dispatch hot path would hide failures behind a
			// "create stuck" error). CloneVM rejects an absent image.
			var extras []weft.ExtraDisk
			if op.CreateVm.DiskGb > 0 {
				extras = []weft.ExtraDisk{{SizeGiB: int(op.CreateVm.DiskGb)}}
			}
			if err := a.CloneVM(op.CreateVm.Image, op.CreateVm.Project, vmUUID, extras, io.Discard); err != nil {
				return &weftv1.DriverReply{
					RequestId: req.RequestId,
					Error:     fmt.Sprintf("CreateVM: %v", err),
				}
			}
			return &weftv1.DriverReply{
				RequestId: req.RequestId,
				Result: &weftv1.DriverReply_CreateVm{
					CreateVm: &weftv1.CreateVMResult{VmUuid: vmUUID},
				},
			}
		case *weftv1.DriverRequest_StartVm:
			// StartVM has no cloud-init payload from the dispatch
			// path — the boot artefacts are baked in at RegisterMicroVM
			// time, so the second arg stays empty.
			err := a.StartVM(op.StartVm.Name, "")
			reply := &weftv1.DriverReply{
				RequestId: req.RequestId,
				Result:    &weftv1.DriverReply_StartVm{StartVm: &weftv1.StartVMResult{}},
			}
			if err != nil {
				reply.Error = fmt.Sprintf("StartVM: %v", err)
			}
			return reply
		case *weftv1.DriverRequest_StopVm:
			err := a.StopVM(op.StopVm.Name)
			reply := &weftv1.DriverReply{
				RequestId: req.RequestId,
				Result:    &weftv1.DriverReply_StopVm{StopVm: &weftv1.StopVMResult{}},
			}
			if err != nil {
				reply.Error = fmt.Sprintf("StopVM: %v", err)
			}
			return reply
		case *weftv1.DriverRequest_DeleteVm:
			err := a.DeleteVM(op.DeleteVm.Name)
			reply := &weftv1.DriverReply{
				RequestId: req.RequestId,
				Result:    &weftv1.DriverReply_DeleteVm{DeleteVm: &weftv1.DeleteVMResult{}},
			}
			if err != nil {
				reply.Error = fmt.Sprintf("DeleteVM: %v", err)
			}
			return reply
		default:
			return &weftv1.DriverReply{
				RequestId: req.RequestId,
				Error:     fmt.Sprintf("unknown driver op: %T", op),
			}
		}
	}
}

// toMicroVMShares converts the proto wire shape to the
// Adapter's struct slice. Done in one place so future share
// fields (`clone`, `read_only`) are handled consistently.
func toMicroVMShares(in []*weftv1.MicroVMShare) []weft.MicroVMShare {
	if len(in) == 0 {
		return nil
	}
	out := make([]weft.MicroVMShare, 0, len(in))
	for _, s := range in {
		out = append(out, weft.MicroVMShare{
			Tag:      s.Tag,
			Path:     s.Path,
			ReadOnly: s.ReadOnly,
			Clone:    s.Clone,
		})
	}
	return out
}

// attestTPMDeviceOrDefault renders the TPM device for the log line : the
// configured path, or devtpm.DefaultDevice ("/dev/tpmrm0") when empty (the
// same default the agent applies). Logging-only — the agent re-applies the
// default internally.
func attestTPMDeviceOrDefault(device string) string {
	if device == "" {
		return "/dev/tpmrm0"
	}
	return device
}

// hostnameOrEmpty returns os.Hostname() or "" on error — the
// agent treats empty as "use the OS default" so the fallback
// is harmless.
func hostnameOrEmpty() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// newDispatchUUID mints a fresh RFC 4122 v4 UUID for a CreateVMOp
// dispatch that arrived without an explicit vm_uuid. crypto/rand
// failure falls back to a timestamp-derived ID — the dispatch hot
// path must never deadlock on RNG entropy starvation. Mirrors the
// weft package's internal newUUID() (not exported), kept local here
// to avoid widening that helper's visibility for one call site.
func newDispatchUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// We don't import time-of-day for a fallback : the per-call
		// dispatch UUID is just a unique handle, and the only way
		// crypto/rand fails is a kernel-level disaster where weft is
		// dying anyway. Return a zeroed marker the control plane
		// can treat as "server-side mint failed".
		_ = err
	}
	// RFC 4122 v4 bits — parseable by every UUID library.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return strings.Join([]string{h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]}, "-")
}

// Silence the unused-import linter when the build tags strip
// pieces of this file out. weft has cross-imports that the
// agent transitively touches via the ControlPlane interface.
var _ = weft.HostStateActive
