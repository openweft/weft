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
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	sshtransport "github.com/grpc-transports/ssh"
	cloudinit "github.com/openweft/cloud-init"
	imock "github.com/openweft/hclconfig"
	"github.com/openweft/weft"
	"github.com/openweft/weft-microvm-init/pkg/pod"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/auditlog"
	"github.com/openweft/weft/cmd/weft/admin"
	"github.com/openweft/weft/cmd/weft/clean"
	"github.com/openweft/weft/cmd/weft/completion"
	"github.com/openweft/weft/cmd/weft/events"
	"github.com/openweft/weft/cmd/weft/flavor"
	"github.com/openweft/weft/cmd/weft/host"
	"github.com/openweft/weft/cmd/weft/image"
	"github.com/openweft/weft/cmd/weft/instance"
	"github.com/openweft/weft/cmd/weft/login"
	"github.com/openweft/weft/cmd/weft/microvm"
	"github.com/openweft/weft/cmd/weft/network"
	"github.com/openweft/weft/cmd/weft/overlaycmd"
	"github.com/openweft/weft/cmd/weft/plugin"
	"github.com/openweft/weft/cmd/weft/project"
	"github.com/openweft/weft/cmd/weft/script"
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
//	                        was the standalone weft binary).
//	weft project / vm /   — gRPC clients that talk to a running agent over
//	  network / volume /    the Unix socket (formerly weft subcommands).
//	  events / login / …
//	weft infra deploy /   — in-process orchestrator that boots etcd/dex/zot/
//	  bootstrap / status /  nats micro-VMs from infra/<svc>/plan.hcl. Stays
//	  validate              an Adapter-direct subcommand for chicken-and-egg
//	                        bootstraps (before the agent's gRPC is reachable).
//	weft vz-vm-run /      — hidden subcommands forked by the Apple-VZ driver
//	  vz-provision          for per-VM subprocesses.
func rootCmd() *cobra.Command {
	// Shared client-side connection flags (consumed by every client
	// subcommand below). Mirror the legacy `weft` defaults so operator
	// muscle memory keeps working — same socket paths.
	home, _ := os.UserHomeDir()
	defaultSocket := filepath.Join(home, ".weft", "weft.sock")
	defaultSSHSocket := filepath.Join(home, ".weft", "weft-ssh.sock")

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

	// Server / daemon subcommand. Carries the formerly-cmd/weft flags.
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
	root.AddCommand(newDownCmd())

	// Client subcommands (was: weft). All speak gRPC to the running agent.
	root.AddCommand(
		instance.Command(&socketPath, &sshSocket, &sshKey),
		microvm.Command(&socketPath, &sshSocket, &sshKey),
		image.Command(&socketPath, &sshSocket, &sshKey),
		project.Command(&socketPath, &sshSocket, &sshKey),
		flavor.Command(&socketPath, &sshSocket, &sshKey),
		script.Command(&socketPath, &sshSocket, &sshKey),
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
		plugin.Command(&socketPath, &sshSocket, &sshKey),
		// Shell-completion script generator. Stateless — no socket
		// flags, no gRPC. The script is generated against `root`
		// (resolved via c.Root() inside completion.Command) so it
		// covers the whole tree, not just the completion subcommand.
		// See docs/operations/completion.md for the install runbook.
		completion.Command(),
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
	var tcpListen string
	var az string
	var rack string
	var proxyEnabled bool
	var proxyStateDir string
	var proxyCaddyBinary string
	var proxyKeyPrefix string
	var metricsListen string
	var auditLogPath string

	home, _ := os.UserHomeDir()
	defaultSocket := filepath.Join(home, ".weft", "weft.sock")
	defaultSSHSocket := filepath.Join(home, ".weft", "weft-ssh.sock")
	defaultAuthorizedKeys := filepath.Join(home, ".weft", "authorized_keys")

	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run Weft as the long-lived control-plane daemon",
		Long: `Boots the gRPC server (plain Unix socket + optional SSH-secured Unix
socket), wires the storage backend (file or etcd), the event bus
(in-process or NATS), the OIDC validator (dex), and the driver dispatch
(Apple-VZ + future siblings).

Reads weft.hcl from /etc/weft/weft.hcl or ~/.config/weft/weft.hcl by default ;
CLI flags override the file. The HCL config block, flag set, and default
paths are unchanged from the legacy "weft" daemon for operational
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
			// Same env-var bridge for placement metadata — selfRegisterHost
			// reads $WEFT_AZ / $WEFT_RACK before constructing its
			// RegisterHostSpec, and the scheduler matches plans that ask
			// for az=different / rack=different against those values.
			if az != "" {
				_ = os.Setenv("WEFT_AZ", az)
			}
			if rack != "" {
				_ = os.Setenv("WEFT_RACK", rack)
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
				tcpListen:         tcpListen,
				configDir:         cfgDir,
				oidcIssuer:        oidcIssuer,
				oidcClientID:      oidcClientID,
				storageBackend:    storageBackend,
				eventBusBackend:   eventBusBackend,
				// Proxy fields seeded with CLI-flag defaults so that
				// `applyFileConfigDefaults` only overrides them when
				// the HCL block sets a value. The Changed()-restore
				// loop below flips CLI back on top for any flag the
				// operator passed explicitly.
				proxyEnabled:     proxyEnabled,
				proxyStateDir:    proxyStateDir,
				proxyCaddyBinary: proxyCaddyBinary,
				proxyKeyPrefix:   proxyKeyPrefix,
				metricsListen:    metricsListen,
				auditLogPath:     auditLogPath,
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
			if c.Flags().Changed("proxy") {
				tgt.proxyEnabled = before.proxyEnabled
			}
			if c.Flags().Changed("proxy-state-dir") {
				tgt.proxyStateDir = before.proxyStateDir
			}
			if c.Flags().Changed("proxy-caddy-binary") {
				tgt.proxyCaddyBinary = before.proxyCaddyBinary
			}
			if c.Flags().Changed("proxy-key-prefix") {
				tgt.proxyKeyPrefix = before.proxyKeyPrefix
			}
			if c.Flags().Changed("metrics-listen") {
				tgt.metricsListen = before.metricsListen
			}
			if c.Flags().Changed("audit-log") {
				tgt.auditLogPath = before.auditLogPath
			}
			// proxyStorageEndpoints has no CLI flag yet — the HCL
			// block is the only source. Add a comma-separated
			// `--proxy-storage-endpoints` later if operator demand
			// shows up ; until then, env var fallback inside
			// agent/proxy.Supervisor.resolveStorageEndpoints handles
			// container injection.
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
				if proxyEnabled {
					// The proxy plane needs a local etcd handle ;
					// in client mode etcd is reached only through
					// the control-plane gRPC bridge. Flagging here
					// rather than silently degrading — an operator
					// who asked for --proxy expects ingress, and
					// not getting it should be loud.
					logger.Printf("weft agent: --proxy ignored in --client mode (no local etcd handle; follow-up: etcd-over-gRPC bridge)")
				}
				return runClient(tgt)
			}
			if clientMode && controlPlaneURL == "" {
				logger.Printf("weft agent: --client without --control-plane=URL ; running all-in-one")
			}
			return run(tgt)
		},
	}

	cmd.Flags().StringVar(&configFile, "config", "", "Path to weft.hcl (default: /etc/weft/weft.hcl or ~/.config/weft/weft.hcl)")
	cmd.Flags().StringVar(&cfgDir, "config-dir", ".mock/hcl", "Path to HCL config directory")
	cmd.Flags().StringVar(&socketPath, "socket", defaultSocket, "Unix socket path to listen on")
	cmd.Flags().StringVar(&sshSocket, "ssh-socket", defaultSSHSocket, "Unix socket path for the SSH-secured gRPC listener (empty to disable)")
	cmd.Flags().StringVar(&sshAuthorizedKeys, "ssh-authorized-keys", defaultAuthorizedKeys, "Path to authorized_keys for SSH client authentication")
	cmd.Flags().StringVar(&tcpListen, "tcp-listen", "", "host:port for an additional plain-TCP gRPC listener — dev-mode cross-host bring-up; production should use the SSH transport. Empty disables.")
	cmd.Flags().StringVar(&az, "az", "", "Availability-zone label for this host (matched by scheduler placement rules; mirrors $WEFT_AZ).")
	cmd.Flags().StringVar(&rack, "rack", "", "Rack label for this host (sub-AZ placement domain; mirrors $WEFT_RACK).")
	cmd.Flags().StringVar(&oidcIssuer, "oidc-issuer", "", "OIDC issuer URL (empty = dev mode, no token validation)")
	cmd.Flags().StringVar(&oidcClientID, "oidc-client-id", "", "OIDC audience that tokens must be issued for")
	cmd.Flags().StringVar(&storageBackend, "storage-backend", "", `Registry persistence backend: "file" (dev, local disk), "etcd" (prod, 3-DC cluster), or "embed-etcd" (single-host, in-process etcd under <configDir>/etcd-embed). Empty = HCL config decides; HCL empty = "file".`)
	cmd.Flags().StringVar(&eventBusBackend, "event-bus", "", `Event-bus backend: "local" (dev, in-process channels) or "nats" (prod, 3-DC cluster). Empty = HCL config decides; HCL empty = "local".`)
	cmd.Flags().StringVar(&hypervisor, "hypervisor", "", `Local hypervisor driver: "" / "apple-vz" (default) or "qemu" (QEMU/TCG — pure emulation, works without nested virt).`)
	cmd.Flags().BoolVar(&serverMode, "server", false, "Run as control-plane server (no per-host driver dispatch). Default mode includes both.")
	cmd.Flags().BoolVar(&clientMode, "client", false, "Run as per-host driver runtime only. Requires --control-plane to point at the server.")
	cmd.Flags().StringVar(&controlPlaneURL, "control-plane", "", "URL of the Weft control-plane server (only consulted when --client is set).")

	// Reverse-proxy plane (see proxy.go + agent/proxy/). Off by
	// default — operators opt in with --proxy. The Caddy binary
	// can point at any caddy on PATH ; production deploys should
	// use the weft-proxy artefact (xcaddy-built with the etcd
	// storage module) published by openweft/weft-proxy.
	cmd.Flags().BoolVar(&proxyEnabled, "proxy", false, "Enable the reverse-proxy plane (Caddy supervised subprocess + etcd Watcher). All-in-one mode only ; ignored under --client.")
	cmd.Flags().StringVar(&proxyStateDir, "proxy-state-dir", "", "Directory for the Caddy admin socket + cert storage. Empty → $XDG_RUNTIME_DIR/weft-agent-proxy.")
	cmd.Flags().StringVar(&proxyCaddyBinary, "proxy-caddy-binary", "caddy", "Caddy executable. For production, point at the weft-proxy binary from openweft/weft-proxy (xcaddy + etcd-storage module).")
	cmd.Flags().StringVar(&proxyKeyPrefix, "proxy-key-prefix", "", "etcd key prefix the Watcher streams from. Empty → /weft/proxy/routes (proxy.Watcher default).")

	// Prometheus observability (per docs/operations/observability.md).
	// Off by default — a Mac laptop doesn't need a metrics endpoint.
	// Operators that scrape `weft agent` set --metrics-listen=":9101"
	// (or `metrics_listen = ":9101"` in weft.hcl) ; same `host:port`
	// shape as --tcp-listen for muscle-memory consistency.
	cmd.Flags().StringVar(&metricsListen, "metrics-listen", "", "host:port for the Prometheus /metrics endpoint (process + Go runtime + gRPC server histograms). Empty disables.")

	// RBAC audit log — every Allow/Deny decision through the three
	// ACL primitives in pkg/openweft/weft/acl.go ships one LDJSON
	// line here when set. The flag without a value enables the
	// audit log at the default path (/var/log/weft/audit.jsonl) ;
	// pass `--audit-log=/some/path` to override. Rotation is the
	// operator's job — see docs/operations/rbac.md.
	cmd.Flags().StringVar(&auditLogPath, "audit-log", "", "Path to the RBAC audit log (LDJSON). Empty disables ; pass --audit-log on its own to enable at "+auditlog.DefaultPath+".")
	cmd.Flags().Lookup("audit-log").NoOptDefVal = auditlog.DefaultPath

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

	// Event bus backend (per [[weft-event-bus-nats]]). Default is
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
	// path = … }` in weft.hcl. The hook is a no-op when path is
	// empty; the renderer stays callable via `weft admin nats-authz`.
	// Per [[weft-tenant-event-access]] Phase-5 follow-up.
	if t.natsAuthzPath != "" {
		a.SetNATSAuthorizationFile(t.natsAuthzPath, t.natsAuthzAdminPubkey)
		logger.Printf("nats authorization auto-render enabled: path=%s", t.natsAuthzPath)
	}

	// RBAC audit log : per docs/operations/rbac.md, Allow/Deny
	// decisions from the three ACL primitives ship one LDJSON line
	// to t.auditLogPath. Empty disables ; Open expands the parent
	// dir with 0700 and opens the file with 0600 (audit lines can
	// leak attack surfaces, treat them as sensitive). Rotation is
	// external (logrotate / journald) — see auditlog/auditlog.go.
	if t.auditLogPath != "" {
		al, err := auditlog.Open(t.auditLogPath)
		if err != nil {
			return fmt.Errorf("open audit log: %w", err)
		}
		weft.SetAuditLogger(al)
		defer func() { _ = al.Close() }()
		logger.Printf("rbac audit log enabled: path=%s", t.auditLogPath)
	}

	logger.Printf("weft starting — config-dir=%s socket=%s storage=%s event_bus=%s",
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

	// Prometheus observability — opt-in via --metrics-listen. When
	// enabled we mint a dedicated *prometheus.Registry (NOT the
	// default global) so the gRPC server-side metrics live alongside
	// the process + Go runtime collectors and unrelated client_golang
	// users elsewhere in the binary don't leak into our scrape output.
	// The interceptor below is added to the chain regardless — when
	// srvMetrics is nil it's a no-op grpc.{Unary,Stream}ServerInterceptor.
	var srvMetrics *grpcprom.ServerMetrics
	var rpcByKind *rpcMetrics
	var metricsCloser func() error
	if t.metricsListen != "" {
		reg, closer, mErr := startMetricsServer(t.metricsListen, logger)
		if mErr != nil {
			return fmt.Errorf("start metrics server: %w", mErr)
		}
		metricsCloser = closer
		srvMetrics = grpcprom.NewServerMetrics(grpcprom.WithServerHandlingTimeHistogram())
		if err := reg.Register(srvMetrics); err != nil {
			return fmt.Errorf("register grpc server metrics: %w", err)
		}
		// weft-specific per-driver-kind counter. Sits alongside the
		// stock grpc histogram so a single scrape exposes both ; the
		// `driver_kind` label is what makes per-driver alerting on
		// multi-plugin hosts work (see rpc_metrics.go).
		rm, mErr := newRPCMetrics(reg)
		if mErr != nil {
			return fmt.Errorf("register weft_rpc_total: %w", mErr)
		}
		rpcByKind = rm
		defer func() { _ = metricsCloser() }()
	}

	unaryInterceptors := []grpc.UnaryServerInterceptor{
		weft.UnaryAuthInterceptor(validator, userPersister(a)),
	}
	streamInterceptors := []grpc.StreamServerInterceptor{
		weft.StreamAuthInterceptor(validator, userPersister(a)),
	}
	if srvMetrics != nil {
		// Order matters : the Prom interceptor wraps the inner call so
		// it observes the handler's elapsed time + status code. We
		// append after the auth interceptor so authn failures are
		// also counted (rejected RPCs still show up in
		// grpc_server_handled_total{code="Unauthenticated"}).
		unaryInterceptors = append(unaryInterceptors, srvMetrics.UnaryServerInterceptor())
		streamInterceptors = append(streamInterceptors, srvMetrics.StreamServerInterceptor())
	}
	if rpcByKind != nil {
		// rpcByKind wraps INSIDE the grpc histogram so the kind slot
		// it installs is visible to the handler ; the histogram only
		// reads code/elapsed and doesn't peek at ctx values.
		unaryInterceptors = append(unaryInterceptors, rpcByKind.UnaryInterceptor())
		streamInterceptors = append(streamInterceptors, rpcByKind.StreamInterceptor())
	}
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(unaryInterceptors...),
		grpc.ChainStreamInterceptor(streamInterceptors...),
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
	vmPropReg, err := weft.LoadVMPropertyRegistry(context.Background(), a.RegistryStorage("vm-properties"))
	if err != nil {
		return fmt.Errorf("load vm-property registry: %w", err)
	}
	uefiReg, err := weft.LoadUEFIVarRegistry(context.Background(), a.RegistryStorage("uefi-vars"))
	if err != nil {
		return fmt.Errorf("load uefi-var registry: %w", err)
	}
	sshKeyReg, err := weft.LoadVMSSHKeyRegistry(context.Background(), a.RegistryStorage("vm-sshkeys"))
	if err != nil {
		return fmt.Errorf("load vm-sshkey registry: %w", err)
	}
	weftv1.RegisterWeftAgentServer(srv, &weftServer{
		cfgDir:        t.configDir,
		mc:            mc,
		adp:           a,
		dispatch:      dispatchSrv,
		localHostUUID: localHostUUID(a),
		flavors:       flavorReg,
		scripts:       scriptReg,
		vmProps:       vmPropReg,
		uefiVars:      uefiReg,
		vmKeys:        sshKeyReg,
	})
	weftv1.RegisterAgentDispatchServer(srv, dispatchSrv)

	// Top-level lifecycle ctx — cancelled on SIGINT/SIGTERM. The
	// proxy plane (when --proxy is set) hangs off it so the
	// watcher cancels before the etcd client is torn down by the
	// storage factory's deferred close.
	ctx, stop := signalContext()
	defer stop()

	// Reverse-proxy plane (Caddy supervised subprocess + etcd
	// Watcher). Opt-in via --proxy ; sf.etcdClient is nil in the
	// file backend and bootProxy degrades to "supervisor-only"
	// (Caddy starts with empty routes). The closer runs before
	// sf.close() because defers unwind LIFO — exactly the order
	// we need (watcher off → etcd client closed).
	if t.proxyEnabled {
		hostUUID := localHostUUID(a)
		proxyCloser, perr := bootProxyFn(ctx, hostUUID, sf.etcdClient, proxyOpts{
			StateDir:         t.proxyStateDir,
			CaddyBinary:      t.proxyCaddyBinary,
			KeyPrefix:        t.proxyKeyPrefix,
			StorageEndpoints: t.proxyStorageEndpoints,
		})
		if perr != nil {
			return fmt.Errorf("boot proxy: %w", perr)
		}
		defer func() { _ = proxyCloser() }()
		logger.Printf("proxy plane enabled — host_uuid=%s caddy=%s state_dir=%s key_prefix=%s",
			hostUUID, displayOrDefault(t.proxyCaddyBinary, "caddy"), displayOrDefault(t.proxyStateDir, "<runtime-default>"), displayOrDefault(t.proxyKeyPrefix, "<watcher-default>"))
	}

	go func() {
		if err := srv.Serve(lis); err != nil {
			logger.Printf("grpc server stopped: %v", err)
		}
	}()
	logger.Printf("listening on %s", t.socket)

	// Optional plain-TCP listener — same gRPC server, dev-mode transport
	// for the cross-host bring-up. The SSH-secured Unix listener below is
	// preferred everywhere else; this exists because sshtransport doesn't
	// yet bridge to a remote SSH host. No TLS — caller identity still
	// flows through the standard bearer interceptor chain.
	if t.tcpListen != "" {
		tcpLis, err := net.Listen("tcp", t.tcpListen)
		if err != nil {
			return fmt.Errorf("tcp listener on %s: %w", t.tcpListen, err)
		}
		go func() {
			if err := srv.Serve(tcpLis); err != nil {
				logger.Printf("tcp grpc server stopped: %v", err)
			}
		}()
		logger.Printf("TCP gRPC listening on %s (dev-mode, no TLS)", t.tcpListen)
	}

	// Optional SSH-secured listener — same gRPC server (and same
	// auth interceptors), different transport.
	if t.sshSocket != "" {
		home, _ := os.UserHomeDir()
		_ = os.Remove(t.sshSocket)
		sshLis, err := sshtransport.ListenSSH("unix:"+t.sshSocket, sshtransport.ServerConfig{
			HostKeyPath:        filepath.Join(home, ".weft", "weft_host_key"),
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

	// Block until a termination signal lands. The deferred
	// proxyCloser + sf.close() then run LIFO : watcher cancelled
	// → Caddy stopped → etcd client closed. Pre-proxy code path
	// kept `select {}` (no signals) ; the new wait preserves that
	// behaviour when --proxy is not set because nothing else
	// cancels ctx.
	<-ctx.Done()
	logger.Printf("weft agent: shutdown signal received")
	return nil
}

// ---- gRPC server -----------------------------------------------------------

type weftServer struct {
	weftv1.UnimplementedWeftAgentServer
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
	// vmProps is the per-VM host-set annotations registry. Pairs
	// with the guest's pkg/properties NATS subscriber.
	vmProps *weft.VMPropertyRegistry
	// uefiVars is the per-VM UEFI NVRAM editor. Hypervisor (vz /
	// qemu drivers) writes the OVMF VARS file from it at boot.
	uefiVars *weft.UEFIVarRegistry
	// vmKeys is the per-VM authorised SSH-keys store. Consumed by
	// the in-guest pkg/sshkeys subscriber (writes authorized_keys +
	// feeds the embedded sshd AuthStore).
	vmKeys *weft.VMSSHKeyRegistry
}

func (s *weftServer) ListVMs(ctx context.Context, req *weftv1.ListVMsRequest) (*weftv1.ListVMsResponse, error) {
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
	var vms []*weftv1.VMInfo
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
		info := &weftv1.VMInfo{
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
	return &weftv1.ListVMsResponse{Vms: vms}, nil
}

func (s *weftServer) VMStatus(ctx context.Context, req *weftv1.VMStatusRequest) (*weftv1.VMStatusResponse, error) {
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
	info := &weftv1.VMInfo{
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
	return &weftv1.VMStatusResponse{Vm: info}, nil
}

func (s *weftServer) StartVM(ctx context.Context, req *weftv1.StartVMRequest) (*weftv1.StartVMResponse, error) {
	logger.Printf("StartVM name=%s project=%s host=%s", req.Name, req.Project, req.HostUuid)
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	// Stamp the driver kind that will field this RPC onto the metric
	// slot the interceptor installed. Empty when the VM isn't in the
	// inventory (legacy on-disk VM) — the empty `driver_kind=""` series
	// captures it without conflating with any driver.
	RecordRPCKind(ctx, s.adp.LookupKindForVM(req.Name))
	if s.shouldDispatch(req.HostUuid) {
		return s.dispatchStartVM(ctx, req)
	}
	if err := s.adp.StartVM(req.Name, ""); err != nil {
		logger.Printf("StartVM %s: error: %v", req.Name, err)
		return nil, status.Errorf(codes.Internal, "start vm: %v", err)
	}
	return &weftv1.StartVMResponse{}, nil
}

func (s *weftServer) StopVM(ctx context.Context, req *weftv1.StopVMRequest) (*weftv1.StopVMResponse, error) {
	logger.Printf("StopVM name=%s project=%s host=%s", req.Name, req.Project, req.HostUuid)
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	RecordRPCKind(ctx, s.adp.LookupKindForVM(req.Name))
	if s.shouldDispatch(req.HostUuid) {
		return s.dispatchStopVM(ctx, req)
	}
	if err := s.adp.StopVM(req.Name); err != nil {
		logger.Printf("StopVM %s: error: %v", req.Name, err)
		return nil, status.Errorf(codes.Internal, "stop vm: %v", err)
	}
	return &weftv1.StopVMResponse{}, nil
}

func (s *weftServer) CreateVM(ctx context.Context, req *weftv1.CreateVMRequest) (*weftv1.CreateVMResponse, error) {
	logger.Printf("CreateVM name=%s image=%s project=%s", req.Name, req.Image, req.Project)
	projUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	// Hard-cap enforcement at handler entry per
	// docs/operations/tenant-quotas.md. ResourceExhausted is the
	// canonical gRPC code for "request denied by a quota" — clients
	// (CLI + webui) translate it to the operator-visible "quota
	// exceeded" toast without needing handler-specific knowledge.
	if err := s.adp.EnforceTenantQuotaForVM(projUUID, int(req.Cpu), int(req.MemMb)); err != nil {
		return nil, err
	}
	// GPU aggregate cap. The proto's CreateVMRequest doesn't yet
	// carry RequestedGpus (classic VMs don't request GPUs through
	// this RPC today — see weftv1.CreateVMRequest) ; passing nil
	// re-checks the already-allocated total against the cap, a
	// no-op for projects still within budget. Threading
	// RequestedGpus through the proto is a follow-up for when
	// CreateVMRequest grows a GPU surface ; the aggregate
	// arithmetic itself is in place via projectAllocation +
	// EnforceTenantQuotaForGPU.
	if err := s.adp.EnforceTenantQuotaForGPU(projUUID, nil); err != nil {
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
	return &weftv1.CreateVMResponse{}, nil
}

func (s *weftServer) DeleteVM(ctx context.Context, req *weftv1.DeleteVMRequest) (*weftv1.DeleteVMResponse, error) {
	logger.Printf("DeleteVM name=%s project=%s host=%s", req.Name, req.Project, req.HostUuid)
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	// Resolve the kind BEFORE DeleteVM drops the inventory row — once
	// the row is gone LookupKindForVM returns "" and the metric label
	// would be empty on the success path. Resolving up-front means a
	// successful DELETE is still counted against the right driver kind.
	RecordRPCKind(ctx, s.adp.LookupKindForVM(req.Name))
	if s.shouldDispatch(req.HostUuid) {
		return s.dispatchDeleteVM(ctx, req)
	}
	if err := s.adp.DeleteVM(req.Name); err != nil {
		logger.Printf("DeleteVM %s: error: %v", req.Name, err)
		return nil, status.Errorf(codes.Internal, "delete vm: %v", err)
	}
	return &weftv1.DeleteVMResponse{}, nil
}

// ProvisionVM clones the image, injects a cloud-init ISO if an SSH public key
// is provided, starts the VM and waits up to 3 minutes for an IP address.
func (s *weftServer) ProvisionVM(ctx context.Context, req *weftv1.ProvisionVMRequest) (*weftv1.ProvisionVMResponse, error) {
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
	return &weftv1.ProvisionVMResponse{}, nil
}

// DeprovisionVM stops (best-effort) then deletes a VM.
func (s *weftServer) DeprovisionVM(ctx context.Context, req *weftv1.DeprovisionVMRequest) (*weftv1.DeprovisionVMResponse, error) {
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	logger.Printf("DeprovisionVM name=%s", req.Name)
	_ = s.adp.StopVM(req.Name) // best-effort; ignore error
	if err := s.adp.DeleteVM(req.Name); err != nil {
		return nil, status.Errorf(codes.Internal, "delete vm: %v", err)
	}
	return &weftv1.DeprovisionVMResponse{}, nil
}

// PullImages pulls all images referenced in the HCL config directory.
func (s *weftServer) PullImages(ctx context.Context, req *weftv1.PullImagesRequest) (*weftv1.PullImagesResponse, error) {
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
		return &weftv1.PullImagesResponse{}, nil
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
	return &weftv1.PullImagesResponse{}, nil
}

// PullImage pulls a single image by URL.
func (s *weftServer) PullImage(ctx context.Context, req *weftv1.PullImageRequest) (*weftv1.PullImageResponse, error) {
	if req.Url == "" {
		return nil, status.Error(codes.InvalidArgument, "url is required")
	}
	logger.Printf("PullImage url=%s", req.Url)
	if err := s.adp.Pull(ctx, []string{req.Url}, 1); err != nil {
		logger.Printf("PullImage: error: %v", err)
		return nil, status.Errorf(codes.Internal, "pull image: %v", err)
	}
	logger.Printf("PullImage: done")
	return &weftv1.PullImageResponse{}, nil
}

// PatchImage applies DiskFileOps to a cached image so that all VMs cloned
// from it inherit the patches without needing per-instance copy blocks.
func (s *weftServer) PatchImage(_ context.Context, req *weftv1.PatchImageRequest) (*weftv1.PatchImageResponse, error) {
	if req.Url == "" {
		return nil, status.Error(codes.InvalidArgument, "url is required")
	}
	if len(req.FileOps) == 0 && len(req.DeleteOps) == 0 && len(req.ModOps) == 0 {
		return &weftv1.PatchImageResponse{}, nil
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
	return &weftv1.PatchImageResponse{}, nil
}

// ListImages returns all locally cached images.
func (s *weftServer) ListImages(_ context.Context, _ *weftv1.ListImagesRequest) (*weftv1.ListImagesResponse, error) {
	images, err := s.adp.ListCachedImages()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list cached images: %v", err)
	}
	var infos []*weftv1.ImageInfo
	for _, img := range images {
		infos = append(infos, &weftv1.ImageInfo{
			Url:       img.URL(),
			Name:      img.Name(),
			Format:    img.Format(),
			SizeBytes: img.SizeBytes(),
		})
	}
	return &weftv1.ListImagesResponse{Images: infos}, nil
}

// CleanImages removes cached images referenced in the HCL config.
func (s *weftServer) CleanImages(_ context.Context, req *weftv1.CleanImagesRequest) (*weftv1.CleanImagesResponse, error) {
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
		return &weftv1.CleanImagesResponse{Deleted: toDelete}, nil
	}
	var deleted []string
	for _, n := range toDelete {
		if err := s.adp.DeleteOCI(n); err == nil {
			deleted = append(deleted, n)
		}
	}
	return &weftv1.CleanImagesResponse{Deleted: deleted}, nil
}

// RegisterMicroVM wires a VM directory for a microVM-style boot:
// the primary storage device is a read-only weft-microvm-init UKI ISO, and
// the guest sees one or more virtio-fs shares carrying the OCI
// image rootfs(es). The actual VM creation happens through
// adapter.RegisterMicroVM (added in the same change as this RPC);
// once it returns the VM behaves like any other weft VM and is
// started/stopped via the normal StartVM/StopVM RPCs.
func (s *weftServer) RegisterMicroVM(ctx context.Context, req *weftv1.RegisterMicroVMRequest) (*weftv1.RegisterMicroVMResponse, error) {
	logger.Printf("RegisterMicroVM name=%s project=%s boot_iso=%s kernel=%s initrd=%s cmdline=%q shares=%d",
		req.Name, req.Project, req.BootIso, req.Kernel, req.Initrd, req.Cmdline, len(req.Shares))
	projUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	// RegisterMicroVM doesn't carry cpu/memory in its request (the
	// boot artefacts dictate the runtime shape) ; we still consult
	// the quota so a project that has already exhausted its
	// cpu/memory cap can't keep spawning microVMs. Passing (0, 0)
	// just re-checks the already-allocated total against the cap —
	// a no-op for projects still within budget. Per
	// docs/operations/tenant-quotas.md.
	if err := s.adp.EnforceTenantQuotaForVM(projUUID, 0, 0); err != nil {
		return nil, err
	}
	// GPU aggregate cap, same shape as the CreateVM site. The
	// proto's RegisterMicroVMRequest doesn't carry RequestedGpus
	// either (microVM boots so far don't request GPUs through
	// this RPC) ; passing nil re-checks the already-allocated
	// total against the cap. Threading RequestedGpus through the
	// proto is a follow-up for when the microVM surface grows a
	// GPU shape.
	if err := s.adp.EnforceTenantQuotaForGPU(projUUID, nil); err != nil {
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
	//
	// Stamp the driver kind for the metric BEFORE we route — the
	// request carries the target host, so we can resolve the kind
	// without needing the VM record (which RegisterMicroVM is about
	// to create). Arch isn't on the wire yet ; passing "" picks the
	// primary kind, same rule HostHandleOnArch uses.
	hostUUID := req.HostUuid
	if hostUUID == "" {
		hostUUID = s.localHostUUID
	}
	RecordRPCKind(ctx, s.adp.LookupKind(hostUUID, ""))
	if s.shouldDispatch(req.HostUuid) {
		return s.dispatchRegisterMicroVM(ctx, req, boot, shares)
	}
	if err := s.adp.RegisterMicroVM(req.Project, req.Name, boot, shares); err != nil {
		logger.Printf("RegisterMicroVM %s: error: %v", req.Name, err)
		return nil, status.Errorf(codes.Internal, "register microvm: %v", err)
	}
	return &weftv1.RegisterMicroVMResponse{}, nil
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
func (s *weftServer) PublishShareToProject(ctx context.Context, req *weftv1.PublishShareToProjectRequest) (*weftv1.PublishShareToProjectResponse, error) {
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
	return &weftv1.PublishShareToProjectResponse{VmCount: uint32(n)}, nil
}

// shouldDispatch reports whether a RegisterMicroVM (or any
// future host-pinned op) should route through the dispatch
// stream rather than run in-process. Returns false for empty
// host_uuid, a self-target, or a server without a configured
// dispatch registry — all of those fall through to the local
// Adapter call.
func (s *weftServer) shouldDispatch(hostUUID string) bool {
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
func (s *weftServer) dispatchRegisterMicroVM(
	ctx context.Context,
	req *weftv1.RegisterMicroVMRequest,
	boot weft.MicroVMBoot,
	shares []weft.MicroVMShare,
) (*weftv1.RegisterMicroVMResponse, error) {
	wireShares := make([]*weftv1.MicroVMShare, len(shares))
	for i, sh := range shares {
		wireShares[i] = &weftv1.MicroVMShare{
			Tag:      sh.Tag,
			Path:     sh.Path,
			ReadOnly: sh.ReadOnly,
			Clone:    sh.Clone,
		}
	}
	op := &weftv1.DriverRequest{Op: &weftv1.DriverRequest_RegisterMicroVm{
		RegisterMicroVm: &weftv1.RegisterMicroVMOp{
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
	return &weftv1.RegisterMicroVMResponse{}, nil
}

// dispatchStartVM routes a StartVM op over the AgentDispatch
// stream to `req.HostUuid`. Mirrors dispatchRegisterMicroVM —
// surfaces the agent's typed error as codes.Internal.
func (s *weftServer) dispatchStartVM(ctx context.Context, req *weftv1.StartVMRequest) (*weftv1.StartVMResponse, error) {
	op := &weftv1.DriverRequest{Op: &weftv1.DriverRequest_StartVm{
		StartVm: &weftv1.StartVMOp{Project: req.Project, Name: req.Name},
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
	return &weftv1.StartVMResponse{}, nil
}

// dispatchStopVM routes a StopVM op over the AgentDispatch
// stream to `req.HostUuid`.
func (s *weftServer) dispatchStopVM(ctx context.Context, req *weftv1.StopVMRequest) (*weftv1.StopVMResponse, error) {
	op := &weftv1.DriverRequest{Op: &weftv1.DriverRequest_StopVm{
		StopVm: &weftv1.StopVMOp{Project: req.Project, Name: req.Name},
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
	return &weftv1.StopVMResponse{}, nil
}

// dispatchDeleteVM routes a DeleteVM op over the AgentDispatch
// stream to `req.HostUuid`.
func (s *weftServer) dispatchDeleteVM(ctx context.Context, req *weftv1.DeleteVMRequest) (*weftv1.DeleteVMResponse, error) {
	op := &weftv1.DriverRequest{Op: &weftv1.DriverRequest_DeleteVm{
		DeleteVm: &weftv1.DeleteVMOp{Project: req.Project, Name: req.Name},
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
	return &weftv1.DeleteVMResponse{}, nil
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

// VMTimings returns the lifecycle event log recorded by weft at
// <vmDir>/timings.jsonl, in wall-clock order. Empty when the VM
// has no events recorded yet (e.g. queried before any
// RegisterMicroVM or CloneVM finished).
func (s *weftServer) VMTimings(ctx context.Context, req *weftv1.VMTimingsRequest) (*weftv1.VMTimingsResponse, error) {
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
	out := make([]*weftv1.TimingEvent, len(events))
	for i, e := range events {
		out[i] = &weftv1.TimingEvent{
			Name:     e.Name,
			TsUnixNs: e.TsUnixNano,
			Meta:     e.Meta,
		}
	}
	return &weftv1.VMTimingsResponse{Events: out}, nil
}

// VMLogs returns the raw bytes of <vmDir>/console.log, optionally
// truncated to the last `tail_bytes` to keep the gRPC response from
// pinning the whole serial log into memory for chatty guests.
//
// `total_bytes` always carries the on-disk size so the client can
// detect when the response was truncated and decide whether to
// page the rest.
func (s *weftServer) VMLogs(ctx context.Context, req *weftv1.VMLogsRequest) (*weftv1.VMLogsResponse, error) {
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
			return &weftv1.VMLogsResponse{}, nil
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
	return &weftv1.VMLogsResponse{Contents: buf, TotalBytes: total}, nil
}

// WatchEvents streams platform events to the caller until the
// underlying stream is cancelled. Subscription filter is built
// from (a) the caller's visible projects (ACL — never leaks
// events from projects the operator can't see) plus (b) the
// optional kind_prefix / project narrowing the request specified.
//
// Backpressure: the bus drops events for slow subscribers rather
// than blocking publishers (per [[weft-event-bus]]). A consumer
// that needs guaranteed delivery should pair WatchEvents with
// occasional VMTimings reads.
func (s *weftServer) WatchEvents(req *weftv1.WatchEventsRequest, stream weftv1.WeftAgent_WatchEventsServer) error {
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
			if err := stream.Send(&weftv1.PlatformEvent{
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
// Per [[weft-tenant-event-access]] Phase 3.
func (s *weftServer) RenderNATSAuthorization(ctx context.Context, req *weftv1.RenderNATSAuthorizationRequest) (*weftv1.RenderNATSAuthorizationResponse, error) {
	if err := weft.RequireAdmin(ctx, "render nats authorization"); err != nil {
		return nil, err
	}
	conf, err := s.adp.RenderNATSAuthorization(weft.NATSAuthorizationOptions{
		AdminPubkey: req.AdminPubkey,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "render nats authz: %v", err)
	}
	return &weftv1.RenderNATSAuthorizationResponse{Config: []byte(conf)}, nil
}

// --- Project registry RPCs ------------------------------------------------

func toProjectInfo(p weft.Project) *weftv1.ProjectInfo {
	return &weftv1.ProjectInfo{
		Uuid:            p.UUID,
		Name:            p.Name,
		CreatedAtUnixNs: p.CreatedAt.UnixNano(),
	}
}

func (s *weftServer) ListProjects(ctx context.Context, _ *weftv1.ListProjectsRequest) (*weftv1.ListProjectsResponse, error) {
	visible, all, err := s.adp.VisibleProjects(ctx)
	if err != nil {
		return nil, err
	}
	projects := s.adp.Projects()
	out := make([]*weftv1.ProjectInfo, 0, len(projects))
	for _, p := range projects {
		if !all {
			if _, ok := visible[p.UUID]; !ok {
				continue
			}
		}
		out = append(out, toProjectInfo(p))
	}
	return &weftv1.ListProjectsResponse{Projects: out}, nil
}

func (s *weftServer) CreateProject(ctx context.Context, req *weftv1.CreateProjectRequest) (*weftv1.CreateProjectResponse, error) {
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
	return &weftv1.CreateProjectResponse{Project: toProjectInfo(p), Created: created}, nil
}

func (s *weftServer) RenameProject(ctx context.Context, req *weftv1.RenameProjectRequest) (*weftv1.RenameProjectResponse, error) {
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
	return &weftv1.RenameProjectResponse{Project: toProjectInfo(p)}, nil
}

func (s *weftServer) DeleteProject(ctx context.Context, req *weftv1.DeleteProjectRequest) (*weftv1.DeleteProjectResponse, error) {
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
	return &weftv1.DeleteProjectResponse{}, nil
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

func (s *weftServer) flavorsReady() error {
	if s.flavors == nil {
		return status.Error(codes.Unavailable, "flavors registry not wired on this build")
	}
	return nil
}

func (s *weftServer) ListFlavors(_ context.Context, _ *weftv1.ListFlavorsRequest) (*weftv1.ListFlavorsResponse, error) {
	if err := s.flavorsReady(); err != nil {
		return nil, err
	}
	all := s.flavors.List()
	out := &weftv1.ListFlavorsResponse{Flavors: make([]*weftv1.Flavor, 0, len(all))}
	for _, f := range all {
		out.Flavors = append(out.Flavors, &weftv1.Flavor{
			Name: f.Name, Vcpu: int32(f.VCPU), Ram: f.RAM,
			EphemeralGb: int32(f.EphemeralGB), Gpu: f.GPU,
		})
	}
	return out, nil
}

func (s *weftServer) GetFlavor(_ context.Context, req *weftv1.GetFlavorRequest) (*weftv1.GetFlavorResponse, error) {
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
	return &weftv1.GetFlavorResponse{Flavor: &weftv1.Flavor{
		Name: f.Name, Vcpu: int32(f.VCPU), Ram: f.RAM,
		EphemeralGb: int32(f.EphemeralGB), Gpu: f.GPU,
	}}, nil
}

func (s *weftServer) SetFlavor(ctx context.Context, req *weftv1.SetFlavorRequest) (*weftv1.SetFlavorResponse, error) {
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
	return &weftv1.SetFlavorResponse{Flavor: &weftv1.Flavor{
		Name: saved.Name, Vcpu: int32(saved.VCPU), Ram: saved.RAM,
		EphemeralGb: int32(saved.EphemeralGB), Gpu: saved.GPU,
	}}, nil
}

func (s *weftServer) DeleteFlavor(ctx context.Context, req *weftv1.DeleteFlavorRequest) (*weftv1.DeleteFlavorResponse, error) {
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
	return &weftv1.DeleteFlavorResponse{Deleted: req.Name}, nil
}

// ---- Scripts (provisioning catalogue) -----------------------------
//
// Same shape as the flavors block above. Body is the literal sh
// source ; UpdatedAt + UpdatedBy are stamped server-side from the
// auth context so the wire can't lie about provenance.

func (s *weftServer) scriptsReady() error {
	if s.scripts == nil {
		return status.Error(codes.Unavailable, "scripts registry not wired on this build")
	}
	return nil
}

func (s *weftServer) ListScripts(_ context.Context, _ *weftv1.ListScriptsRequest) (*weftv1.ListScriptsResponse, error) {
	if err := s.scriptsReady(); err != nil {
		return nil, err
	}
	all := s.scripts.List()
	out := &weftv1.ListScriptsResponse{Scripts: make([]*weftv1.Script, 0, len(all))}
	for _, sc := range all {
		out.Scripts = append(out.Scripts, scriptToProto(sc))
	}
	return out, nil
}

func (s *weftServer) GetScript(_ context.Context, req *weftv1.GetScriptRequest) (*weftv1.GetScriptResponse, error) {
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
	return &weftv1.GetScriptResponse{Script: scriptToProto(sc)}, nil
}

func (s *weftServer) SetScript(ctx context.Context, req *weftv1.SetScriptRequest) (*weftv1.SetScriptResponse, error) {
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
	return &weftv1.SetScriptResponse{Script: scriptToProto(saved)}, nil
}

func (s *weftServer) DeleteScript(ctx context.Context, req *weftv1.DeleteScriptRequest) (*weftv1.DeleteScriptResponse, error) {
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
	return &weftv1.DeleteScriptResponse{Deleted: req.Name}, nil
}

func scriptToProto(s weft.Script) *weftv1.Script {
	ts := ""
	if !s.UpdatedAt.IsZero() {
		ts = s.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return &weftv1.Script{
		Name: s.Name, Description: s.Description, Body: s.Body,
		UpdatedAt: ts, UpdatedBy: s.UpdatedBy,
	}
}

// ---- VM properties (per-VM host-set annotations) ------------------
//
// (vm_name, project) addresses one VM ; key is the operator-chosen
// label inside that VM. guest_readable opts the entry into the
// in-guest weft-microvm-agent's NATS read surface. Admin-only writes ;
// every authenticated caller can read (the dashboard's Properties
// drawer tab is open to operators with project membership).

func (s *weftServer) vmPropsReady() error {
	if s.vmProps == nil {
		return status.Error(codes.Unavailable, "vm-properties registry not wired on this build")
	}
	return nil
}

func (s *weftServer) ListVMProperties(_ context.Context, req *weftv1.ListVMPropertiesRequest) (*weftv1.ListVMPropertiesResponse, error) {
	if err := s.vmPropsReady(); err != nil {
		return nil, err
	}
	if req.VmName == "" {
		return nil, status.Error(codes.InvalidArgument, "vm_name is required")
	}
	all := s.vmProps.ListForVM(req.Project, req.VmName)
	out := &weftv1.ListVMPropertiesResponse{Properties: make([]*weftv1.VMProperty, 0, len(all))}
	for _, p := range all {
		out.Properties = append(out.Properties, vmPropToProto(p))
	}
	return out, nil
}

func (s *weftServer) SetVMProperty(ctx context.Context, req *weftv1.SetVMPropertyRequest) (*weftv1.SetVMPropertyResponse, error) {
	if err := s.vmPropsReady(); err != nil {
		return nil, err
	}
	if req.Property == nil {
		return nil, status.Error(codes.InvalidArgument, "property is required")
	}
	if req.VmName == "" {
		return nil, status.Error(codes.InvalidArgument, "vm_name is required")
	}
	if err := weft.RequireAdmin(ctx, "set vm property"); err != nil {
		return nil, err
	}
	in := req.Property
	if err := s.vmProps.Set(weft.VMProperty{
		VMName: req.VmName, Project: req.Project,
		Key: in.Key, Value: in.Value, GuestReadable: in.GuestReadable,
	}); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "set vm property: %v", err)
	}
	// Re-fetch to return the stamped UpdatedAt the registry just wrote.
	resolved := pickProperty(s.vmProps.ListForVM(req.Project, req.VmName), in.Key)
	logger.Printf("SetVMProperty vm=%s/%s key=%s guest=%t", req.Project, req.VmName, in.Key, in.GuestReadable)
	return &weftv1.SetVMPropertyResponse{Property: vmPropToProto(resolved)}, nil
}

func (s *weftServer) DeleteVMProperty(ctx context.Context, req *weftv1.DeleteVMPropertyRequest) (*weftv1.DeleteVMPropertyResponse, error) {
	if err := s.vmPropsReady(); err != nil {
		return nil, err
	}
	if req.VmName == "" || req.Key == "" {
		return nil, status.Error(codes.InvalidArgument, "vm_name and key are required")
	}
	if err := weft.RequireAdmin(ctx, "delete vm property"); err != nil {
		return nil, err
	}
	if err := s.vmProps.Delete(req.Project, req.VmName, req.Key); err != nil {
		return nil, status.Errorf(codes.Internal, "delete vm property: %v", err)
	}
	logger.Printf("DeleteVMProperty vm=%s/%s key=%s", req.Project, req.VmName, req.Key)
	return &weftv1.DeleteVMPropertyResponse{Deleted: req.Key}, nil
}

func vmPropToProto(p weft.VMProperty) *weftv1.VMProperty {
	ts := ""
	if !p.UpdatedAt.IsZero() {
		ts = p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return &weftv1.VMProperty{
		Key: p.Key, Value: p.Value,
		GuestReadable: p.GuestReadable, UpdatedAt: ts,
	}
}

// pickProperty returns the entry with the given Key from a slice, or
// a zero-value VMProperty when none matches. Used by SetVMProperty
// to recover the registry-stamped UpdatedAt without re-locking the
// registry mutex.
func pickProperty(list []weft.VMProperty, key string) weft.VMProperty {
	for _, p := range list {
		if p.Key == key {
			return p
		}
	}
	return weft.VMProperty{}
}

// ---- UEFI variables (per-VM NVRAM editor) -------------------------
//
// Hex-encoded byte blobs keyed by (project, vm, namespace, name).
// Admin-only writes ; reads open to authenticated callers (the
// drawer's UEFI tab is operator-discoverable). Empty namespace from
// the wire defaults to EFI Global ; the registry normalises before
// storing.

func (s *weftServer) uefiVarsReady() error {
	if s.uefiVars == nil {
		return status.Error(codes.Unavailable, "uefi-vars registry not wired on this build")
	}
	return nil
}

func (s *weftServer) ListUEFIVars(_ context.Context, req *weftv1.ListUEFIVarsRequest) (*weftv1.ListUEFIVarsResponse, error) {
	if err := s.uefiVarsReady(); err != nil {
		return nil, err
	}
	if req.VmName == "" {
		return nil, status.Error(codes.InvalidArgument, "vm_name is required")
	}
	all := s.uefiVars.ListForVM(req.Project, req.VmName)
	out := &weftv1.ListUEFIVarsResponse{Vars: make([]*weftv1.UEFIVar, 0, len(all))}
	for _, v := range all {
		out.Vars = append(out.Vars, uefiVarToProto(v))
	}
	return out, nil
}

func (s *weftServer) SetUEFIVar(ctx context.Context, req *weftv1.SetUEFIVarRequest) (*weftv1.SetUEFIVarResponse, error) {
	if err := s.uefiVarsReady(); err != nil {
		return nil, err
	}
	if req.Var == nil || req.VmName == "" {
		return nil, status.Error(codes.InvalidArgument, "vm_name and var are required")
	}
	if err := weft.RequireAdmin(ctx, "set uefi var"); err != nil {
		return nil, err
	}
	in := req.Var
	if err := s.uefiVars.Set(weft.UEFIVar{
		VMName: req.VmName, Project: req.Project,
		Namespace: in.Namespace, Name: in.Name,
		ValueHex: in.ValueHex, Attributes: in.Attributes,
	}); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "set uefi var: %v", err)
	}
	// Re-fetch to recover the stamped UpdatedAt + the normalised
	// namespace (empty → EFI Global).
	saved := weft.UEFIVar{}
	wantNS := in.Namespace
	if wantNS == "" {
		wantNS = weft.EFIGlobalNS
	}
	for _, v := range s.uefiVars.ListForVM(req.Project, req.VmName) {
		if v.Namespace == wantNS && v.Name == in.Name {
			saved = v
			break
		}
	}
	logger.Printf("SetUEFIVar vm=%s/%s var=%s/%s", req.Project, req.VmName, wantNS, in.Name)
	return &weftv1.SetUEFIVarResponse{Var: uefiVarToProto(saved)}, nil
}

func (s *weftServer) DeleteUEFIVar(ctx context.Context, req *weftv1.DeleteUEFIVarRequest) (*weftv1.DeleteUEFIVarResponse, error) {
	if err := s.uefiVarsReady(); err != nil {
		return nil, err
	}
	if req.VmName == "" || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "vm_name and name are required")
	}
	if err := weft.RequireAdmin(ctx, "delete uefi var"); err != nil {
		return nil, err
	}
	if err := s.uefiVars.Delete(req.Project, req.VmName, req.Namespace, req.Name); err != nil {
		return nil, status.Errorf(codes.Internal, "delete uefi var: %v", err)
	}
	logger.Printf("DeleteUEFIVar vm=%s/%s var=%s/%s", req.Project, req.VmName, req.Namespace, req.Name)
	return &weftv1.DeleteUEFIVarResponse{Deleted: req.Name}, nil
}

func uefiVarToProto(v weft.UEFIVar) *weftv1.UEFIVar {
	ts := ""
	if !v.UpdatedAt.IsZero() {
		ts = v.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return &weftv1.UEFIVar{
		Namespace: v.Namespace, Name: v.Name,
		ValueHex: v.ValueHex, Attributes: v.Attributes, UpdatedAt: ts,
	}
}

// ---- Per-VM SSH keys ----------------------------------------------
//
// Server parses the OpenSSH line + computes the fingerprint server-
// side ; that's the stable identity for Remove. Idempotent on
// re-add (same fingerprint = no duplication). Admin-only writes ;
// reads open to any authenticated caller.

func (s *weftServer) vmKeysReady() error {
	if s.vmKeys == nil {
		return status.Error(codes.Unavailable, "vm-sshkey registry not wired on this build")
	}
	return nil
}

func (s *weftServer) ListVMSSHKeys(_ context.Context, req *weftv1.ListVMSSHKeysRequest) (*weftv1.ListVMSSHKeysResponse, error) {
	if err := s.vmKeysReady(); err != nil {
		return nil, err
	}
	if req.VmName == "" {
		return nil, status.Error(codes.InvalidArgument, "vm_name is required")
	}
	all := s.vmKeys.ListForVM(req.Project, req.VmName)
	out := &weftv1.ListVMSSHKeysResponse{Keys: make([]*weftv1.VMSSHKey, 0, len(all))}
	for _, k := range all {
		out.Keys = append(out.Keys, vmKeyToProto(k))
	}
	return out, nil
}

func (s *weftServer) AddVMSSHKey(ctx context.Context, req *weftv1.AddVMSSHKeyRequest) (*weftv1.AddVMSSHKeyResponse, error) {
	if err := s.vmKeysReady(); err != nil {
		return nil, err
	}
	if req.VmName == "" || req.PublicKey == "" {
		return nil, status.Error(codes.InvalidArgument, "vm_name and public_key are required")
	}
	if err := weft.RequireAdmin(ctx, "add vm ssh key"); err != nil {
		return nil, err
	}
	entry, err := s.vmKeys.Add(req.Project, req.VmName, req.PublicKey)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "add vm ssh key: %v", err)
	}
	logger.Printf("AddVMSSHKey vm=%s/%s fp=%s", req.Project, req.VmName, entry.Fingerprint)
	return &weftv1.AddVMSSHKeyResponse{Key: vmKeyToProto(entry)}, nil
}

func (s *weftServer) RemoveVMSSHKey(ctx context.Context, req *weftv1.RemoveVMSSHKeyRequest) (*weftv1.RemoveVMSSHKeyResponse, error) {
	if err := s.vmKeysReady(); err != nil {
		return nil, err
	}
	if req.VmName == "" || req.Fingerprint == "" {
		return nil, status.Error(codes.InvalidArgument, "vm_name and fingerprint are required")
	}
	if err := weft.RequireAdmin(ctx, "remove vm ssh key"); err != nil {
		return nil, err
	}
	if err := s.vmKeys.Remove(req.Project, req.VmName, req.Fingerprint); err != nil {
		return nil, status.Errorf(codes.Internal, "remove vm ssh key: %v", err)
	}
	logger.Printf("RemoveVMSSHKey vm=%s/%s fp=%s", req.Project, req.VmName, req.Fingerprint)
	return &weftv1.RemoveVMSSHKeyResponse{Removed: req.Fingerprint}, nil
}

func vmKeyToProto(k weft.VMSSHKey) *weftv1.VMSSHKey {
	ts := ""
	if !k.AddedAt.IsZero() {
		ts = k.AddedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return &weftv1.VMSSHKey{
		Fingerprint: k.Fingerprint, Type: k.Type,
		PublicKey: k.PublicKey, Comment: k.Comment, AddedAt: ts,
	}
}

// AddProjectMember grants project access to a user-UUID. Admin-
// only: the operator promising "this token's `sub` matches that
// weft user" is a security-sensitive call. The membership
// shows up immediately in the caller's next VisibleProjects /
// AuthorizeProject evaluation.
func (s *weftServer) AddProjectMember(ctx context.Context, req *weftv1.AddProjectMemberRequest) (*weftv1.AddProjectMemberResponse, error) {
	if req.ProjectUuid == "" || req.UserUuid == "" {
		return nil, status.Error(codes.InvalidArgument, "project_uuid and user_uuid are required")
	}
	if err := weft.RequireAdmin(ctx, "add project member"); err != nil {
		return nil, err
	}
	if _, ok := s.adp.UserByUUID(req.UserUuid); !ok {
		return nil, status.Errorf(codes.NotFound, "user %s not in registry — login first or use `weft user ls` to find the UUID", req.UserUuid)
	}
	if err := s.adp.AddProjectMember(req.ProjectUuid, req.UserUuid); err != nil {
		return nil, status.Errorf(codes.Internal, "add member: %v", err)
	}
	members, _ := s.adp.ProjectMembers(req.ProjectUuid)
	logger.Printf("AddProjectMember project=%s user=%s (count=%d)", req.ProjectUuid, req.UserUuid, len(members))
	return &weftv1.AddProjectMemberResponse{UserUuids: members}, nil
}

// RemoveProjectMember revokes the platform-side grant. Admin
// only. Note this does NOT clear a `project:<uuid>` dex group
// claim — those revocations happen on the IdP side.
func (s *weftServer) RemoveProjectMember(ctx context.Context, req *weftv1.RemoveProjectMemberRequest) (*weftv1.RemoveProjectMemberResponse, error) {
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
	return &weftv1.RemoveProjectMemberResponse{UserUuids: members}, nil
}

// ListProjectMembers returns the member UUIDs. AuthorizeProject
// gates the read — a project member can list their own peers
// without needing platform-admin, but a non-member can't probe
// who's in.
func (s *weftServer) ListProjectMembers(ctx context.Context, req *weftv1.ListProjectMembersRequest) (*weftv1.ListProjectMembersResponse, error) {
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
	return &weftv1.ListProjectMembersResponse{UserUuids: members}, nil
}

// WaitVM polls until the VM has an IP address or the timeout elapses.
func (s *weftServer) WaitVM(ctx context.Context, req *weftv1.WaitVMRequest) (*weftv1.WaitVMResponse, error) {
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
	return &weftv1.WaitVMResponse{Ip: ip}, nil
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

func rowToProto(r imock.Row) *weftv1.VMInfo {
	return &weftv1.VMInfo{
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

func stateToProto(s string) weftv1.VMState {
	switch s {
	case "running":
		return weftv1.VMState_VM_STATE_RUNNING
	case "stopped":
		return weftv1.VMState_VM_STATE_STOPPED
	case "not-created":
		return weftv1.VMState_VM_STATE_NOT_CREATED
	default:
		return weftv1.VMState_VM_STATE_UNSPECIFIED
	}
}

// applyDiskFileOps writes each DiskFileOp into the VM disk image and runs any
// requested post-copy trigger. Logic is delegated to the grub package.
func applyDiskFileOps(diskPath string, ops []*weftv1.DiskFileOp) error {
	fileOps := make([]grubpkg.FileOp, len(ops))
	for i, op := range ops {
		fileOps[i] = grubpkg.NewFileOp(op.Content, op.Dst, op.Trigger)
	}
	return grubpkg.ApplyFileOps(diskPath, fileOps)
}

// deleteDiskFileOps removes each DiskDeleteOp path from the disk image.
func deleteDiskFileOps(diskPath string, ops []*weftv1.DiskDeleteOp) error {
	dsts := make([]string, len(ops))
	for i, op := range ops {
		dsts[i] = op.Dst
	}
	return grubpkg.DeleteFileOps(diskPath, dsts)
}

// modDiskFileOps applies each DiskModOp in-place substitution to the disk image.
func modDiskFileOps(diskPath string, ops []*weftv1.DiskModOp) error {
	modOps := make([]grubpkg.ModOp, len(ops))
	for i, op := range ops {
		modOps[i] = grubpkg.NewModOp(op.Dst, op.Old, op.New)
	}
	return grubpkg.ModFileOps(diskPath, modOps)
}
