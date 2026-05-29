// weft is the single binary for the Weft cloud platform. It runs in two
// modes selected by the subcommand : `weft agent` is the long-lived daemon
// (control-plane gRPC API + Apple-Virtualization driver dispatch on macOS
// hosts) ; `weft <noun> <verb>` (e.g. `weft project create`, `weft vm
// start`) talks to a running agent over the Unix socket. Same Consul-style
// model — one binary, role chosen by the subcommand.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	grubpkg "github.com/go-grub/grub"
	sshtransport "github.com/grpc-transports/ssh"
	cloudinit "github.com/openweft/cloud-init"
	imock "github.com/openweft/hclconfig"
	"github.com/openweft/weft"
	"github.com/openweft/weft-microvm-init/pkg/pod"
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/cmd/weft/admin"
	"github.com/openweft/weft/cmd/weft/clean"
	"github.com/openweft/weft/cmd/weft/events"
	"github.com/openweft/weft/cmd/weft/host"
	"github.com/openweft/weft/cmd/weft/image"
	"github.com/openweft/weft/cmd/weft/instance"
	"github.com/openweft/weft/cmd/weft/login"
	"github.com/openweft/weft/cmd/weft/microvm"
	"github.com/openweft/weft/cmd/weft/network"
	"github.com/openweft/weft/cmd/weft/overlaycmd"
	"github.com/openweft/weft/cmd/weft/project"
	"github.com/openweft/weft/cmd/weft/securitygroup"
	"github.com/openweft/weft/cmd/weft/share"
	"github.com/openweft/weft/cmd/weft/user"
	"github.com/openweft/weft/cmd/weft/volume"
	"github.com/openweft/weft/cmd/weft/wait"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// logger is the process-wide logger; messages go to stderr with timestamps.
var logger = log.New(os.Stderr, "", log.LstdFlags)

func main() {
	// The host-local datapath (the AppKit VM window via vz-vm-run, and
	// provision) moved into the weft-driver-vz plugin executable, so weft no
	// longer forks itself for VM display and needs no main-thread OS lock.
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// rootCmd builds the unified Weft cobra tree. The top-level binary is
// stateless ; daemon and client behaviour both live behind subcommands.
//
//	weft agent            — the long-lived control-plane daemon (Consul-style;
//	                        was the standalone vzd binary).
//	weft project / vm /   — gRPC clients that talk to a running agent over
//	  network / volume /    the Unix socket (formerly vzc subcommands).
//	  events / login / …
//	weft infra deploy /   — in-process orchestrator that boots etcd/dex/zot/
//	  bootstrap / status /  nats micro-VMs from infra/<svc>/plan.hcl. Stays
//	  validate              an Adapter-direct subcommand for chicken-and-egg
//	                        bootstraps (before the agent's gRPC is reachable).
//	weft vz-vm-run /      — hidden subcommands forked by the Apple-VZ driver
//	  vz-provision          for per-VM subprocesses.
func rootCmd() *cobra.Command {
	// Shared client-side connection flags (consumed by every client
	// subcommand below). Mirror the legacy `vzc` defaults so operator
	// muscle memory keeps working — same socket paths.
	home, _ := os.UserHomeDir()
	defaultSocket := filepath.Join(home, ".vzd", "vzd.sock")
	defaultSSHSocket := filepath.Join(home, ".vzd", "vzd-ssh.sock")

	var socketPath string
	var sshSocket string
	var sshKey string

	root := &cobra.Command{
		Use:   "weft",
		Short: "Weft — Go-native multi-hypervisor, multi-tenant, multi-AZ cloud platform",
		Long: `Weft is a single binary that runs in server or client mode depending on
the subcommand used (HashiCorp-style). "weft agent" boots the long-lived
control-plane daemon; "weft <noun> <verb>" issues client RPCs against a
running agent.`,
	}

	// Persistent flags consumed by client subcommands. Agent / infra /
	// vz-vm-run / vz-provision ignore these — they get their own flag set.
	root.PersistentFlags().StringVar(&socketPath, "socket", defaultSocket, "Weft agent Unix socket path (plain, no auth)")
	root.PersistentFlags().StringVar(&sshSocket, "ssh-socket", "", "Weft agent SSH socket path (enables SSH auth); default "+defaultSSHSocket+" when --ssh-key is set")
	root.PersistentFlags().StringVar(&sshKey, "ssh-key", "", "SSH private key for authentication (enables SSH transport)")

	// Server / daemon subcommand. Carries the formerly-cmd/vzd flags.
	root.AddCommand(agentCmd())

	// Bootstrap / out-of-band subcommands that drive the Adapter
	// in-process rather than via gRPC. These run before the agent is
	// reachable on some early-stage flows.
	// Host-local datapath subcommands that drive a hypervisor driver
	// in-process (e.g. the Apple-VZ vz-vm-run / vz-provision). Platform-gated:
	// darwin registers the VZ ones; linux is a no-op (QEMU needs no forked
	// runner). See main_darwin.go / main_linux.go.
	registerHostCommands(root)
	root.AddCommand(newInfraCmd())
	root.AddCommand(newUpCmd())

	// Client subcommands (was: vzc). All speak gRPC to the running agent.
	root.AddCommand(
		instance.Command(&socketPath, &sshSocket, &sshKey),
		microvm.Command(&socketPath, &sshSocket, &sshKey),
		image.Command(&socketPath, &sshSocket, &sshKey),
		project.Command(&socketPath, &sshSocket, &sshKey),
		volume.Command(&socketPath, &sshSocket, &sshKey),
		network.Command(&socketPath, &sshSocket, &sshKey),
		securitygroup.Command(&socketPath, &sshSocket, &sshKey),
		share.Command(&socketPath, &sshSocket, &sshKey),
		user.Command(&socketPath, &sshSocket, &sshKey),
		events.Command(&socketPath, &sshSocket, &sshKey),
		host.Command(&socketPath, &sshSocket, &sshKey),
		admin.Command(&socketPath, &sshSocket, &sshKey),
		clean.Command(&socketPath, &sshSocket, &sshKey),
		wait.Command(&socketPath, &sshSocket, &sshKey),
		login.LoginCommand(),
		login.LogoutCommand(),
		login.WhoamiCommand(),
		overlaycmd.Command(),
	)
	return root
}

// agentCmd is the Consul-style daemon subcommand. Same binary, role
// chosen by --server / --client flags :
//
//	weft agent             single-host all-in-one (server + local driver
//	                       dispatch, in-process). Default for a Mac laptop.
//	weft agent --server    control plane only (gRPC API + registries +
//	                       scheduler ; no driver dispatch).
//	weft agent --client    per-host driver runtime only (registers with a
//	                       remote control plane via --control-plane=URL).
//	                       Requires the gRPC ControlPlane client stub —
//	                       declared today, full enforcement lands when
//	                       the per-host gRPC transport is wired.
//
// Both flags can be set together — equivalent to the default no-flag mode.
func agentCmd() *cobra.Command {
	var cfgDir string
	var socketPath string
	var sshSocket string
	var sshAuthorizedKeys string
	var configFile string
	var oidcIssuer string
	var oidcClientID string
	var storageBackend string
	var eventBusBackend string
	var serverMode bool
	var clientMode bool
	var controlPlaneURL string
	var hypervisor string

	home, _ := os.UserHomeDir()
	defaultSocket := filepath.Join(home, ".vzd", "vzd.sock")
	defaultSSHSocket := filepath.Join(home, ".vzd", "vzd-ssh.sock")
	defaultAuthorizedKeys := filepath.Join(home, ".vzd", "authorized_keys")

	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run Weft as the long-lived control-plane daemon",
		Long: `Boots the gRPC server (plain Unix socket + optional SSH-secured Unix
socket), wires the storage backend (file or etcd), the event bus
(in-process or NATS), the OIDC validator (dex), and the driver dispatch
(Apple-VZ + future siblings).

Reads vzd.hcl from /etc/vzd/vzd.hcl or ~/.config/vzd/vzd.hcl by default ;
CLI flags override the file. The HCL config block, flag set, and default
paths are unchanged from the legacy "vzd" daemon for operational
continuity (same sockets, same registry on-disk layout).`,
		RunE: func(c *cobra.Command, _ []string) error {
			// Driver-backend selection. The Adapter reads $WEFT_HYPERVISOR
			// in initLocalDrivers (which runs eagerly in weft.New), so the
			// flag bridges to the env before the Adapter is constructed.
			// "qemu" boots VMs via QEMU/TCG — usable where Apple VZ can't
			// nest (non-nested dev VM).
			if hypervisor != "" {
				_ = os.Setenv("WEFT_HYPERVISOR", hypervisor)
			}
			// HCL config (optional) supplies defaults for any flag
			// the operator did not explicitly pass on the command
			// line — CLI > HCL > built-in default.
			fc, fcPath, err := loadFileConfig(configFile)
			if err != nil {
				return err
			}
			tgt := fileConfigTargets{
				socket:            socketPath,
				sshSocket:         sshSocket,
				sshAuthorizedKeys: sshAuthorizedKeys,
				configDir:         cfgDir,
				oidcIssuer:        oidcIssuer,
				oidcClientID:      oidcClientID,
				storageBackend:    storageBackend,
				eventBusBackend:   eventBusBackend,
			}
			before := tgt
			applyFileConfigDefaults(fc, &tgt)
			if c.Flags().Changed("socket") {
				tgt.socket = before.socket
			}
			if c.Flags().Changed("ssh-socket") {
				tgt.sshSocket = before.sshSocket
			}
			if c.Flags().Changed("ssh-authorized-keys") {
				tgt.sshAuthorizedKeys = before.sshAuthorizedKeys
			}
			if c.Flags().Changed("config-dir") {
				tgt.configDir = before.configDir
			}
			if c.Flags().Changed("oidc-issuer") {
				tgt.oidcIssuer = before.oidcIssuer
			}
			if c.Flags().Changed("oidc-client-id") {
				tgt.oidcClientID = before.oidcClientID
			}
			if c.Flags().Changed("storage-backend") {
				tgt.storageBackend = before.storageBackend
			}
			if c.Flags().Changed("event-bus") {
				tgt.eventBusBackend = before.eventBusBackend
			}
			if fcPath != "" {
				logger.Printf("loaded config %s", fcPath)
			}
			// Consul-style role selection.
			//   default (no flags) — single-host all-in-one
			//   --server                — control plane only
			//   --client + --control-plane=URL — per-host runtime
			//                                    that dials a
			//                                    remote control
			//                                    plane via gRPC.
			tgt.serverMode = serverMode
			tgt.clientMode = clientMode
			tgt.controlPlaneURL = controlPlaneURL
			if clientMode && controlPlaneURL != "" {
				return runClient(tgt)
			}
			if clientMode && controlPlaneURL == "" {
				logger.Printf("weft agent: --client without --control-plane=URL ; running all-in-one")
			}
			return run(tgt)
		},
	}

	cmd.Flags().StringVar(&configFile, "config", "", "Path to vzd.hcl (default: /etc/vzd/vzd.hcl or ~/.config/vzd/vzd.hcl)")
	cmd.Flags().StringVar(&cfgDir, "config-dir", ".mock/hcl", "Path to HCL config directory")
	cmd.Flags().StringVar(&socketPath, "socket", defaultSocket, "Unix socket path to listen on")
	cmd.Flags().StringVar(&sshSocket, "ssh-socket", defaultSSHSocket, "Unix socket path for the SSH-secured gRPC listener (empty to disable)")
	cmd.Flags().StringVar(&sshAuthorizedKeys, "ssh-authorized-keys", defaultAuthorizedKeys, "Path to authorized_keys for SSH client authentication")
	cmd.Flags().StringVar(&oidcIssuer, "oidc-issuer", "", "OIDC issuer URL (empty = dev mode, no token validation)")
	cmd.Flags().StringVar(&oidcClientID, "oidc-client-id", "", "OIDC audience that tokens must be issued for")
	cmd.Flags().StringVar(&storageBackend, "storage-backend", "", `Registry persistence backend: "file" (dev, local disk) or "etcd" (prod, 3-DC cluster). Empty = HCL config decides; HCL empty = "file".`)
	cmd.Flags().StringVar(&eventBusBackend, "event-bus", "", `Event-bus backend: "local" (dev, in-process channels) or "nats" (prod, 3-DC cluster). Empty = HCL config decides; HCL empty = "local".`)
	cmd.Flags().StringVar(&hypervisor, "hypervisor", "", `Local hypervisor driver: "" / "apple-vz" (default) or "qemu" (QEMU/TCG — pure emulation, works without nested virt).`)
	cmd.Flags().BoolVar(&serverMode, "server", false, "Run as control-plane server (no per-host driver dispatch). Default mode includes both.")
	cmd.Flags().BoolVar(&clientMode, "client", false, "Run as per-host driver runtime only. Requires --control-plane to point at the server.")
	cmd.Flags().StringVar(&controlPlaneURL, "control-plane", "", "URL of the Weft control-plane server (only consulted when --client is set).")

	return cmd
}

func run(t fileConfigTargets) error {
	mc := imock.LoadMockBlock(t.configDir)

	// Storage factory selects which Storage backend every registry
	// (projects, users, networks, volumes, …) goes through. Default
	// is "file" (local disk under <vmsDir>/.<name>.hcl); "etcd"
	// switches to a 3-DC etcd cluster per [[etcd-control-plane]].
	// CLI flag > HCL > built-in "file" default.
	sf, err := buildStorageFactory(t)
	if err != nil {
		return fmt.Errorf("build storage factory: %w", err)
	}
	if sf.close != nil {
		defer func() { _ = sf.close() }()
	}
	a := weft.NewWithStorage(filepath.Dir(t.configDir), sf.new)
	a.SetPaths(mc.CachePath, mc.VMsPath)

	// Event bus backend (per [[vzd-event-bus-nats]]). Default is
	// LocalEventBus (in-process channels); "nats" connects to the
	// cluster URL configured in HCL. Failing fast on NATS dial
	// is intentional — silently degrading to local would surprise
	// the operator at the worst time.
	bf, err := buildEventBus(t)
	if err != nil {
		return fmt.Errorf("build event bus: %w", err)
	}
	a.SetEventBus(bf.bus)
	if bf.close != nil {
		defer func() { _ = bf.close() }()
	}

	// Auto-render the NATS authorization block on every project
	// mutation when the operator configured `nats_authorization {
	// path = … }` in vzd.hcl. The hook is a no-op when path is
	// empty; the renderer stays callable via `vzc admin nats-authz`.
	// Per [[vzd-tenant-event-access]] Phase-5 follow-up.
	if t.natsAuthzPath != "" {
		a.SetNATSAuthorizationFile(t.natsAuthzPath, t.natsAuthzAdminPubkey)
		logger.Printf("nats authorization auto-render enabled: path=%s", t.natsAuthzPath)
	}

	logger.Printf("vzd starting — config-dir=%s socket=%s storage=%s event_bus=%s",
		t.configDir, t.socket,
		displayStorageBackend(t.storageBackend),
		displayEventBusBackend(t.eventBusBackend))

	// OIDC validator (per [[oidc-server-dex]]). Empty issuer leaves
	// the validator nil → interceptors run in dev mode and attach a
	// synthetic `dev:<os-user>` Caller to every request.
	validator, err := weft.NewOIDCValidator(context.Background(), weft.OIDCConfig{
		Issuer:            t.oidcIssuer,
		ClientID:          t.oidcClientID,
		SkipClientIDCheck: t.oidcSkipClientIDCheck,
	})
	if err != nil {
		return fmt.Errorf("init OIDC validator: %w", err)
	}
	if validator != nil {
		logger.Printf("OIDC enabled — issuer=%s client_id=%q", validator.Issuer(), t.oidcClientID)
	} else {
		logger.Printf("OIDC disabled (dev mode) — callers synthesised as dev:<os-user>")
	}

	if err := os.MkdirAll(filepath.Dir(t.socket), 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	_ = os.Remove(t.socket)

	lis, err := net.Listen("unix", t.socket)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", t.socket, err)
	}

	srv := grpc.NewServer(
		grpc.UnaryInterceptor(weft.UnaryAuthInterceptor(validator, userPersister(a))),
		grpc.StreamInterceptor(weft.StreamAuthInterceptor(validator, userPersister(a))),
	)
	// Share one agentDispatchServer between the WeftAgent
	// handlers (so RegisterMicroVM-and-friends can dispatch to
	// remote hosts) and the AgentDispatch service (so connecting
	// agents register their streams in the same registry).
	dispatchSrv := newAgentDispatchServer()
	// When a dispatch session genuinely ends (stream dropped or
	// liveness sweep killed it — NOT a fresh reconnect that
	// supersedes the old session), demote the host so the
	// scheduler stops picking it. The agent's next RegisterHost
	// re-promotes it back to Active automatically.
	dispatchSrv.onSessionDown = func(hostUUID string) {
		if err := a.SetHostState(hostUUID, weft.HostStateDown); err != nil {
			logger.Printf("agent-dispatch: demote host %s to down: %v", hostUUID, err)
		}
	}
	// Cluster-wide compute-envelope catalogue. Loaded once at
	// startup via the adapter's registry-storage factory (file in
	// single-host dev, etcd in HA when embed.Etcd lands). A failure
	// to load is fatal — the dashboard's CreateVMModal can't work
	// without flavors, and silently starting with an empty
	// catalogue would mask an operator hand-edit gone wrong.
	flavorReg, err := weft.LoadFlavorRegistry(context.Background(), a.RegistryStorage("flavors"))
	if err != nil {
		return fmt.Errorf("load flavor registry: %w", err)
	}
	scriptReg, err := weft.LoadScriptRegistry(context.Background(), a.RegistryStorage("scripts"))
	if err != nil {
		return fmt.Errorf("load script registry: %w", err)
	}
	vzdv1.RegisterWeftAgentServer(srv, &vzdServer{
		cfgDir:        t.configDir,
		mc:            mc,
		adp:           a,
		dispatch:      dispatchSrv,
		localHostUUID: localHostUUID(a),
		flavors:       flavorReg,
		scripts:       scriptReg,
	})
	vzdv1.RegisterAgentDispatchServer(srv, dispatchSrv)

	go func() {
		if err := srv.Serve(lis); err != nil {
			logger.Printf("grpc server stopped: %v", err)
		}
	}()
	logger.Printf("listening on %s", t.socket)

	// Optional SSH-secured listener — same gRPC server (and same
	// auth interceptors), different transport.
	if t.sshSocket != "" {
		home, _ := os.UserHomeDir()
		_ = os.Remove(t.sshSocket)
		sshLis, err := sshtransport.ListenSSH("unix:"+t.sshSocket, sshtransport.ServerConfig{
			HostKeyPath:        filepath.Join(home, ".vzd", "vzd_host_key"),
			AuthorizedKeysPath: t.sshAuthorizedKeys,
			Logger:             logger,
		})
		if err != nil {
			return fmt.Errorf("ssh listener: %w", err)
		}
		go func() {
			if err := srv.Serve(sshLis); err != nil {
				logger.Printf("ssh grpc server stopped: %v", err)
			}
		}()
		logger.Printf("SSH gRPC listening on %s", t.sshSocket)
	}

	select {}
}

// ---- gRPC server -----------------------------------------------------------

type vzdServer struct {
	vzdv1.UnimplementedWeftAgentServer
	cfgDir string
	mc     imock.MockBlock
	adp    weft.VZAdapter
	// dispatch is the per-host AgentDispatch stream registry.
	// Server-side RPCs that target a specific Host (via the
	// scheduler) route through it instead of calling the local
	// Adapter directly. Set by run() at startup ; tests can
	// leave it nil to force the all-local path.
	dispatch *agentDispatchServer
	// localHostUUID is read once at startup from the host-uuid
	// file. Empty when the server isn't running with a
	// self-registered local host (e.g. integration tests). Used
	// to decide local-vs-remote in RegisterMicroVM dispatch.
	localHostUUID string
	// flavors is the cluster-wide compute-envelope catalogue.
	// Constructed at startup against the configured Storage (file
	// in single-host dev, etcd in HA). nil in tests that don't
	// need it — every RPC checks before deref.
	flavors *weft.FlavorRegistry
	// scripts is the cluster-wide provisioning-script catalogue.
	// Same shape as flavors. nil in tests ; RPCs guard via
	// scriptsReady.
	scripts *weft.ScriptRegistry
}

func (s *vzdServer) ListVMs(ctx context.Context, req *vzdv1.ListVMsRequest) (*vzdv1.ListVMsResponse, error) {
	visible, all, err := s.adp.VisibleProjects(ctx)
	if err != nil {
		return nil, err
	}
	localMap, err := s.adp.ListLocal()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list vms: %v", err)
	}
	// Phase 1: no auth — `project == ""` means "every VM"; otherwise
	// return only the requested project. Phase 2 will instead scope
	// the empty case to the caller's projects.
	wantProject := req.Project
	var vms []*vzdv1.VMInfo
	for _, props := range localMap {
		project, _ := props["project"].(string)
		projectUUID, _ := props["project_uuid"].(string)
		if wantProject != "" && project != wantProject && projectUUID != wantProject {
			continue
		}
		// ACL filter: when the caller isn't unscoped (dev / admin),
		// only let their own visible projects through. Same set
		// `--project=…` filter already narrowed against.
		if !all {
			if _, ok := visible[projectUUID]; !ok {
				continue
			}
		}
		name, _ := props["name"].(string)
		vmState := "stopped"
		if running, _ := props["Running"].(bool); running {
			vmState = "running"
		}
		ip, _ := s.adp.IP(name)
		info := &vzdv1.VMInfo{
			Name:        name,
			State:       stateToProto(vmState),
			Ip:          ip,
			Project:     project,
			ProjectUuid: projectUUID,
		}
		if v, ok := props["cpu"].(float64); ok {
			info.Cpu = uint32(v)
		}
		if v, ok := props["mem_mb"].(float64); ok {
			info.MemMb = uint64(v)
		}
		if v, ok := props["disk_gb"].(float64); ok {
			info.DiskGb = uint64(v)
		}
		if v, ok := props["image"].(string); ok {
			info.Image = v
		}
		if v, ok := props["os"].(string); ok {
			info.Os = v
		}
		vms = append(vms, info)
	}
	return &vzdv1.ListVMsResponse{Vms: vms}, nil
}

func (s *vzdServer) VMStatus(ctx context.Context, req *vzdv1.VMStatusRequest) (*vzdv1.VMStatusResponse, error) {
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	localMap, err := s.adp.ListLocal()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list vms: %v", err)
	}
	// Find the VM by (project, name). When project is empty, accept
	// the first hit. When project is set (display name OR UUID),
	// only return a match from that project — preserves the
	// multi-project name-reuse invariant.
	var props map[string]interface{}
	for _, p := range localMap {
		gotName, _ := p["name"].(string)
		if gotName != req.Name {
			continue
		}
		if req.Project != "" {
			gotProject, _ := p["project"].(string)
			gotUUID, _ := p["project_uuid"].(string)
			if gotProject != req.Project && gotUUID != req.Project {
				continue
			}
		}
		props = p
		break
	}
	if props == nil {
		if req.Project != "" {
			return nil, status.Errorf(codes.NotFound, "vm %q not found in project %q", req.Name, req.Project)
		}
		return nil, status.Errorf(codes.NotFound, "vm %q not found", req.Name)
	}
	vmState := "stopped"
	if running, _ := props["Running"].(bool); running {
		vmState = "running"
	}
	ip, _ := s.adp.IP(req.Name)
	project, _ := props["project"].(string)
	projectUUID, _ := props["project_uuid"].(string)
	info := &vzdv1.VMInfo{
		Name:        req.Name,
		State:       stateToProto(vmState),
		Ip:          ip,
		Project:     project,
		ProjectUuid: projectUUID,
	}
	// Populate CPU/Mem/Disk/OS from config.json if available.
	if v, ok := props["cpu"].(float64); ok {
		info.Cpu = uint32(v)
	}
	if v, ok := props["mem_mb"].(float64); ok {
		info.MemMb = uint64(v)
	}
	if v, ok := props["disk_gb"].(float64); ok {
		info.DiskGb = uint64(v)
	}
	if v, ok := props["image"].(string); ok {
		info.Image = v
	}
	if v, ok := props["os"].(string); ok {
		info.Os = v
	}
	return &vzdv1.VMStatusResponse{Vm: info}, nil
}

func (s *vzdServer) StartVM(ctx context.Context, req *vzdv1.StartVMRequest) (*vzdv1.StartVMResponse, error) {
	logger.Printf("StartVM name=%s project=%s host=%s", req.Name, req.Project, req.HostUuid)
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	if s.shouldDispatch(req.HostUuid) {
		return s.dispatchStartVM(ctx, req)
	}
	if err := s.adp.StartVM(req.Name, ""); err != nil {
		logger.Printf("StartVM %s: error: %v", req.Name, err)
		return nil, status.Errorf(codes.Internal, "start vm: %v", err)
	}
	return &vzdv1.StartVMResponse{}, nil
}

func (s *vzdServer) StopVM(ctx context.Context, req *vzdv1.StopVMRequest) (*vzdv1.StopVMResponse, error) {
	logger.Printf("StopVM name=%s project=%s host=%s", req.Name, req.Project, req.HostUuid)
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	if s.shouldDispatch(req.HostUuid) {
		return s.dispatchStopVM(ctx, req)
	}
	if err := s.adp.StopVM(req.Name); err != nil {
		logger.Printf("StopVM %s: error: %v", req.Name, err)
		return nil, status.Errorf(codes.Internal, "stop vm: %v", err)
	}
	return &vzdv1.StopVMResponse{}, nil
}

func (s *vzdServer) CreateVM(ctx context.Context, req *vzdv1.CreateVMRequest) (*vzdv1.CreateVMResponse, error) {
	logger.Printf("CreateVM name=%s image=%s project=%s", req.Name, req.Image, req.Project)
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	if err := s.adp.CloneVM(req.Image, req.Project, req.Name, nil, io.Discard); err != nil {
		logger.Printf("CreateVM %s: error: %v", req.Name, err)
		return nil, status.Errorf(codes.Internal, "create vm: %v", err)
	}
	memGiB := req.MemMb / 1024
	if memGiB == 0 && req.MemMb > 0 {
		memGiB = 1
	}
	if err := enrichVMConfig(s.adp.VMDir(req.Name), map[string]interface{}{
		"cpu":     req.Cpu,
		"mem_mb":  req.MemMb,
		"mem_gib": memGiB,
		"disk_gb": req.DiskGb,
	}); err != nil {
		logger.Printf("CreateVM %s: enrich config: %v", req.Name, err)
	}
	return &vzdv1.CreateVMResponse{}, nil
}

func (s *vzdServer) DeleteVM(ctx context.Context, req *vzdv1.DeleteVMRequest) (*vzdv1.DeleteVMResponse, error) {
	logger.Printf("DeleteVM name=%s project=%s host=%s", req.Name, req.Project, req.HostUuid)
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	if s.shouldDispatch(req.HostUuid) {
		return s.dispatchDeleteVM(ctx, req)
	}
	if err := s.adp.DeleteVM(req.Name); err != nil {
		logger.Printf("DeleteVM %s: error: %v", req.Name, err)
		return nil, status.Errorf(codes.Internal, "delete vm: %v", err)
	}
	return &vzdv1.DeleteVMResponse{}, nil
}

// ProvisionVM clones the image, injects a cloud-init ISO if an SSH public key
// is provided, starts the VM and waits up to 3 minutes for an IP address.
func (s *vzdServer) ProvisionVM(ctx context.Context, req *vzdv1.ProvisionVMRequest) (*vzdv1.ProvisionVMResponse, error) {
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	logger.Printf("ProvisionVM name=%s image=%s cpu=%d mem_mb=%d disk_gb=%d project=%s", req.Name, req.Image, req.Cpu, req.MemMb, req.DiskGb, req.Project)
	var cloneOut bytes.Buffer
	if err := s.adp.CloneVM(req.Image, req.Project, req.Name, nil, &cloneOut); err != nil {
		logger.Printf("ProvisionVM %s: clone output: %s", req.Name, cloneOut.String())
		return nil, status.Errorf(codes.Internal, "clone vm: %v", err)
	}
	if out := cloneOut.String(); out != "" {
		logger.Printf("ProvisionVM %s: clone output: %s", req.Name, out)
	}

	// Apply disk file operations (add + delete) before first boot.
	if len(req.FileOps) > 0 {
		diskPath := s.adp.DiskPath(req.Name)
		if err := applyDiskFileOps(diskPath, req.FileOps); err != nil {
			return nil, status.Errorf(codes.Internal, "disk file ops: %v", err)
		}
		logger.Printf("ProvisionVM %s: applied %d disk file op(s)", req.Name, len(req.FileOps))
	}
	if len(req.DeleteOps) > 0 {
		diskPath := s.adp.DiskPath(req.Name)
		if err := deleteDiskFileOps(diskPath, req.DeleteOps); err != nil {
			return nil, status.Errorf(codes.Internal, "disk delete ops: %v", err)
		}
		logger.Printf("ProvisionVM %s: deleted %d path(s) from disk", req.Name, len(req.DeleteOps))
	}
	if len(req.ModOps) > 0 {
		diskPath := s.adp.DiskPath(req.Name)
		if err := modDiskFileOps(diskPath, req.ModOps); err != nil {
			return nil, status.Errorf(codes.Internal, "disk mod ops: %v", err)
		}
		logger.Printf("ProvisionVM %s: applied %d mod op(s) to disk", req.Name, len(req.ModOps))
	}
	// Enrich config.json with CPU/mem/disk so VMStatus can return them.
	// mem_gib is written alongside mem_mb so runvm.go (which reads mem_gib) uses
	// the requested value instead of its hardcoded 2 GiB default.
	memGiB := req.MemMb / 1024
	if memGiB == 0 && req.MemMb > 0 {
		memGiB = 1
	}
	if err := enrichVMConfig(s.adp.VMDir(req.Name), map[string]interface{}{
		"cpu":     req.Cpu,
		"mem_mb":  req.MemMb,
		"mem_gib": memGiB,
		"disk_gb": req.DiskGb,
	}); err != nil {
		logger.Printf("ProvisionVM %s: enrich config: %v", req.Name, err)
	}

	cloudInitISO := ""
	if req.SshPubKey != "" {
		userData := cloudinit.BuildSSHCloudConfig([]string{req.SshPubKey}, "")
		h := sha256.Sum256([]byte(userData))
		instanceID := fmt.Sprintf("%s-%x", req.Name, h[:4])
		isoData, err := cloudinit.BuildCloudInitISO(instanceID, req.Name, userData)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "build cloud-init ISO: %v", err)
		}
		isoPath, err := s.adp.WriteCloudInitISO(req.Name, isoData)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "write cloud-init ISO: %v", err)
		}
		cloudInitISO = isoPath
	}

	if err := s.adp.StartVM(req.Name, cloudInitISO); err != nil {
		logger.Printf("ProvisionVM %s: start error: %v", req.Name, err)
		return nil, status.Errorf(codes.Internal, "start vm: %v", err)
	}
	logger.Printf("ProvisionVM %s: VM started, cloudInitISO=%q", req.Name, cloudInitISO)

	logger.Printf("ProvisionVM %s: waiting for IP (timeout=3m)", req.Name)
	ip, err := waitForIPVerbose(s.adp, req.Name, 3*time.Minute)
	if err != nil {
		return nil, status.Errorf(codes.DeadlineExceeded, "wait for ip: %v", err)
	}
	logger.Printf("ProvisionVM %s: ready ip=%s", req.Name, ip)
	return &vzdv1.ProvisionVMResponse{}, nil
}

// DeprovisionVM stops (best-effort) then deletes a VM.
func (s *vzdServer) DeprovisionVM(ctx context.Context, req *vzdv1.DeprovisionVMRequest) (*vzdv1.DeprovisionVMResponse, error) {
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	logger.Printf("DeprovisionVM name=%s", req.Name)
	_ = s.adp.StopVM(req.Name) // best-effort; ignore error
	if err := s.adp.DeleteVM(req.Name); err != nil {
		return nil, status.Errorf(codes.Internal, "delete vm: %v", err)
	}
	return &vzdv1.DeprovisionVMResponse{}, nil
}

// PullImages pulls all images referenced in the HCL config directory.
func (s *vzdServer) PullImages(ctx context.Context, req *vzdv1.PullImagesRequest) (*vzdv1.PullImagesResponse, error) {
	cfgDir := req.ConfigDir
	if cfgDir == "" {
		cfgDir = s.cfgDir
	}
	logger.Printf("PullImages config-dir=%s parallel=%d", cfgDir, req.Parallel)
	ociMap := imock.LoadOCIFroms(cfgDir)
	imgSet := map[string]struct{}{}
	for _, v := range ociMap {
		if v != "" {
			imgSet[v] = struct{}{}
		}
	}
	rows, err := imock.BuildRowsFromConfig(cfgDir, "", map[string]map[string]interface{}{}, ociMap)
	if err == nil {
		for _, r := range rows {
			if r.Image != "" {
				imgSet[r.Image] = struct{}{}
			}
		}
	}
	imgs := make([]string, 0, len(imgSet))
	for img := range imgSet {
		imgs = append(imgs, img)
	}
	if len(imgs) == 0 {
		return &vzdv1.PullImagesResponse{}, nil
	}
	parallel := int(req.Parallel)
	if parallel <= 0 {
		parallel = 4
	}
	if err := s.adp.Pull(ctx, imgs, parallel); err != nil {
		logger.Printf("PullImages: error: %v", err)
		return nil, status.Errorf(codes.Internal, "pull images: %v", err)
	}
	logger.Printf("PullImages: done (%d images)", len(imgs))
	return &vzdv1.PullImagesResponse{}, nil
}

// PullImage pulls a single image by URL.
func (s *vzdServer) PullImage(ctx context.Context, req *vzdv1.PullImageRequest) (*vzdv1.PullImageResponse, error) {
	if req.Url == "" {
		return nil, status.Error(codes.InvalidArgument, "url is required")
	}
	logger.Printf("PullImage url=%s", req.Url)
	if err := s.adp.Pull(ctx, []string{req.Url}, 1); err != nil {
		logger.Printf("PullImage: error: %v", err)
		return nil, status.Errorf(codes.Internal, "pull image: %v", err)
	}
	logger.Printf("PullImage: done")
	return &vzdv1.PullImageResponse{}, nil
}

// PatchImage applies DiskFileOps to a cached image so that all VMs cloned
// from it inherit the patches without needing per-instance copy blocks.
func (s *vzdServer) PatchImage(_ context.Context, req *vzdv1.PatchImageRequest) (*vzdv1.PatchImageResponse, error) {
	if req.Url == "" {
		return nil, status.Error(codes.InvalidArgument, "url is required")
	}
	if len(req.FileOps) == 0 && len(req.DeleteOps) == 0 && len(req.ModOps) == 0 {
		return &vzdv1.PatchImageResponse{}, nil
	}
	logger.Printf("PatchImage url=%s add=%d del=%d mod=%d", req.Url, len(req.FileOps), len(req.DeleteOps), len(req.ModOps))
	cachedPath, err := s.adp.CachedImagePath(req.Url)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "patch image: %v", err)
	}
	if err := applyDiskFileOps(cachedPath, req.FileOps); err != nil {
		logger.Printf("PatchImage: error applying file ops: %v", err)
		return nil, status.Errorf(codes.Internal, "patch image: %v", err)
	}
	if err := deleteDiskFileOps(cachedPath, req.DeleteOps); err != nil {
		logger.Printf("PatchImage: error applying delete ops: %v", err)
		return nil, status.Errorf(codes.Internal, "patch image delete: %v", err)
	}
	if err := modDiskFileOps(cachedPath, req.ModOps); err != nil {
		logger.Printf("PatchImage: error applying mod ops: %v", err)
		return nil, status.Errorf(codes.Internal, "patch image mod: %v", err)
	}
	logger.Printf("PatchImage: done")
	return &vzdv1.PatchImageResponse{}, nil
}

// ListImages returns all locally cached images.
func (s *vzdServer) ListImages(_ context.Context, _ *vzdv1.ListImagesRequest) (*vzdv1.ListImagesResponse, error) {
	images, err := s.adp.ListCachedImages()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list cached images: %v", err)
	}
	var infos []*vzdv1.ImageInfo
	for _, img := range images {
		infos = append(infos, &vzdv1.ImageInfo{
			Url:       img.URL(),
			Name:      img.Name(),
			Format:    img.Format(),
			SizeBytes: img.SizeBytes(),
		})
	}
	return &vzdv1.ListImagesResponse{Images: infos}, nil
}

// CleanImages removes cached images referenced in the HCL config.
func (s *vzdServer) CleanImages(_ context.Context, req *vzdv1.CleanImagesRequest) (*vzdv1.CleanImagesResponse, error) {
	cfgDir := req.ConfigDir
	if cfgDir == "" {
		cfgDir = s.cfgDir
	}
	ociMap := imock.LoadOCIFroms(cfgDir)
	imageSet := map[string]struct{}{}
	for _, v := range ociMap {
		if v != "" {
			imageSet[v] = struct{}{}
		}
	}
	items, err := s.adp.ListOCI()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list oci: %v", err)
	}
	var toDelete []string
	for _, it := range items {
		src, _ := it["source"].(string)
		name, _ := it["name"].(string)
		for img := range imageSet {
			if src == img || name == img {
				toDelete = append(toDelete, name)
				break
			}
		}
	}
	if req.DryRun {
		return &vzdv1.CleanImagesResponse{Deleted: toDelete}, nil
	}
	var deleted []string
	for _, n := range toDelete {
		if err := s.adp.DeleteOCI(n); err == nil {
			deleted = append(deleted, n)
		}
	}
	return &vzdv1.CleanImagesResponse{Deleted: deleted}, nil
}

// RegisterMicroVM wires a VM directory for a microVM-style boot:
// the primary storage device is a read-only ncl-init UKI ISO, and
// the guest sees one or more virtio-fs shares carrying the OCI
// image rootfs(es). The actual VM creation happens through
// adapter.RegisterMicroVM (added in the same change as this RPC);
// once it returns the VM behaves like any other vzd VM and is
// started/stopped via the normal StartVM/StopVM RPCs.
func (s *vzdServer) RegisterMicroVM(ctx context.Context, req *vzdv1.RegisterMicroVMRequest) (*vzdv1.RegisterMicroVMResponse, error) {
	logger.Printf("RegisterMicroVM name=%s project=%s boot_iso=%s kernel=%s initrd=%s cmdline=%q shares=%d",
		req.Name, req.Project, req.BootIso, req.Kernel, req.Initrd, req.Cmdline, len(req.Shares))
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	shares := make([]weft.MicroVMShare, len(req.Shares))
	for i, s := range req.Shares {
		shares[i] = weft.MicroVMShare{
			Tag:      s.Tag,
			Path:     s.Path,
			ReadOnly: s.ReadOnly,
			Clone:    s.Clone,
		}
	}
	boot := weft.MicroVMBoot{
		BootISO: req.BootIso,
		Kernel:  req.Kernel,
		Initrd:  req.Initrd,
		Cmdline: req.Cmdline,
	}
	// Multi-host dispatch : when req.HostUuid is set and refers
	// to a remote host (i.e. one that has a connected `weft
	// agent --client` stream), route the RegisterMicroVM op
	// over the AgentDispatch stream instead of running it
	// locally. Empty / matching-local stays on the in-process
	// path — the Mac-laptop / single-host default.
	if s.shouldDispatch(req.HostUuid) {
		return s.dispatchRegisterMicroVM(ctx, req, boot, shares)
	}
	if err := s.adp.RegisterMicroVM(req.Project, req.Name, boot, shares); err != nil {
		logger.Printf("RegisterMicroVM %s: error: %v", req.Name, err)
		return nil, status.Errorf(codes.Internal, "register microvm: %v", err)
	}
	return &vzdv1.RegisterMicroVMResponse{}, nil
}

// shareAttacher is the adapter capability PublishShareToProject needs.
// Kept as a narrow local interface (rather than widening VZAdapter) so a
// mock adapter without share support still satisfies the server.
type shareAttacher interface {
	AttachShareToProject(projectUUID string, m pod.ShareMount) (int, error)
}

// PublishShareToProject resolves the project's VMs and fans the share mount
// (or unmount) out to each over the event bus. The control plane reaches
// every VM regardless of host via NATS, so there's no per-host dispatch.
func (s *vzdServer) PublishShareToProject(ctx context.Context, req *vzdv1.PublishShareToProjectRequest) (*vzdv1.PublishShareToProjectResponse, error) {
	if req.Mount == nil {
		return nil, status.Errorf(codes.InvalidArgument, "mount is required")
	}
	projUUID, err := s.adp.AuthorizeProject(ctx, req.ProjectUuid)
	if err != nil {
		return nil, err
	}
	a, ok := s.adp.(shareAttacher)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "share fan-out unsupported by this adapter")
	}
	m := pod.ShareMount{
		ID:         req.Mount.Id,
		Action:     pod.MountAction(req.Mount.Action),
		Backend:    req.Mount.Backend,
		MountPoint: req.Mount.MountPoint,
		Readonly:   req.Mount.Readonly,
	}
	if c := req.Mount.Cubefs; c != nil {
		m.CubeFS = &pod.CubeFSMount{
			Volume:    c.Volume,
			Masters:   c.Masters,
			Owner:     c.Owner,
			AccessKey: c.AccessKey,
			SecretKey: c.SecretKey,
			SubDir:    c.Subdir,
		}
	}
	n, err := a.AttachShareToProject(projUUID, m)
	if err != nil {
		logger.Printf("PublishShareToProject project=%s share=%s: error: %v", req.ProjectUuid, m.ID, err)
		return nil, status.Errorf(codes.Internal, "publish share: %v", err)
	}
	logger.Printf("PublishShareToProject project=%s share=%s action=%q -> %d VMs", req.ProjectUuid, m.ID, m.Action, n)
	return &vzdv1.PublishShareToProjectResponse{VmCount: uint32(n)}, nil
}

// shouldDispatch reports whether a RegisterMicroVM (or any
// future host-pinned op) should route through the dispatch
// stream rather than run in-process. Returns false for empty
// host_uuid, a self-target, or a server without a configured
// dispatch registry — all of those fall through to the local
// Adapter call.
func (s *vzdServer) shouldDispatch(hostUUID string) bool {
	if s.dispatch == nil || hostUUID == "" {
		return false
	}
	if s.localHostUUID != "" && hostUUID == s.localHostUUID {
		return false
	}
	return true
}

// dispatchRegisterMicroVM marshals the op + sends it through
// the AgentDispatch stream for the target host. Returns the
// gRPC-level Response on success ; the agent's typed error (if
// any) is surfaced as codes.Internal so callers see the agent's
// message verbatim.
func (s *vzdServer) dispatchRegisterMicroVM(
	ctx context.Context,
	req *vzdv1.RegisterMicroVMRequest,
	boot weft.MicroVMBoot,
	shares []weft.MicroVMShare,
) (*vzdv1.RegisterMicroVMResponse, error) {
	wireShares := make([]*vzdv1.MicroVMShare, len(shares))
	for i, sh := range shares {
		wireShares[i] = &vzdv1.MicroVMShare{
			Tag:      sh.Tag,
			Path:     sh.Path,
			ReadOnly: sh.ReadOnly,
			Clone:    sh.Clone,
		}
	}
	op := &vzdv1.DriverRequest{Op: &vzdv1.DriverRequest_RegisterMicroVm{
		RegisterMicroVm: &vzdv1.RegisterMicroVMOp{
			Project: req.Project,
			Name:    req.Name,
			BootIso: boot.BootISO,
			Kernel:  boot.Kernel,
			Initrd:  boot.Initrd,
			Cmdline: boot.Cmdline,
			Shares:  wireShares,
		},
	}}
	logger.Printf("RegisterMicroVM %s: dispatching to host %s", req.Name, req.HostUuid)
	reply, err := s.dispatch.Dispatch(ctx, req.HostUuid, op)
	if err != nil {
		return nil, err // already a status.Status from Dispatch
	}
	if reply.Error != "" {
		logger.Printf("RegisterMicroVM %s on host %s: agent error: %s", req.Name, req.HostUuid, reply.Error)
		return nil, status.Errorf(codes.Internal, "agent %s: %s", req.HostUuid, reply.Error)
	}
	logger.Printf("RegisterMicroVM %s: dispatched to host %s", req.Name, req.HostUuid)
	return &vzdv1.RegisterMicroVMResponse{}, nil
}

// dispatchStartVM routes a StartVM op over the AgentDispatch
// stream to `req.HostUuid`. Mirrors dispatchRegisterMicroVM —
// surfaces the agent's typed error as codes.Internal.
func (s *vzdServer) dispatchStartVM(ctx context.Context, req *vzdv1.StartVMRequest) (*vzdv1.StartVMResponse, error) {
	op := &vzdv1.DriverRequest{Op: &vzdv1.DriverRequest_StartVm{
		StartVm: &vzdv1.StartVMOp{Project: req.Project, Name: req.Name},
	}}
	logger.Printf("StartVM %s: dispatching to host %s", req.Name, req.HostUuid)
	reply, err := s.dispatch.Dispatch(ctx, req.HostUuid, op)
	if err != nil {
		return nil, err
	}
	if reply.Error != "" {
		logger.Printf("StartVM %s on host %s: agent error: %s", req.Name, req.HostUuid, reply.Error)
		return nil, status.Errorf(codes.Internal, "agent %s: %s", req.HostUuid, reply.Error)
	}
	logger.Printf("StartVM %s: dispatched to host %s", req.Name, req.HostUuid)
	return &vzdv1.StartVMResponse{}, nil
}

// dispatchStopVM routes a StopVM op over the AgentDispatch
// stream to `req.HostUuid`.
func (s *vzdServer) dispatchStopVM(ctx context.Context, req *vzdv1.StopVMRequest) (*vzdv1.StopVMResponse, error) {
	op := &vzdv1.DriverRequest{Op: &vzdv1.DriverRequest_StopVm{
		StopVm: &vzdv1.StopVMOp{Project: req.Project, Name: req.Name},
	}}
	logger.Printf("StopVM %s: dispatching to host %s", req.Name, req.HostUuid)
	reply, err := s.dispatch.Dispatch(ctx, req.HostUuid, op)
	if err != nil {
		return nil, err
	}
	if reply.Error != "" {
		logger.Printf("StopVM %s on host %s: agent error: %s", req.Name, req.HostUuid, reply.Error)
		return nil, status.Errorf(codes.Internal, "agent %s: %s", req.HostUuid, reply.Error)
	}
	logger.Printf("StopVM %s: dispatched to host %s", req.Name, req.HostUuid)
	return &vzdv1.StopVMResponse{}, nil
}

// dispatchDeleteVM routes a DeleteVM op over the AgentDispatch
// stream to `req.HostUuid`.
func (s *vzdServer) dispatchDeleteVM(ctx context.Context, req *vzdv1.DeleteVMRequest) (*vzdv1.DeleteVMResponse, error) {
	op := &vzdv1.DriverRequest{Op: &vzdv1.DriverRequest_DeleteVm{
		DeleteVm: &vzdv1.DeleteVMOp{Project: req.Project, Name: req.Name},
	}}
	logger.Printf("DeleteVM %s: dispatching to host %s", req.Name, req.HostUuid)
	reply, err := s.dispatch.Dispatch(ctx, req.HostUuid, op)
	if err != nil {
		return nil, err
	}
	if reply.Error != "" {
		logger.Printf("DeleteVM %s on host %s: agent error: %s", req.Name, req.HostUuid, reply.Error)
		return nil, status.Errorf(codes.Internal, "agent %s: %s", req.HostUuid, reply.Error)
	}
	logger.Printf("DeleteVM %s: dispatched to host %s", req.Name, req.HostUuid)
	return &vzdv1.DeleteVMResponse{}, nil
}

// localHostUUID reads the persisted UUID for the local host.
// Empty when self-registration hasn't run yet (typical in
// integration tests) — `shouldDispatch` treats that as "every
// non-empty host_uuid is remote", which is the right behavior :
// if we don't know our own UUID we can't be the dispatch
// target, so let the agent stream take it.
func localHostUUID(a weft.VZAdapter) string {
	if a == nil {
		return ""
	}
	type uuidGetter interface {
		LocalHostUUID() string
	}
	if g, ok := a.(uuidGetter); ok {
		return g.LocalHostUUID()
	}
	return ""
}

// VMTimings returns the lifecycle event log recorded by vzd at
// <vmDir>/timings.jsonl, in wall-clock order. Empty when the VM
// has no events recorded yet (e.g. queried before any
// RegisterMicroVM or CloneVM finished).
func (s *vzdServer) VMTimings(ctx context.Context, req *vzdv1.VMTimingsRequest) (*vzdv1.VMTimingsResponse, error) {
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	_ = ctx // intentional: read path keeps using its existing background ctx below
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	dir := s.adp.VMDirFor(req.Project, req.Name)
	events, err := weft.ReadTimings(dir)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read timings: %v", err)
	}
	out := make([]*vzdv1.TimingEvent, len(events))
	for i, e := range events {
		out[i] = &vzdv1.TimingEvent{
			Name:     e.Name,
			TsUnixNs: e.TsUnixNano,
			Meta:     e.Meta,
		}
	}
	return &vzdv1.VMTimingsResponse{Events: out}, nil
}

// VMLogs returns the raw bytes of <vmDir>/console.log, optionally
// truncated to the last `tail_bytes` to keep the gRPC response from
// pinning the whole serial log into memory for chatty guests.
//
// `total_bytes` always carries the on-disk size so the client can
// detect when the response was truncated and decide whether to
// page the rest.
func (s *vzdServer) VMLogs(ctx context.Context, req *vzdv1.VMLogsRequest) (*vzdv1.VMLogsResponse, error) {
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	dir := s.adp.VMDirFor(req.Project, req.Name)
	path := filepath.Join(dir, "console.log")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// VM exists but has never booted (no console.log yet),
			// or the name is unknown. Either way: return an empty
			// payload with total=0 — clients distinguish these by
			// pairing with VMStatus, not by introspecting this RPC.
			return &vzdv1.VMLogsResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "open console.log: %v", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stat console.log: %v", err)
	}
	total := st.Size()
	// Seek to the right offset for the tail case. tail_bytes <= 0
	// or larger than the file means "give me the whole thing".
	offset := int64(0)
	if req.TailBytes > 0 && req.TailBytes < total {
		offset = total - req.TailBytes
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, status.Errorf(codes.Internal, "seek console.log: %v", err)
		}
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read console.log: %v", err)
	}
	return &vzdv1.VMLogsResponse{Contents: buf, TotalBytes: total}, nil
}

// WatchEvents streams platform events to the caller until the
// underlying stream is cancelled. Subscription filter is built
// from (a) the caller's visible projects (ACL — never leaks
// events from projects the operator can't see) plus (b) the
// optional kind_prefix / project narrowing the request specified.
//
// Backpressure: the bus drops events for slow subscribers rather
// than blocking publishers (per [[vzd-event-bus]]). A consumer
// that needs guaranteed delivery should pair WatchEvents with
// occasional VMTimings reads.
func (s *vzdServer) WatchEvents(req *vzdv1.WatchEventsRequest, stream vzdv1.WeftAgent_WatchEventsServer) error {
	ctx := stream.Context()
	visible, all, err := s.adp.VisibleProjects(ctx)
	if err != nil {
		return err
	}
	// Resolve the optional `project` filter (display name or UUID)
	// to a UUID up-front so the per-event check is O(1).
	var projectFilter string
	if req.Project != "" {
		uuid, err := s.adp.AuthorizeProject(ctx, req.Project)
		if err != nil {
			return err
		}
		projectFilter = uuid
	}
	bus := s.adp.EventBus()
	if bus == nil {
		return status.Error(codes.Unavailable, "event bus not initialised")
	}
	ch, cancel := bus.Subscribe(weft.EventFilter{
		KindPrefixes: req.KindPrefix,
		Visible:      visible,
		SeeAll:       all,
		Project:      projectFilter,
		Subject:      req.Subject,
	})
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&vzdv1.PlatformEvent{
				TsUnixNs:    ev.TsUnixNano,
				Kind:        ev.Kind,
				Subject:     ev.Subject,
				ProjectUuid: ev.ProjectUUID,
				Meta:        ev.Meta,
			}); err != nil {
				return err
			}
		}
	}
}

// RenderNATSAuthorization returns the NATS-conf `authorization`
// block for the operator to splice into nats.conf. Admin-gated:
// the block reveals every project's NATS pubkey, which is
// operator-information rather than tenant-information.
// Per [[vzd-tenant-event-access]] Phase 3.
func (s *vzdServer) RenderNATSAuthorization(ctx context.Context, req *vzdv1.RenderNATSAuthorizationRequest) (*vzdv1.RenderNATSAuthorizationResponse, error) {
	if err := weft.RequireAdmin(ctx, "render nats authorization"); err != nil {
		return nil, err
	}
	conf, err := s.adp.RenderNATSAuthorization(weft.NATSAuthorizationOptions{
		AdminPubkey: req.AdminPubkey,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "render nats authz: %v", err)
	}
	return &vzdv1.RenderNATSAuthorizationResponse{Config: []byte(conf)}, nil
}

// --- Project registry RPCs ------------------------------------------------

func toProjectInfo(p weft.Project) *vzdv1.ProjectInfo {
	return &vzdv1.ProjectInfo{
		Uuid:            p.UUID,
		Name:            p.Name,
		CreatedAtUnixNs: p.CreatedAt.UnixNano(),
	}
}

func (s *vzdServer) ListProjects(ctx context.Context, _ *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error) {
	visible, all, err := s.adp.VisibleProjects(ctx)
	if err != nil {
		return nil, err
	}
	projects := s.adp.Projects()
	out := make([]*vzdv1.ProjectInfo, 0, len(projects))
	for _, p := range projects {
		if !all {
			if _, ok := visible[p.UUID]; !ok {
				continue
			}
		}
		out = append(out, toProjectInfo(p))
	}
	return &vzdv1.ListProjectsResponse{Projects: out}, nil
}

func (s *vzdServer) CreateProject(ctx context.Context, req *vzdv1.CreateProjectRequest) (*vzdv1.CreateProjectResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	// Free-form project creation is admin-only: every authenticated
	// caller already has an auto-created default project under their
	// `sub`, so a non-admin name pick would just be a way to grab
	// arbitrary namespaces. Dev mode keeps the open path.
	if err := weft.RequireAdmin(ctx, "create project"); err != nil {
		return nil, err
	}
	p, created, err := s.adp.CreateProject(req.Name)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create project: %v", err)
	}
	logger.Printf("CreateProject name=%s uuid=%s created=%v", p.Name, p.UUID, created)
	return &vzdv1.CreateProjectResponse{Project: toProjectInfo(p), Created: created}, nil
}

func (s *vzdServer) RenameProject(ctx context.Context, req *vzdv1.RenameProjectRequest) (*vzdv1.RenameProjectResponse, error) {
	if req.Uuid == "" || req.NewName == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid and new_name are required")
	}
	if err := weft.RequireAdmin(ctx, "rename project"); err != nil {
		return nil, err
	}
	if err := s.adp.RenameProject(req.Uuid, req.NewName); err != nil {
		return nil, status.Errorf(codes.Internal, "rename project: %v", err)
	}
	p, ok := s.adp.ProjectByUUID(req.Uuid)
	if !ok {
		return nil, status.Errorf(codes.Internal, "project %s vanished after rename", req.Uuid)
	}
	logger.Printf("RenameProject uuid=%s -> name=%s", p.UUID, p.Name)
	return &vzdv1.RenameProjectResponse{Project: toProjectInfo(p)}, nil
}

func (s *vzdServer) DeleteProject(ctx context.Context, req *vzdv1.DeleteProjectRequest) (*vzdv1.DeleteProjectResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := weft.RequireAdmin(ctx, "delete project"); err != nil {
		return nil, err
	}
	if err := s.adp.DeleteProject(req.Uuid); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "delete project: %v", err)
	}
	logger.Printf("DeleteProject uuid=%s", req.Uuid)
	return &vzdv1.DeleteProjectResponse{}, nil
}

// ---- Flavors (compute-envelope catalogue) -------------------------
//
// Cluster-wide catalogue backed by weft.FlavorRegistry. Read RPCs
// are open to any authenticated caller (the catalogue is needed in
// every CreateVMModal) ; write RPCs are admin-only (operators with
// RequireAdmin per the existing convention).
//
// The registry is constructed at startup against the configured
// Storage — file backend for single-host dev, etcd backend for HA.
// nil during integration tests that don't need it ; every RPC
// guards via flavorsReady.

func (s *vzdServer) flavorsReady() error {
	if s.flavors == nil {
		return status.Error(codes.Unavailable, "flavors registry not wired on this build")
	}
	return nil
}

func (s *vzdServer) ListFlavors(_ context.Context, _ *vzdv1.ListFlavorsRequest) (*vzdv1.ListFlavorsResponse, error) {
	if err := s.flavorsReady(); err != nil {
		return nil, err
	}
	all := s.flavors.List()
	out := &vzdv1.ListFlavorsResponse{Flavors: make([]*vzdv1.Flavor, 0, len(all))}
	for _, f := range all {
		out.Flavors = append(out.Flavors, &vzdv1.Flavor{
			Name: f.Name, Vcpu: int32(f.VCPU), Ram: f.RAM,
			EphemeralGb: int32(f.EphemeralGB), Gpu: f.GPU,
		})
	}
	return out, nil
}

func (s *vzdServer) GetFlavor(_ context.Context, req *vzdv1.GetFlavorRequest) (*vzdv1.GetFlavorResponse, error) {
	if err := s.flavorsReady(); err != nil {
		return nil, err
	}
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	f, ok := s.flavors.Get(req.Name)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no such flavor: %s", req.Name)
	}
	return &vzdv1.GetFlavorResponse{Flavor: &vzdv1.Flavor{
		Name: f.Name, Vcpu: int32(f.VCPU), Ram: f.RAM,
		EphemeralGb: int32(f.EphemeralGB), Gpu: f.GPU,
	}}, nil
}

func (s *vzdServer) SetFlavor(ctx context.Context, req *vzdv1.SetFlavorRequest) (*vzdv1.SetFlavorResponse, error) {
	if err := s.flavorsReady(); err != nil {
		return nil, err
	}
	if req.Flavor == nil {
		return nil, status.Error(codes.InvalidArgument, "flavor is required")
	}
	if err := weft.RequireAdmin(ctx, "set flavor"); err != nil {
		return nil, err
	}
	in := req.Flavor
	if err := s.flavors.Set(weft.Flavor{
		Name: in.Name, VCPU: int(in.Vcpu), RAM: in.Ram,
		EphemeralGB: int(in.EphemeralGb), GPU: in.Gpu,
	}); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "set flavor: %v", err)
	}
	saved, _ := s.flavors.Get(in.Name)
	logger.Printf("SetFlavor name=%s vcpu=%d ram=%s", saved.Name, saved.VCPU, saved.RAM)
	return &vzdv1.SetFlavorResponse{Flavor: &vzdv1.Flavor{
		Name: saved.Name, Vcpu: int32(saved.VCPU), Ram: saved.RAM,
		EphemeralGb: int32(saved.EphemeralGB), Gpu: saved.GPU,
	}}, nil
}

func (s *vzdServer) DeleteFlavor(ctx context.Context, req *vzdv1.DeleteFlavorRequest) (*vzdv1.DeleteFlavorResponse, error) {
	if err := s.flavorsReady(); err != nil {
		return nil, err
	}
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := weft.RequireAdmin(ctx, "delete flavor"); err != nil {
		return nil, err
	}
	if err := s.flavors.Delete(req.Name); err != nil {
		return nil, status.Errorf(codes.Internal, "delete flavor: %v", err)
	}
	logger.Printf("DeleteFlavor name=%s", req.Name)
	return &vzdv1.DeleteFlavorResponse{Deleted: req.Name}, nil
}

// ---- Scripts (provisioning catalogue) -----------------------------
//
// Same shape as the flavors block above. Body is the literal sh
// source ; UpdatedAt + UpdatedBy are stamped server-side from the
// auth context so the wire can't lie about provenance.

func (s *vzdServer) scriptsReady() error {
	if s.scripts == nil {
		return status.Error(codes.Unavailable, "scripts registry not wired on this build")
	}
	return nil
}

func (s *vzdServer) ListScripts(_ context.Context, _ *vzdv1.ListScriptsRequest) (*vzdv1.ListScriptsResponse, error) {
	if err := s.scriptsReady(); err != nil {
		return nil, err
	}
	all := s.scripts.List()
	out := &vzdv1.ListScriptsResponse{Scripts: make([]*vzdv1.Script, 0, len(all))}
	for _, sc := range all {
		out.Scripts = append(out.Scripts, scriptToProto(sc))
	}
	return out, nil
}

func (s *vzdServer) GetScript(_ context.Context, req *vzdv1.GetScriptRequest) (*vzdv1.GetScriptResponse, error) {
	if err := s.scriptsReady(); err != nil {
		return nil, err
	}
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	sc, ok := s.scripts.Get(req.Name)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no such script: %s", req.Name)
	}
	return &vzdv1.GetScriptResponse{Script: scriptToProto(sc)}, nil
}

func (s *vzdServer) SetScript(ctx context.Context, req *vzdv1.SetScriptRequest) (*vzdv1.SetScriptResponse, error) {
	if err := s.scriptsReady(); err != nil {
		return nil, err
	}
	if req.Script == nil {
		return nil, status.Error(codes.InvalidArgument, "script is required")
	}
	if err := weft.RequireAdmin(ctx, "set script"); err != nil {
		return nil, err
	}
	in := req.Script
	editor := ""
	if c, ok := weft.CallerFrom(ctx); ok && c != nil {
		editor = c.Email
		if editor == "" {
			editor = c.Subject
		}
	}
	if err := s.scripts.Set(weft.Script{
		Name: in.Name, Description: in.Description, Body: in.Body,
	}, editor); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "set script: %v", err)
	}
	saved, _ := s.scripts.Get(in.Name)
	logger.Printf("SetScript name=%s by=%s body-bytes=%d", saved.Name, saved.UpdatedBy, len(saved.Body))
	return &vzdv1.SetScriptResponse{Script: scriptToProto(saved)}, nil
}

func (s *vzdServer) DeleteScript(ctx context.Context, req *vzdv1.DeleteScriptRequest) (*vzdv1.DeleteScriptResponse, error) {
	if err := s.scriptsReady(); err != nil {
		return nil, err
	}
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := weft.RequireAdmin(ctx, "delete script"); err != nil {
		return nil, err
	}
	if err := s.scripts.Delete(req.Name); err != nil {
		return nil, status.Errorf(codes.Internal, "delete script: %v", err)
	}
	logger.Printf("DeleteScript name=%s", req.Name)
	return &vzdv1.DeleteScriptResponse{Deleted: req.Name}, nil
}

func scriptToProto(s weft.Script) *vzdv1.Script {
	ts := ""
	if !s.UpdatedAt.IsZero() {
		ts = s.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return &vzdv1.Script{
		Name: s.Name, Description: s.Description, Body: s.Body,
		UpdatedAt: ts, UpdatedBy: s.UpdatedBy,
	}
}

// AddProjectMember grants project access to a user-UUID. Admin-
// only: the operator promising "this token's `sub` matches that
// vzd user" is a security-sensitive call. The membership
// shows up immediately in the caller's next VisibleProjects /
// AuthorizeProject evaluation.
func (s *vzdServer) AddProjectMember(ctx context.Context, req *vzdv1.AddProjectMemberRequest) (*vzdv1.AddProjectMemberResponse, error) {
	if req.ProjectUuid == "" || req.UserUuid == "" {
		return nil, status.Error(codes.InvalidArgument, "project_uuid and user_uuid are required")
	}
	if err := weft.RequireAdmin(ctx, "add project member"); err != nil {
		return nil, err
	}
	if _, ok := s.adp.UserByUUID(req.UserUuid); !ok {
		return nil, status.Errorf(codes.NotFound, "user %s not in registry — login first or use `vzc user ls` to find the UUID", req.UserUuid)
	}
	if err := s.adp.AddProjectMember(req.ProjectUuid, req.UserUuid); err != nil {
		return nil, status.Errorf(codes.Internal, "add member: %v", err)
	}
	members, _ := s.adp.ProjectMembers(req.ProjectUuid)
	logger.Printf("AddProjectMember project=%s user=%s (count=%d)", req.ProjectUuid, req.UserUuid, len(members))
	return &vzdv1.AddProjectMemberResponse{UserUuids: members}, nil
}

// RemoveProjectMember revokes the platform-side grant. Admin
// only. Note this does NOT clear a `project:<uuid>` dex group
// claim — those revocations happen on the IdP side.
func (s *vzdServer) RemoveProjectMember(ctx context.Context, req *vzdv1.RemoveProjectMemberRequest) (*vzdv1.RemoveProjectMemberResponse, error) {
	if req.ProjectUuid == "" || req.UserUuid == "" {
		return nil, status.Error(codes.InvalidArgument, "project_uuid and user_uuid are required")
	}
	if err := weft.RequireAdmin(ctx, "remove project member"); err != nil {
		return nil, err
	}
	if err := s.adp.RemoveProjectMember(req.ProjectUuid, req.UserUuid); err != nil {
		return nil, status.Errorf(codes.Internal, "remove member: %v", err)
	}
	members, _ := s.adp.ProjectMembers(req.ProjectUuid)
	logger.Printf("RemoveProjectMember project=%s user=%s (count=%d)", req.ProjectUuid, req.UserUuid, len(members))
	return &vzdv1.RemoveProjectMemberResponse{UserUuids: members}, nil
}

// ListProjectMembers returns the member UUIDs. AuthorizeProject
// gates the read — a project member can list their own peers
// without needing platform-admin, but a non-member can't probe
// who's in.
func (s *vzdServer) ListProjectMembers(ctx context.Context, req *vzdv1.ListProjectMembersRequest) (*vzdv1.ListProjectMembersResponse, error) {
	if req.ProjectUuid == "" {
		return nil, status.Error(codes.InvalidArgument, "project_uuid is required")
	}
	if _, err := s.adp.AuthorizeProject(ctx, req.ProjectUuid); err != nil {
		return nil, err
	}
	members, ok := s.adp.ProjectMembers(req.ProjectUuid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "project %s not found", req.ProjectUuid)
	}
	return &vzdv1.ListProjectMembersResponse{UserUuids: members}, nil
}

// WaitVM polls until the VM has an IP address or the timeout elapses.
func (s *vzdServer) WaitVM(ctx context.Context, req *vzdv1.WaitVMRequest) (*vzdv1.WaitVMResponse, error) {
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	logger.Printf("WaitVM name=%s timeout=%ds", req.Name, req.TimeoutSeconds)
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	if err := waitForIP(s.adp, req.Name, timeout); err != nil {
		return nil, status.Errorf(codes.DeadlineExceeded, "wait vm: %v", err)
	}
	ip, _ := s.adp.IP(req.Name)
	return &vzdv1.WaitVMResponse{Ip: ip}, nil
}

// enrichVMConfig reads config.json from vmDir and merges extra fields into it.
func enrichVMConfig(vmDir string, extra map[string]interface{}) error {
	cfgPath := filepath.Join(vmDir, "config.json")
	data, _ := os.ReadFile(cfgPath)
	m := map[string]interface{}{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &m)
	}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, b, 0o600)
}

// waitForIPVerbose polls until the VM has an IP, logging each failed attempt.
func waitForIPVerbose(a weft.VZAdapter, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	attempt := 0
	for time.Now().Before(deadline) {
		attempt++
		ip, err := a.IP(name)
		if err == nil && ip != "" {
			return ip, nil
		}
		if attempt == 1 || attempt%15 == 0 { // log first attempt then every 30s
			if err != nil {
				logger.Printf("waitForIP %s: attempt %d: %v", name, attempt, err)
			} else {
				logger.Printf("waitForIP %s: attempt %d: no IP yet", name, attempt)
			}
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("vm %q did not obtain an IP within %s", name, timeout)
}

// waitForIP polls the adapter until the named VM reports a non-empty IP.
func waitForIP(a weft.VZAdapter, name string, timeout time.Duration) error {
	_, err := waitForIPVerbose(a, name, timeout)
	return err
}

// ---- helpers ---------------------------------------------------------------

func rowToProto(r imock.Row) *vzdv1.VMInfo {
	return &vzdv1.VMInfo{
		Name:   r.Name,
		State:  stateToProto(r.State),
		Os:     r.OS,
		Cpu:    uint32(r.CPU),
		MemMb:  uint64(r.Mem) * 1024, // r.Mem is in GiB; proto field is MB
		DiskGb: uint64(r.Disk),
		Image:  r.Image,
		Ip:     r.IP,
	}
}

func stateToProto(s string) vzdv1.VMState {
	switch s {
	case "running":
		return vzdv1.VMState_VM_STATE_RUNNING
	case "stopped":
		return vzdv1.VMState_VM_STATE_STOPPED
	case "not-created":
		return vzdv1.VMState_VM_STATE_NOT_CREATED
	default:
		return vzdv1.VMState_VM_STATE_UNSPECIFIED
	}
}

// applyDiskFileOps writes each DiskFileOp into the VM disk image and runs any
// requested post-copy trigger. Logic is delegated to the grub package.
func applyDiskFileOps(diskPath string, ops []*vzdv1.DiskFileOp) error {
	fileOps := make([]grubpkg.FileOp, len(ops))
	for i, op := range ops {
		fileOps[i] = grubpkg.NewFileOp(op.Content, op.Dst, op.Trigger)
	}
	return grubpkg.ApplyFileOps(diskPath, fileOps)
}

// deleteDiskFileOps removes each DiskDeleteOp path from the disk image.
func deleteDiskFileOps(diskPath string, ops []*vzdv1.DiskDeleteOp) error {
	dsts := make([]string, len(ops))
	for i, op := range ops {
		dsts[i] = op.Dst
	}
	return grubpkg.DeleteFileOps(diskPath, dsts)
}

// modDiskFileOps applies each DiskModOp in-place substitution to the disk image.
func modDiskFileOps(diskPath string, ops []*vzdv1.DiskModOp) error {
	modOps := make([]grubpkg.ModOp, len(ops))
	for i, op := range ops {
		modOps[i] = grubpkg.NewModOp(op.Dst, op.Old, op.New)
	}
	return grubpkg.ModFileOps(diskPath, modOps)
}
