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
	"fmt"
	"os"
	"os/signal"
	"syscall"

	weft "github.com/openweft/weft"
	vzclient "github.com/openweft/weft-client"
	vzdv1 "github.com/openweft/weft-proto"
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
	conn, err := vzclient.Dial(t.controlPlaneURL)
	if err != nil {
		return fmt.Errorf("dial control plane %s: %w", t.controlPlaneURL, err)
	}
	defer conn.Close()

	cpClient := vzdv1.NewVzdServiceClient(conn)
	dispatchClient := vzdv1.NewAgentDispatchClient(conn)
	cp := agent.NewGRPCControlPlane(cpClient, logger)
	logger.Printf("weft agent --client: dialed control plane at %s", t.controlPlaneURL)

	// Build the agent. The driver Bundle is built per-host from
	// the local Apple-VZ binding ; same code as the all-in-one
	// path. StateDir reuses the daemon's <stateDir>/vms layout
	// so a node that flips between --client and all-in-one
	// preserves its VM state.
	a, err := agent.New(agent.Options{
		StateDir:     t.configDir, // operator can re-use existing dir
		Hostname:     hostnameOrEmpty(),
		AZ:           "", // operator can extend via --az flag in a follow-up
		Rack:         "",
		Endpoint:     "",
		ControlPlane: cp,
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
func buildDriverHandler(a weft.VZAdapter) func(context.Context, *vzdv1.DriverRequest) *vzdv1.DriverReply {
	return func(_ context.Context, req *vzdv1.DriverRequest) *vzdv1.DriverReply {
		switch op := req.Op.(type) {
		case *vzdv1.DriverRequest_RegisterMicroVm:
			err := a.RegisterMicroVM(op.RegisterMicroVm.Project, op.RegisterMicroVm.Name,
				weft.MicroVMBoot{
					BootISO: op.RegisterMicroVm.BootIso,
					Kernel:  op.RegisterMicroVm.Kernel,
					Initrd:  op.RegisterMicroVm.Initrd,
					Cmdline: op.RegisterMicroVm.Cmdline,
				},
				toMicroVMShares(op.RegisterMicroVm.Shares),
			)
			reply := &vzdv1.DriverReply{
				RequestId: req.RequestId,
				Result:    &vzdv1.DriverReply_RegisterMicroVm{RegisterMicroVm: &vzdv1.RegisterMicroVMResult{}},
			}
			if err != nil {
				reply.Error = fmt.Sprintf("RegisterMicroVM: %v", err)
			}
			return reply
		case *vzdv1.DriverRequest_CreateVm:
			// CreateVM dispatch lands when the Adapter's CreateVM
			// surface settles ; today it's a placeholder. Surfacing
			// the explicit "not implemented" keeps the control plane
			// from waiting on a reply that would never carry useful
			// data.
			return &vzdv1.DriverReply{
				RequestId: req.RequestId,
				Error:     "CreateVMOp dispatch is not yet wired on the agent",
			}
		case *vzdv1.DriverRequest_StartVm:
			// StartVM has no cloud-init payload from the dispatch
			// path — the boot artefacts are baked in at RegisterMicroVM
			// time, so the second arg stays empty.
			err := a.StartVM(op.StartVm.Name, "")
			reply := &vzdv1.DriverReply{
				RequestId: req.RequestId,
				Result:    &vzdv1.DriverReply_StartVm{StartVm: &vzdv1.StartVMResult{}},
			}
			if err != nil {
				reply.Error = fmt.Sprintf("StartVM: %v", err)
			}
			return reply
		case *vzdv1.DriverRequest_StopVm:
			err := a.StopVM(op.StopVm.Name)
			reply := &vzdv1.DriverReply{
				RequestId: req.RequestId,
				Result:    &vzdv1.DriverReply_StopVm{StopVm: &vzdv1.StopVMResult{}},
			}
			if err != nil {
				reply.Error = fmt.Sprintf("StopVM: %v", err)
			}
			return reply
		case *vzdv1.DriverRequest_DeleteVm:
			err := a.DeleteVM(op.DeleteVm.Name)
			reply := &vzdv1.DriverReply{
				RequestId: req.RequestId,
				Result:    &vzdv1.DriverReply_DeleteVm{DeleteVm: &vzdv1.DeleteVMResult{}},
			}
			if err != nil {
				reply.Error = fmt.Sprintf("DeleteVM: %v", err)
			}
			return reply
		default:
			return &vzdv1.DriverReply{
				RequestId: req.RequestId,
				Error:     fmt.Sprintf("unknown driver op: %T", op),
			}
		}
	}
}

// toMicroVMShares converts the proto wire shape to the
// Adapter's struct slice. Done in one place so future share
// fields (`clone`, `read_only`) are handled consistently.
func toMicroVMShares(in []*vzdv1.MicroVMShare) []weft.MicroVMShare {
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

// Silence the unused-import linter when the build tags strip
// pieces of this file out. weft has cross-imports that the
// agent transitively touches via the ControlPlane interface.
var _ = weft.HostStateActive
