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
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clientv3 "go.etcd.io/etcd/client/v3"

	grubpkg "github.com/go-grub/grub"
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	sshtransport "github.com/grpc-transports/ssh"
	"github.com/openweft/weft"
	cloudinit "github.com/openweft/weft-cidata"
	wefthcl "github.com/openweft/weft-hcl"
	"github.com/openweft/weft-microvm-init/pkg/pod"
	weftv1 "github.com/openweft/weft-proto"
	agentv1 "github.com/openweft/weft-proto/agentv1"
	weftslognats "github.com/openweft/weft-slognats"
	"github.com/openweft/weft/auditlog"
	"github.com/openweft/weft/cmd/weft/admin"
	"github.com/openweft/weft/cmd/weft/az"
	"github.com/openweft/weft/cmd/weft/bucket"
	"github.com/openweft/weft/cmd/weft/clean"
	"github.com/openweft/weft/cmd/weft/completion"
	"github.com/openweft/weft/cmd/weft/dnsrecord"
	"github.com/openweft/weft/cmd/weft/dnszone"
	"github.com/openweft/weft/cmd/weft/events"
	"github.com/openweft/weft/cmd/weft/flavor"
	"github.com/openweft/weft/cmd/weft/floatingip"
	"github.com/openweft/weft/cmd/weft/host"
	"github.com/openweft/weft/cmd/weft/image"
	"github.com/openweft/weft/cmd/weft/instance"
	"github.com/openweft/weft/cmd/weft/loadbalancer"
	"github.com/openweft/weft/cmd/weft/login"
	"github.com/openweft/weft/cmd/weft/microvm"
	"github.com/openweft/weft/cmd/weft/monitor"
	"github.com/openweft/weft/cmd/weft/port"
	"github.com/openweft/weft/cmd/weft/network"
	"github.com/openweft/weft/cmd/weft/overlaycmd"
	"github.com/openweft/weft/cmd/weft/plugin"
	"github.com/openweft/weft/cmd/weft/project"
	"github.com/openweft/weft/cmd/weft/quota"
	"github.com/openweft/weft/cmd/weft/rack"
	"github.com/openweft/weft/cmd/weft/registry"
	"github.com/openweft/weft/cmd/weft/schedulingrule"
	"github.com/openweft/weft/cmd/weft/script"
	"github.com/openweft/weft/cmd/weft/securitygroup"
	"github.com/openweft/weft/cmd/weft/share"
	"github.com/openweft/weft/cmd/weft/sshkeycatalogue"
	"github.com/openweft/weft/cmd/weft/subnet"
	"github.com/openweft/weft/cmd/weft/tenant"
	"github.com/openweft/weft/cmd/weft/user"
	"github.com/openweft/weft/cmd/weft/volume"
	"github.com/openweft/weft/cmd/weft/wait"
	"github.com/openweft/weft/dhcpd"
	"github.com/openweft/weft/federation"
	"github.com/openweft/weft/firewallpub"
	"github.com/openweft/weft/floatingipnat"
	"github.com/openweft/weft/portqos"
	"github.com/openweft/weft/portsec"
	"github.com/openweft/weft/registryclient"
	"github.com/openweft/weft/zombiegc"
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
	defer panicReporter()
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// panicReporter is the top-level recover() that gives weft-doctor a
// shot at diagnosing a Go panic before the process exits. Without it
// the stack trace lands on stderr only ; weft-slognats fan-out only
// covers slog calls, so a runtime crash is invisible to anyone
// subscribing to the NATS log subject — including the AI log triage
// in `weft-doctor` ([[project_weft_doctor]]).
//
// We slog.Error with the full stack, flush stderr, then re-panic so
// systemd still sees the abnormal exit + the kernel still produces
// a coredump if the unit is configured for one.
//
// Off-systemd / non-NATS hosts get the same behaviour as before :
// slog.Default() falls back to stderr, which is where the stack was
// going to land anyway.
func panicReporter() {
	r := recover()
	if r == nil {
		return
	}
	stack := debug.Stack()
	slog.Error("weft : panic in main",
		"value", fmt.Sprintf("%v", r),
		"stack", string(stack),
	)
	// Give the NATS fan-out a beat to flush the line before the
	// runtime exits. 100ms is generous compared to RTT inside the
	// rack ; if the fan-out is wedged we lose the line but the
	// process exit goes through regardless.
	time.Sleep(100 * time.Millisecond)
	// Mirror the standard panic exit code so systemd's Restart= /
	// any wrapper still treats the exit as a real crash. Re-panic
	// rather than os.Exit so the runtime prints the canonical
	// stderr stack trace too — operators reading journalctl still
	// see what they expect.
	panic(r)
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
	root.AddCommand(newClusterCmd())
	root.AddCommand(newAttestCmd())

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
		monitor.Command(),
		az.Command(&socketPath, &sshSocket, &sshKey),
		rack.Command(&socketPath, &sshSocket, &sshKey),
		floatingip.Command(&socketPath, &sshSocket, &sshKey),
		port.Command(&socketPath, &sshSocket, &sshKey),
		subnet.Command(&socketPath, &sshSocket, &sshKey),
		loadbalancer.Command(&socketPath, &sshSocket, &sshKey),
		dnszone.Command(&socketPath, &sshSocket, &sshKey),
		dnsrecord.Command(&socketPath, &sshSocket, &sshKey),
		bucket.Command(&socketPath, &sshSocket, &sshKey),
		sshkeycatalogue.Command(&socketPath, &sshSocket, &sshKey),
		schedulingrule.Command(&socketPath, &sshSocket, &sshKey),
		registry.Command(&socketPath, &sshSocket, &sshKey),
		tenant.Command(&socketPath, &sshSocket, &sshKey),
		quota.Command(&socketPath, &sshSocket, &sshKey),
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
	var attestTPM bool
	var attestTPMDevice string
	var hypervisor string
	var tcpListen string
	var attestationEnabled bool
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
				socket:             socketPath,
				sshSocket:          sshSocket,
				sshAuthorizedKeys:  sshAuthorizedKeys,
				tcpListen:          tcpListen,
				attestationEnabled: attestationEnabled,
				configDir:          cfgDir,
				oidcIssuer:         oidcIssuer,
				oidcClientID:       oidcClientID,
				storageBackend:     storageBackend,
				eventBusBackend:    eventBusBackend,
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
			tgt.attestTPM = attestTPM
			tgt.attestTPMDevice = attestTPMDevice
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
	cmd.Flags().StringVar(&cfgDir, "config-dir", "state/hcl", "Path to HCL config directory")
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
	cmd.Flags().BoolVar(&attestationEnabled, "attestation-enabled", false, "Enable the TPM remote-attestation host-admission gate. When set, RegisterHost additionally requires the calling node to have completed the Enroll/Admit attestation handshake (its AK must be freshly admitted). Default OFF — RegisterHost uses the legacy OIDC RequireAdmin path only.")
	cmd.Flags().BoolVar(&serverMode, "server", false, "Run as control-plane server (no per-host driver dispatch). Default mode includes both.")
	cmd.Flags().BoolVar(&clientMode, "client", false, "Run as per-host driver runtime only. Requires --control-plane to point at the server.")
	cmd.Flags().StringVar(&controlPlaneURL, "control-plane", "", "URL of the Weft control-plane server (only consulted when --client is set).")
	cmd.Flags().BoolVar(&attestTPM, "attest-tpm", false, "(--client mode) Gate this node's RegisterHost behind the TPM remote-attestation handshake. When set, the agent opens the local TPM, derives an EK/AK, and runs the Enroll/Admit dance against the control-plane AttestationService before registering ; only a granted admission lets it register. Requires the control plane to run with --attestation-enabled and to have pre-trusted this node's EK. Default OFF — the agent never touches a TPM and bring-up is the legacy path.")
	cmd.Flags().StringVar(&attestTPMDevice, "attest-tpm-device", "/dev/tpmrm0", "(--client mode, with --attest-tpm) TPM character device the agent opens for attestation. Defaults to the Linux resource-manager channel /dev/tpmrm0.")

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
	mc := wefthcl.LoadWeftBlock(t.configDir)

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
	a := weft.NewWithKVStorage(filepath.Dir(t.configDir), sf.new, sf.newKV)
	a.SetPaths(mc.CachePath, mc.VMsPath)

	// Fan-out slog records to NATS as well as stderr so weft-doctor
	// can pick up WARN+ERROR events. WEFT_NATS_URL env unset → stderr-
	// only, no error. localHostUUID(a) is what the rest of the
	// daemon uses to identify itself ; same string in the subject.
	slogLogger, slogCloser := weftslognats.SetupFromEnv("weft.agent." + localHostUUID(a) + ".log")
	defer slogCloser.Close()
	slog.SetDefault(slogLogger)

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

	// Firewall pipeline (per docs/architecture/data-plane.md, "Stateful
	// firewall — per-VM nftables") : the publisher reacts to
	// security_group.* / port.* / network.default_security_groups_updated
	// / vm.created and pushes the effective ruleset on
	// "weft.firewall.<vm-uuid>" ; the status receiver subscribes to
	// the reverse "weft.firewall.<vm-uuid>.status" wildcard, decodes
	// each pod.FirewallStatus the in-VM agents emit every 10 s, and
	// re-publishes them as synthetic "firewall.status" platform
	// events so the webui's existing /api/events SSE pipe surfaces
	// live per-VM enforcement state with no new transport.
	// Both helpers no-op on the local in-process bus (no NATS, no
	// per-VM transport → no agents to push to / receive from).
	defer startFirewallPublisher(a, bf.bus, logger)()
	defer startFirewallStatusReceiver(bf.bus, logger)()

	// Floating-IP host-side NAT (per docs/architecture/data-plane.md,
	// "Floating IPs — host-side nftables NAT") : the Watcher
	// subscribes to floating_ip.* + vm.* + port.* events, recomputes
	// the current host's set of local FIP → private-IP mappings, and
	// drives the nftables reconciler (table "ip weft-fip-nat" with
	// prerouting DNAT + postrouting SNAT chains). No-op on darwin
	// dev hosts (StubReconciler records the desired state without
	// touching the kernel).
	defer startFloatingIPNATWatcher(a, bf.bus, logger)()

	// VM registry hot-reload : the etcd-backed Storage broadcasts
	// remote PUT/DELETE so this agent's in-memory hostIdx /
	// projectIdx / byUUID stay current with claims + creates +
	// deletes effected on other DCs. Without this, the cross-host
	// failover loop in agentrespawn can't see VMs whose host_uuid
	// was just flipped to a now-dead DC by a remote agent.
	defer startVMRegistryWatcher(a, logger)()

	// Project registry hot-reload : per-record V0.1.4 KV path
	// applies cross-DC Put/Delete events surgically to the in-
	// memory byUUID/nameIdx so renames + member changes on a
	// remote agent land here without a process restart.
	defer startProjectRegistryWatcher(a, logger)()

	// Host registry hot-reload : same pattern for the host inventory.
	// Critical for the cross-host failover loop which reads
	// ListVMsForHost to enumerate orphans on a HostDown event.
	defer startHostRegistryWatcher(a, logger)()

	// Scheduling-rule registry hot-reload : keeps the respawn
	// subscriber's selector cache fresh when an operator on another
	// DC creates / updates / deletes a rule without a process restart.
	defer startSchedulingRuleRegistryWatcher(a, logger)()

	// Respawn reconciler (per [[openweft_nominal_binding]] V0.1) :
	// subscribes to vm.state_changed + schedulingrule.* events and
	// drives weft/respawn's state machine for every VM bound by a
	// SchedulingRule with respawn.enabled=true. Honours grace_period,
	// max_restarts inside window, constant/exponential backoff.
	// V0.1 selector grammar is `vm.name=<name>` only — see
	// agentrespawn/agentrespawn.go for the V0.1.1 follow-ups.
	defer startRespawnSubscriber(a, bf.bus, sf.etcdClient, logger)()

	// Local-host heartbeat ticker : keep the registry's LastSeenAt
	// fresh for the locally-running host. The etcd liveness lease
	// (registered above) covers cross-host failover, but the
	// host-registry timestamp is a separate concern and ages
	// stale without explicit Heartbeat calls. 30s matches the
	// client-mode default in agent/agent.go.
	if hostUUID := localHostUUID(a); hostUUID != "" {
		defer startLocalHostHeartbeat(a, hostUUID, 30*time.Second, logger)()
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
	var promRegistry *prometheus.Registry
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
		promRegistry = reg
		// Network-plane reconciler metrics : floatingipnat (DNAT/SNAT
		// rules), firewallpub (security-group publishes), portsec
		// (anti-spoof), portqos (HTB shaping). Each package
		// implements an idempotent Register ; if the operator never
		// configured `--metrics-listen` the recordApply hot path
		// lazy-binds to prometheus.DefaultRegisterer instead (no
		// observation lost, just not scraped).
		if err := floatingipnat.Register(reg); err != nil {
			return fmt.Errorf("register floatingipnat metrics: %w", err)
		}
		if err := firewallpub.Register(reg); err != nil {
			return fmt.Errorf("register firewallpub metrics: %w", err)
		}
		if err := portsec.Register(reg); err != nil {
			return fmt.Errorf("register portsec metrics: %w", err)
		}
		if err := portqos.Register(reg); err != nil {
			return fmt.Errorf("register portqos metrics: %w", err)
		}
		if err := dhcpd.Register(reg); err != nil {
			return fmt.Errorf("register dhcpd metrics: %w", err)
		}
		// Cluster-topology gauge : weft_monitors_live = count of
		// etcd-coord liveness leases at /weft/coord/hosts/. Tracks the
		// number of healthy agent monitors operators can fail over to.
		// Refreshes every 5s. No-op when storage backend isn't etcd.
		defer startMonitorsGauge(reg, sf.etcdClient)()
		// Bus saturation gauge : weft_bus_dropped_total counts events
		// suppressed because a subscriber's 128-deep channel was full.
		// Operators alert on a non-zero rate (wedged consumer or
		// genuine burst overrunning the buffer).
		defer startBusDropsGauge(reg, bf.bus)()
		defer func() { _ = metricsCloser() }()
	}
	// V0.1.12 : VM zombie GC. Runs unconditionally — metrics are
	// optional but the cleanup must happen on every agent so the
	// registry stays bounded. When metrics are off, the Prometheus
	// gauge inside startZombieGC is a no-op (nil reg).
	zombieReconciler, zombieCancel := startZombieGC(promRegistry, a)
	defer zombieCancel()

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
	// Attestation gate (feature-flagged, default OFF). Built before the
	// weftServer so it can be wired onto it. When --attestation-enabled
	// is not set, attestGate is a disabled gate and every attestation
	// hook is inert ; RegisterHost's OFF path is untouched.
	var attestKV weft.KVStorage
	if t.attestationEnabled {
		if sf.newKV == nil {
			return fmt.Errorf("--attestation-enabled requires an etcd storage backend (storage.backend = etcd or embed-etcd) for the EK registry; the file backend has no per-record KV store")
		}
		attestKV = sf.newKV("attest")
	}
	attestGate, err := weftAttestGateFromConfig(context.Background(), t.attestationEnabled, attestKV)
	if err != nil {
		return fmt.Errorf("attestation gate: %w", err)
	}
	if t.attestationEnabled {
		logger.Printf("attestation gate ENABLED — TPM host admission required for RegisterHost")
	}
	srvImpl := &weftServer{
		cfgDir:           t.configDir,
		mc:               mc,
		adp:              a,
		dispatch:         dispatchSrv,
		localHostUUID:    localHostUUID(a),
		flavors:          flavorReg,
		scripts:          scriptReg,
		vmProps:          vmPropReg,
		uefiVars:         uefiReg,
		vmKeys:           sshKeyReg,
		zombieReconciler: zombieReconciler,
		attest:           attestGate,
		etcdCli:          sf.etcdClient,
	}
	weftv1.RegisterWeftAgentServer(srv, srvImpl)
	weftv1.RegisterAttestationServiceServer(srv, srvImpl)
	weftv1.RegisterAgentDispatchServer(srv, dispatchSrv)
	// AgentControlPlane (weft-proto agentv1) is the machine-to-machine
	// surface remote agents use for RegisterAgent / Heartbeat. Driver
	// dispatch still travels over AgentDispatch above ; AttachDrivers
	// is wired but accepts-Init-then-drains until the federation work
	// promotes it to the primary dispatch path.
	agentv1.RegisterAgentControlPlaneServer(srv, &agentControlPlaneServer{adp: a})

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

	// systemd integration : tell the unit we're ready (Type=notify
	// kept us in `activating` until now) and start the watchdog
	// ticker. Both no-op when $NOTIFY_SOCKET is unset (dev / off-
	// systemd hosts). See cmd/weft/sdnotify.go for the inline
	// sd_notify implementation.
	sdNotifyReady()
	startWatchdog(ctx, logger)

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
	weftv1.UnimplementedAttestationServiceServer
	cfgDir string
	mc     wefthcl.WeftBlock
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
	// federationPoller is the in-process peer-poll cache (see the
	// federation package). nil = federation not configured ;
	// ListFederationPeers returns an empty peer slice.
	federationPoller *federation.Poller
	// plugins is the catalogue + installed-instance + install
	// surface backing the Plugin RPCs. nil = plugin manager not
	// wired ; List* returns empty, Install returns Unavailable.
	plugins pluginManager
	// zombieReconciler is the running zombiegc instance ; the
	// GetZombieReport RPC queries its LastReport. nil in tests +
	// when the agent boots without `weft agent` (CLI mode), in
	// which case the RPC returns an empty report.
	zombieReconciler *zombiegc.Reconciler
	// attest is the TPM remote-attestation host-admission gate. nil
	// (the default) OR disabled means RegisterHost behaves exactly as
	// it does today (OIDC RequireAdmin only). When enabled, the four
	// AttestationService RPCs drive the verifier and RegisterHost
	// additionally requires a fresh admission for the caller's AK.
	// Set by run() only when --attestation-enabled is passed. See
	// attestation.go (the weft package) + attestation.go (cmd/weft).
	attest *weft.AttestationGate
	// etcdCli is the embedded/external etcd client when the agent is
	// running with storage=etcd. nil in single-host file-storage mode.
	// Used by DeleteHost to evict the orphaned liveness key from
	// /weft/coord/hosts/<uuid> so the watcher fires a HostDown event
	// instead of leaving a phantom lease referenced by no registry
	// entry.
	etcdCli *clientv3.Client
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
		// V0.1.9 : surface VM properties by cross-referencing the
		// inventory registry. The local-list path doesn't carry
		// Properties in `props` ; the inventory does. Empty for VMs
		// not in the registry (legacy local-only dev path).
		if rec, ok := s.adp.VMByName(projectUUID, name); ok {
			if len(rec.Properties) > 0 {
				info.Properties = make(map[string]string, len(rec.Properties))
				for k, lv := range rec.Properties {
					info.Properties[k] = lv
				}
			}
			if info.Uuid == "" {
				info.Uuid = rec.UUID
			}
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
	if rec, ok := s.adp.VMByName(projectUUID, req.Name); ok {
		if len(rec.Properties) > 0 {
			info.Properties = make(map[string]string, len(rec.Properties))
			for k, lv := range rec.Properties {
				info.Properties[k] = lv
			}
		}
		if info.Uuid == "" {
			info.Uuid = rec.UUID
		}
	}
	return &weftv1.VMStatusResponse{Vm: info}, nil
}

// protoGPUsToNative converts the wire-level []*weftv1.GPURequest
// shape into the Go-native []weft.GPURequest the adapter API
// expects (EnforceTenantQuotaForGPU, scheduler entry points).
// Returns nil for nil/empty inputs so call sites can pass the
// result through unconditionally.
func protoGPUsToNative(in []*weftv1.GPURequest) []weft.GPURequest {
	if len(in) == 0 {
		return nil
	}
	out := make([]weft.GPURequest, 0, len(in))
	for _, g := range in {
		if g == nil {
			continue
		}
		out = append(out, weft.GPURequest{
			Vendor:   g.Vendor,
			Model:    g.Model,
			Count:    int(g.Count),
			MIGSlice: g.MigSlice,
		})
	}
	return out
}

// protoPCIsToNative is the PCI sibling of protoGPUsToNative.
// Converts wire-level []*weftv1.PCIPassthroughRequest into the
// Go-native []weft.PCIRequest the adapter API expects.
func protoPCIsToNative(in []*weftv1.PCIPassthroughRequest) []weft.PCIRequest {
	if len(in) == 0 {
		return nil
	}
	out := make([]weft.PCIRequest, 0, len(in))
	for _, p := range in {
		if p == nil {
			continue
		}
		out = append(out, weft.PCIRequest{
			VendorID: p.VendorId,
			DeviceID: p.DeviceId,
			Count:    int(p.Count),
		})
	}
	return out
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

// RestartVM is the atomic Stop-then-Start RPC the TUI calls on `R`
// and the CLI on `weft instance restart`. We deliberately do not
// dispatch separately for the two legs : the VM stays pinned to the
// same host (the legacy on-disk VM lookup does this implicitly ; the
// inventory lookup goes through s.adp.StartVM which redrives the
// same hypervisor). If StopVM fails we surface the error verbatim.
// If StartVM fails we leave the VM stopped — the operator sees the
// error in the status bar and can retry start ; this beats the
// previous client-side chain which left a half-state with no
// rollback signal.
func (s *weftServer) RestartVM(ctx context.Context, req *weftv1.RestartVMRequest) (*weftv1.RestartVMResponse, error) {
	logger.Printf("RestartVM name=%s project=%s", req.Name, req.Project)
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
		return nil, err
	}
	RecordRPCKind(ctx, s.adp.LookupKindForVM(req.Name))
	if err := s.adp.StopVM(req.Name); err != nil {
		logger.Printf("RestartVM %s: stop leg error: %v", req.Name, err)
		return nil, status.Errorf(codes.Internal, "restart vm (stop): %v", err)
	}
	if err := s.adp.StartVM(req.Name, ""); err != nil {
		logger.Printf("RestartVM %s: start leg error: %v", req.Name, err)
		return nil, status.Errorf(codes.Internal, "restart vm (start): %v", err)
	}
	return &weftv1.RestartVMResponse{}, nil
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
	// GPU aggregate cap. CreateVMRequest now carries RequestedGpus
	// (since weft-proto v0.3.0) — feed it through so the quota
	// check accounts for the *delta* this admission would add on
	// top of the already-allocated total. Empty / nil is fine and
	// re-checks the running total against the cap.
	if err := s.adp.EnforceTenantQuotaForGPU(projUUID, protoGPUsToNative(req.RequestedGpus)); err != nil {
		return nil, err
	}
	// PCI aggregate cap, same shape as GPU. CreateVMRequest now
	// carries RequestedPci (since weft-proto v0.3.0) ; thread it
	// through so admission accounts for the requested delta.
	if err := s.adp.EnforceTenantQuotaForPCI(projUUID, protoPCIsToNative(req.RequestedPci)); err != nil {
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
	ociMap := wefthcl.LoadOCIFroms(cfgDir)
	imgSet := map[string]struct{}{}
	for _, v := range ociMap {
		if v != "" {
			imgSet[v] = struct{}{}
		}
	}
	rows, err := wefthcl.BuildRowsFromConfig(cfgDir, "", map[string]map[string]interface{}{}, ociMap)
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
	ociMap := wefthcl.LoadOCIFroms(cfgDir)
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
	// GPU aggregate cap, same shape as the CreateVM site.
	// RegisterMicroVMRequest carries RequestedGpus since weft-proto
	// v0.3.0 — feed it through so the quota check accounts for the
	// delta this microVM boot would add on top of the running total.
	if err := s.adp.EnforceTenantQuotaForGPU(projUUID, protoGPUsToNative(req.RequestedGpus)); err != nil {
		return nil, err
	}
	// PCI aggregate cap, same shape as the CreateVM site.
	// RegisterMicroVMRequest carries RequestedPci since weft-proto
	// v0.3.0 — thread it through for delta-aware admission.
	if err := s.adp.EnforceTenantQuotaForPCI(projUUID, protoPCIsToNative(req.RequestedPci)); err != nil {
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

// PublishShareToProject resolves the project's VMs and fans the share mount
// (or unmount) out to each over the event bus. The control plane reaches
// every VM regardless of host via NATS, so there's no per-host dispatch.
// AttachShareToProject is part of VZAdapter ; no defensive type assertion
// needed (mock adapters get a compile-time error if they forget to wire it).
func (s *weftServer) PublishShareToProject(ctx context.Context, req *weftv1.PublishShareToProjectRequest) (*weftv1.PublishShareToProjectResponse, error) {
	if req.Mount == nil {
		return nil, status.Errorf(codes.InvalidArgument, "mount is required")
	}
	projUUID, err := s.adp.AuthorizeProject(ctx, req.ProjectUuid)
	if err != nil {
		return nil, err
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
	n, err := s.adp.AttachShareToProject(projUUID, m)
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

// startLocalHostHeartbeat keeps the host registry's LastSeenAt fresh
// for the locally-running host by calling adapter.HeartbeatHost on a
// ticker. Returns a cancel func that stops the goroutine on agent
// shutdown. The etcd HostLiveness lease handles cross-host failover
// already — this just closes the lease/heartbeat decoupling gap so
// `weft host ls` doesn't show 11h-stale timestamps for hosts that
// are clearly alive (cf. dcN-r2-h1 finding during 2026-06-19 live
// re-validation).
func startLocalHostHeartbeat(adp weft.VZAdapter, hostUUID string, interval time.Duration, logger *log.Logger) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		// Fire once immediately so the first heartbeat lands without
		// waiting `interval` — operator reads `weft host ls` right
		// after agent start and expects a fresh LastSeenAt.
		if err := adp.HeartbeatHost(hostUUID); err != nil {
			logger.Printf("local heartbeat: initial HeartbeatHost(%s) failed: %v", hostUUID, err)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := adp.HeartbeatHost(hostUUID); err != nil {
					logger.Printf("local heartbeat: HeartbeatHost(%s) failed: %v", hostUUID, err)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
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
		TenantUuid:      p.TenantUUID,
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

// SetProjectTenant binds (or unbinds when tenant_uuid is empty) the
// project to a parent tenant. Powers the GetProjectQuota
// siblings_total + tenant_cap aggregation (commit d9f9d46ea +
// 5a93f38a4) without operators needing to hand-edit projects.hcl.
func (s *weftServer) SetProjectTenant(ctx context.Context, req *weftv1.SetProjectTenantRequest) (*weftv1.SetProjectTenantResponse, error) {
	if err := weft.RequireAdmin(ctx, "set project tenant"); err != nil {
		return nil, err
	}
	if req.ProjectUuid == "" {
		return nil, status.Error(codes.InvalidArgument, "project_uuid is required")
	}
	if err := s.adp.SetProjectTenant(req.ProjectUuid, req.TenantUuid); err != nil {
		return nil, status.Errorf(codes.NotFound, "set project tenant: %v", err)
	}
	p, ok := s.adp.ProjectByUUID(req.ProjectUuid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "project %s not found after update", req.ProjectUuid)
	}
	logger.Printf("SetProjectTenant project=%s tenant=%s", req.ProjectUuid, req.TenantUuid)
	return &weftv1.SetProjectTenantResponse{Project: toProjectInfo(p)}, nil
}

// ---- Tenants (top-level multi-tenant boundary) --------------------
//
// Tenants are the optional umbrella above projects : a tenant owns
// N projects, M members (humans), K admins. The webui drives a fresh
// install through `weft tenant create` ; the CLI mirrors the same
// surface so a fresh cluster can be brought up SSH-only.

func (s *weftServer) ListTenants(_ context.Context, _ *weftv1.ListTenantsRequest) (*weftv1.ListTenantsResponse, error) {
	tenants := s.adp.Tenants()
	out := make([]*weftv1.TenantInfo, 0, len(tenants))
	for _, t := range tenants {
		out = append(out, toTenantInfo(t))
	}
	return &weftv1.ListTenantsResponse{Tenants: out}, nil
}

func (s *weftServer) CreateTenant(ctx context.Context, req *weftv1.CreateTenantRequest) (*weftv1.CreateTenantResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := weft.RequireAdmin(ctx, "create tenant"); err != nil {
		return nil, err
	}
	t, _, err := s.adp.CreateTenant(req.Name, req.Domain)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create tenant: %v", err)
	}
	logger.Printf("CreateTenant name=%s uuid=%s", t.Name, t.UUID)
	return &weftv1.CreateTenantResponse{Tenant: toTenantInfo(t)}, nil
}

func (s *weftServer) DeleteTenant(ctx context.Context, req *weftv1.DeleteTenantRequest) (*weftv1.DeleteTenantResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := weft.RequireAdmin(ctx, "delete tenant"); err != nil {
		return nil, err
	}
	if _, err := s.adp.DeleteTenant(req.Uuid); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "delete tenant: %v", err)
	}
	logger.Printf("DeleteTenant uuid=%s", req.Uuid)
	return &weftv1.DeleteTenantResponse{}, nil
}

func (s *weftServer) AddTenantAdmin(ctx context.Context, req *weftv1.AddTenantAdminRequest) (*weftv1.AddTenantAdminResponse, error) {
	if req.TenantUuid == "" || req.Email == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_uuid + email required")
	}
	if err := weft.RequireAdmin(ctx, "add tenant admin"); err != nil {
		return nil, err
	}
	if _, err := s.adp.AddTenantAdmin(req.TenantUuid, req.Email); err != nil {
		return nil, status.Errorf(codes.NotFound, "add tenant admin: %v", err)
	}
	return &weftv1.AddTenantAdminResponse{}, nil
}

func (s *weftServer) RemoveTenantAdmin(ctx context.Context, req *weftv1.RemoveTenantAdminRequest) (*weftv1.RemoveTenantAdminResponse, error) {
	if req.TenantUuid == "" || req.Email == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_uuid + email required")
	}
	if err := weft.RequireAdmin(ctx, "remove tenant admin"); err != nil {
		return nil, err
	}
	if _, err := s.adp.RemoveTenantAdmin(req.TenantUuid, req.Email); err != nil {
		return nil, status.Errorf(codes.NotFound, "remove tenant admin: %v", err)
	}
	return &weftv1.RemoveTenantAdminResponse{}, nil
}

func (s *weftServer) AddTenantMember(ctx context.Context, req *weftv1.AddTenantMemberRequest) (*weftv1.AddTenantMemberResponse, error) {
	if req.TenantUuid == "" || req.Email == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_uuid + email required")
	}
	if err := weft.RequireAdmin(ctx, "add tenant member"); err != nil {
		return nil, err
	}
	if _, err := s.adp.AddTenantMember(req.TenantUuid, req.Email, req.Groups); err != nil {
		return nil, status.Errorf(codes.NotFound, "add tenant member: %v", err)
	}
	return &weftv1.AddTenantMemberResponse{}, nil
}

func (s *weftServer) RemoveTenantMember(ctx context.Context, req *weftv1.RemoveTenantMemberRequest) (*weftv1.RemoveTenantMemberResponse, error) {
	if req.TenantUuid == "" || req.Email == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_uuid + email required")
	}
	if err := weft.RequireAdmin(ctx, "remove tenant member"); err != nil {
		return nil, err
	}
	if _, err := s.adp.RemoveTenantMember(req.TenantUuid, req.Email); err != nil {
		return nil, status.Errorf(codes.NotFound, "remove tenant member: %v", err)
	}
	return &weftv1.RemoveTenantMemberResponse{}, nil
}

// toTenantInfo projects a weft.Tenant onto the proto wire shape.
// Projects count = 0 today : projects don't carry a TenantUUID column
// yet, so we can't derive how many belong to a tenant. Slated for the
// next schema iteration ; clients should treat 0 as "not yet populated"
// rather than "no projects".
func toTenantInfo(t weft.Tenant) *weftv1.TenantInfo {
	return &weftv1.TenantInfo{
		Uuid:            t.UUID,
		Name:            t.Name,
		Domain:          t.Domain,
		Status:          t.Status,
		CreatedAtUnixNs: t.CreatedAt.UnixNano(),
		Projects:        0,
		Members:         int32(len(t.Members)),
		Admins:          int32(len(t.Admins)),
	}
}

// ---- AvailabilityZones (inventory tier 1) -------------------------
//
// AZs are the top tier of the inventory hierarchy (AZ → Rack → Host).
// The proto's immutable `code` is the value scheduling rules + host
// registrations carry around to pin placement ; `name` / `region` /
// `status` are operator-mutable via UpdateAZ.
//
// Read RPCs are open to any authenticated caller (every CreateVMModal
// + scheduler view needs the AZ list) ; mutations are admin-only.

func (s *weftServer) ListAZs(_ context.Context, _ *weftv1.ListAZsRequest) (*weftv1.ListAZsResponse, error) {
	azs := s.adp.AZs()
	out := make([]*weftv1.AZInfo, 0, len(azs))
	for _, a := range azs {
		out = append(out, toAZInfo(s.adp, a))
	}
	return &weftv1.ListAZsResponse{Azs: out}, nil
}

func (s *weftServer) GetAZ(_ context.Context, req *weftv1.GetAZRequest) (*weftv1.GetAZResponse, error) {
	if req.Uuid == "" && req.Code == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid or code is required")
	}
	var (
		az weft.AZ
		ok bool
	)
	if req.Uuid != "" {
		az, ok = s.adp.AZByUUID(req.Uuid)
	} else {
		az, ok = s.adp.AZByCode(req.Code)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "az not found")
	}
	return &weftv1.GetAZResponse{Az: toAZInfo(s.adp, az)}, nil
}

func (s *weftServer) CreateAZ(ctx context.Context, req *weftv1.CreateAZRequest) (*weftv1.CreateAZResponse, error) {
	if req.Code == "" {
		return nil, status.Error(codes.InvalidArgument, "code is required")
	}
	if err := weft.RequireAdmin(ctx, "create az"); err != nil {
		return nil, err
	}
	az, created, err := s.adp.CreateAZ(req.Code, req.Name, req.Region, req.Status)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create az: %v", err)
	}
	logger.Printf("CreateAZ code=%s uuid=%s created=%v", az.Code, az.UUID, created)
	return &weftv1.CreateAZResponse{Az: toAZInfo(s.adp, az), Created: created}, nil
}

func (s *weftServer) UpdateAZ(ctx context.Context, req *weftv1.UpdateAZRequest) (*weftv1.UpdateAZResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := weft.RequireAdmin(ctx, "update az"); err != nil {
		return nil, err
	}
	az, err := s.adp.UpdateAZ(req.Uuid, req.Name, req.Region, req.Status)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "update az: %v", err)
	}
	logger.Printf("UpdateAZ uuid=%s code=%s", az.UUID, az.Code)
	return &weftv1.UpdateAZResponse{Az: toAZInfo(s.adp, az)}, nil
}

func (s *weftServer) DeleteAZ(ctx context.Context, req *weftv1.DeleteAZRequest) (*weftv1.DeleteAZResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := weft.RequireAdmin(ctx, "delete az"); err != nil {
		return nil, err
	}
	blockedRacks, blockedHosts, err := s.adp.DeleteAZ(req.Uuid)
	if err != nil {
		return &weftv1.DeleteAZResponse{
			BlockedByRacks: blockedRacks,
			BlockedByHosts: blockedHosts,
		}, status.Errorf(codes.FailedPrecondition, "delete az: %v", err)
	}
	logger.Printf("DeleteAZ uuid=%s", req.Uuid)
	return &weftv1.DeleteAZResponse{DeletedUuid: req.Uuid}, nil
}

// ---- Racks (inventory tier 2) -------------------------------------

func (s *weftServer) ListRacks(_ context.Context, req *weftv1.ListRacksRequest) (*weftv1.ListRacksResponse, error) {
	racks := s.adp.Racks(req.AzUuid)
	out := make([]*weftv1.RackInfo, 0, len(racks))
	for _, r := range racks {
		out = append(out, toRackInfo(s.adp, r))
	}
	return &weftv1.ListRacksResponse{Racks: out}, nil
}

func (s *weftServer) GetRack(_ context.Context, req *weftv1.GetRackRequest) (*weftv1.GetRackResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	rk, ok := s.adp.RackByUUID(req.Uuid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "rack not found")
	}
	return &weftv1.GetRackResponse{Rack: toRackInfo(s.adp, rk)}, nil
}

func (s *weftServer) CreateRack(ctx context.Context, req *weftv1.CreateRackRequest) (*weftv1.CreateRackResponse, error) {
	if req.AzUuid == "" || req.Code == "" {
		return nil, status.Error(codes.InvalidArgument, "az_uuid and code are required")
	}
	if err := weft.RequireAdmin(ctx, "create rack"); err != nil {
		return nil, err
	}
	rk, created, err := s.adp.CreateRack(req.AzUuid, req.Code, req.Name, req.Status, req.HeightU)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "create rack: %v", err)
	}
	logger.Printf("CreateRack az=%s code=%s uuid=%s created=%v", rk.AZUUID, rk.Code, rk.UUID, created)
	return &weftv1.CreateRackResponse{Rack: toRackInfo(s.adp, rk), Created: created}, nil
}

func (s *weftServer) UpdateRack(ctx context.Context, req *weftv1.UpdateRackRequest) (*weftv1.UpdateRackResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := weft.RequireAdmin(ctx, "update rack"); err != nil {
		return nil, err
	}
	rk, err := s.adp.UpdateRack(req.Uuid, req.Name, req.Status, req.HeightU)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "update rack: %v", err)
	}
	logger.Printf("UpdateRack uuid=%s code=%s", rk.UUID, rk.Code)
	return &weftv1.UpdateRackResponse{Rack: toRackInfo(s.adp, rk)}, nil
}

func (s *weftServer) DeleteRack(ctx context.Context, req *weftv1.DeleteRackRequest) (*weftv1.DeleteRackResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := weft.RequireAdmin(ctx, "delete rack"); err != nil {
		return nil, err
	}
	blockedHosts, err := s.adp.DeleteRack(req.Uuid)
	if err != nil {
		return &weftv1.DeleteRackResponse{BlockedByHosts: blockedHosts}, status.Errorf(codes.FailedPrecondition, "delete rack: %v", err)
	}
	logger.Printf("DeleteRack uuid=%s", req.Uuid)
	return &weftv1.DeleteRackResponse{DeletedUuid: req.Uuid}, nil
}

// toAZInfo projects an AZ + the derived rack/host counts onto the
// wire shape. The counts are server-side derived ; clients never
// fill them on writes.
func toAZInfo(adp weft.VZAdapter, a weft.AZ) *weftv1.AZInfo {
	return &weftv1.AZInfo{
		Uuid:            a.UUID,
		Code:            a.Code,
		Name:            a.Name,
		Region:          a.Region,
		Status:          a.Status,
		CreatedAtUnixNs: a.CreatedAt.UnixNano(),
		Racks:           adp.AZRackCount(a.UUID),
		Hosts:           adp.AZHostCount(a.UUID),
	}
}

// toRackInfo projects a Rack + the derived host count onto the wire
// shape.
func toRackInfo(adp weft.VZAdapter, r weft.Rack) *weftv1.RackInfo {
	return &weftv1.RackInfo{
		Uuid:            r.UUID,
		AzUuid:          r.AZUUID,
		Code:            r.Code,
		Name:            r.Name,
		Status:          r.Status,
		HeightU:         r.HeightU,
		CreatedAtUnixNs: r.CreatedAt.UnixNano(),
		Hosts:           adp.RackHostCount(r.UUID),
	}
}

// ---- Subnets (proto v0.8.0) ---------------------------------------
//
// Subnets are per-network IP scopes. Parent is `network_uuid` ; the
// project is denormalised from the parent at create time so the
// proto's response carries it for ACL display. Read is open to any
// caller ; mutations are admin-only (the Tier-3 noun set sits with
// the network-plane build-out, no per-project ACL surface yet).

func (s *weftServer) ListSubnets(_ context.Context, req *weftv1.ListSubnetsRequest) (*weftv1.ListSubnetsResponse, error) {
	subnets := s.adp.Subnets(req.NetworkUuid)
	out := make([]*weftv1.SubnetInfo, 0, len(subnets))
	for _, sn := range subnets {
		out = append(out, toSubnetInfo(sn))
	}
	return &weftv1.ListSubnetsResponse{Subnets: out}, nil
}

func (s *weftServer) GetSubnet(_ context.Context, req *weftv1.GetSubnetRequest) (*weftv1.GetSubnetResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	sn, ok := s.adp.SubnetByUUID(req.Uuid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "subnet not found")
	}
	return &weftv1.GetSubnetResponse{Subnet: toSubnetInfo(sn)}, nil
}

func (s *weftServer) CreateSubnet(ctx context.Context, req *weftv1.CreateSubnetRequest) (*weftv1.CreateSubnetResponse, error) {
	if req.NetworkUuid == "" || req.Cidr == "" {
		return nil, status.Error(codes.InvalidArgument, "network_uuid and cidr are required")
	}
	// Subnets are project-scoped via their parent network. Resolve
	// the network's owning project, then defer to AuthorizeProject
	// (dev / platform-admin / project-member all pass).
	net, ok := s.adp.NetworkByUUID(req.NetworkUuid)
	if !ok {
		return nil, status.Errorf(codes.PermissionDenied, "no access to network %s", req.NetworkUuid)
	}
	if _, err := s.adp.AuthorizeProject(ctx, net.ProjectUUID); err != nil {
		return nil, err
	}
	sn, created, err := s.adp.CreateSubnet(req.NetworkUuid, req.Name, req.Description, req.Cidr, req.Gateway, req.DnsServers)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "create subnet: %v", err)
	}
	logger.Printf("CreateSubnet network=%s cidr=%s uuid=%s created=%v", sn.NetworkUUID, sn.CIDR, sn.UUID, created)
	return &weftv1.CreateSubnetResponse{Subnet: toSubnetInfo(sn), Created: created}, nil
}

func (s *weftServer) UpdateSubnet(ctx context.Context, req *weftv1.UpdateSubnetRequest) (*weftv1.UpdateSubnetResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if _, err := s.authSubnet(ctx, req.Uuid); err != nil {
		return nil, err
	}
	sn, err := s.adp.UpdateSubnet(req.Uuid, req.Name, req.Description, req.Gateway, req.ClearDnsServers, req.DnsServers)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "update subnet: %v", err)
	}
	logger.Printf("UpdateSubnet uuid=%s", sn.UUID)
	return &weftv1.UpdateSubnetResponse{Subnet: toSubnetInfo(sn)}, nil
}

func (s *weftServer) DeleteSubnet(ctx context.Context, req *weftv1.DeleteSubnetRequest) (*weftv1.DeleteSubnetResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if _, err := s.authSubnet(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.DeleteSubnet(req.Uuid); err != nil {
		return nil, status.Errorf(codes.NotFound, "delete subnet: %v", err)
	}
	logger.Printf("DeleteSubnet uuid=%s", req.Uuid)
	return &weftv1.DeleteSubnetResponse{DeletedUuid: req.Uuid}, nil
}

// authSubnet resolves a subnet UUID to its owning project via the
// parent network, then delegates to AuthorizeProject. Hides
// cross-project existence by returning PermissionDenied for both
// unknown and unauthorised cases, matching authNetwork's pattern.
func (s *weftServer) authSubnet(ctx context.Context, uuid string) (weft.Subnet, error) {
	sn, ok := s.adp.SubnetByUUID(uuid)
	if !ok {
		return weft.Subnet{}, status.Errorf(codes.PermissionDenied, "no access to subnet %s", uuid)
	}
	net, ok := s.adp.NetworkByUUID(sn.NetworkUUID)
	if !ok {
		return weft.Subnet{}, status.Errorf(codes.PermissionDenied, "no access to subnet %s", uuid)
	}
	if _, err := s.adp.AuthorizeProject(ctx, net.ProjectUUID); err != nil {
		return weft.Subnet{}, err
	}
	return sn, nil
}

func toSubnetInfo(sn weft.Subnet) *weftv1.SubnetInfo {
	return &weftv1.SubnetInfo{
		Uuid:            sn.UUID,
		NetworkUuid:     sn.NetworkUUID,
		ProjectUuid:     sn.ProjectUUID,
		Name:            sn.Name,
		Description:     sn.Description,
		Cidr:            sn.CIDR,
		Gateway:         sn.Gateway,
		DnsServers:      append([]string(nil), sn.DNSServers...),
		CreatedAtUnixNs: sn.CreatedAt.UnixNano(),
	}
}

// ---- LoadBalancers (proto v0.8.0) ---------------------------------
//
// Project-scoped VIPs. SetLoadBalancerBackends atomically replaces
// the backend list ; DeleteLoadBalancer refuses while a FloatingIP
// still maps to the VIP.

func (s *weftServer) ListLoadBalancers(_ context.Context, req *weftv1.ListLoadBalancersRequest) (*weftv1.ListLoadBalancersResponse, error) {
	projectUUID := s.resolveProjectUUID(req.Project)
	lbs := s.adp.LoadBalancers(projectUUID)
	out := make([]*weftv1.LoadBalancerInfo, 0, len(lbs))
	for _, lb := range lbs {
		out = append(out, toLBInfo(lb))
	}
	return &weftv1.ListLoadBalancersResponse{LoadBalancers: out}, nil
}

func (s *weftServer) GetLoadBalancer(_ context.Context, req *weftv1.GetLoadBalancerRequest) (*weftv1.GetLoadBalancerResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	lb, ok := s.adp.LoadBalancerByUUID(req.Uuid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "loadbalancer not found")
	}
	return &weftv1.GetLoadBalancerResponse{LoadBalancer: toLBInfo(lb)}, nil
}

func (s *weftServer) CreateLoadBalancer(ctx context.Context, req *weftv1.CreateLoadBalancerRequest) (*weftv1.CreateLoadBalancerResponse, error) {
	if req.Name == "" || req.ListenAddr == "" || req.Protocol == "" {
		return nil, status.Error(codes.InvalidArgument, "name, listen_addr, protocol are required")
	}
	projectUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	backends := fromLBBackends(req.Backends)
	lb, created, err := s.adp.CreateLoadBalancer(projectUUID, req.Name, req.ListenAddr, req.Protocol, backends)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "create loadbalancer: %v", err)
	}
	logger.Printf("CreateLoadBalancer project=%s name=%s uuid=%s created=%v", lb.ProjectUUID, lb.Name, lb.UUID, created)
	return &weftv1.CreateLoadBalancerResponse{LoadBalancer: toLBInfo(lb), Created: created}, nil
}

func (s *weftServer) UpdateLoadBalancer(ctx context.Context, req *weftv1.UpdateLoadBalancerRequest) (*weftv1.UpdateLoadBalancerResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.authLoadBalancer(ctx, req.Uuid); err != nil {
		return nil, err
	}
	lb, err := s.adp.UpdateLoadBalancer(req.Uuid, req.Name, req.ListenAddr, req.Protocol)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "update loadbalancer: %v", err)
	}
	logger.Printf("UpdateLoadBalancer uuid=%s", lb.UUID)
	return &weftv1.UpdateLoadBalancerResponse{LoadBalancer: toLBInfo(lb)}, nil
}

func (s *weftServer) SetLoadBalancerBackends(ctx context.Context, req *weftv1.SetLoadBalancerBackendsRequest) (*weftv1.SetLoadBalancerBackendsResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.authLoadBalancer(ctx, req.Uuid); err != nil {
		return nil, err
	}
	lb, err := s.adp.SetLoadBalancerBackends(req.Uuid, fromLBBackends(req.Backends))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "set loadbalancer backends: %v", err)
	}
	logger.Printf("SetLoadBalancerBackends uuid=%s count=%d", lb.UUID, len(lb.Backends))
	return &weftv1.SetLoadBalancerBackendsResponse{LoadBalancer: toLBInfo(lb)}, nil
}

func (s *weftServer) DeleteLoadBalancer(ctx context.Context, req *weftv1.DeleteLoadBalancerRequest) (*weftv1.DeleteLoadBalancerResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.authLoadBalancer(ctx, req.Uuid); err != nil {
		return nil, err
	}
	blockedFIPs, err := s.adp.DeleteLoadBalancer(req.Uuid)
	if err != nil {
		return &weftv1.DeleteLoadBalancerResponse{BlockedByFips: blockedFIPs}, status.Errorf(codes.FailedPrecondition, "delete loadbalancer: %v", err)
	}
	logger.Printf("DeleteLoadBalancer uuid=%s", req.Uuid)
	return &weftv1.DeleteLoadBalancerResponse{DeletedUuid: req.Uuid}, nil
}

func toLBInfo(lb weft.LoadBalancer) *weftv1.LoadBalancerInfo {
	backends := make([]*weftv1.LBBackend, 0, len(lb.Backends))
	for _, b := range lb.Backends {
		backends = append(backends, &weftv1.LBBackend{Address: b.Address, Weight: b.Weight})
	}
	return &weftv1.LoadBalancerInfo{
		Uuid:            lb.UUID,
		ProjectUuid:     lb.ProjectUUID,
		Name:            lb.Name,
		ListenAddr:      lb.ListenAddr,
		Protocol:        lb.Protocol,
		Backends:        backends,
		CreatedAtUnixNs: lb.CreatedAt.UnixNano(),
	}
}

func fromLBBackends(in []*weftv1.LBBackend) []weft.LBBackend {
	out := make([]weft.LBBackend, 0, len(in))
	for _, b := range in {
		if b == nil {
			continue
		}
		out = append(out, weft.LBBackend{Address: b.Address, Weight: b.Weight})
	}
	return out
}

// authLoadBalancer resolves a LB UUID to its owning project and
// delegates to AuthorizeProject. Hides cross-project existence by
// returning PermissionDenied on unknown UUID.
func (s *weftServer) authLoadBalancer(ctx context.Context, uuid string) error {
	lb, ok := s.adp.LoadBalancerByUUID(uuid)
	if !ok {
		return status.Errorf(codes.PermissionDenied, "no access to loadbalancer %s", uuid)
	}
	if _, err := s.adp.AuthorizeProject(ctx, lb.ProjectUUID); err != nil {
		return err
	}
	return nil
}

// ---- DNS Zones (proto v0.8.0) -------------------------------------

func (s *weftServer) ListDNSZones(_ context.Context, req *weftv1.ListDNSZonesRequest) (*weftv1.ListDNSZonesResponse, error) {
	projectUUID := s.resolveProjectUUID(req.Project)
	zones := s.adp.DNSZones(projectUUID)
	out := make([]*weftv1.DNSZoneInfo, 0, len(zones))
	for _, z := range zones {
		out = append(out, toDNSZoneInfo(s.adp, z))
	}
	return &weftv1.ListDNSZonesResponse{Zones: out}, nil
}

func (s *weftServer) GetDNSZone(_ context.Context, req *weftv1.GetDNSZoneRequest) (*weftv1.GetDNSZoneResponse, error) {
	if req.Uuid == "" && req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid or name is required")
	}
	var (
		z  weft.DNSZone
		ok bool
	)
	if req.Uuid != "" {
		z, ok = s.adp.DNSZoneByUUID(req.Uuid)
	} else {
		z, ok = s.adp.DNSZoneByName(req.Name)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "dns zone not found")
	}
	return &weftv1.GetDNSZoneResponse{Zone: toDNSZoneInfo(s.adp, z)}, nil
}

func (s *weftServer) CreateDNSZone(ctx context.Context, req *weftv1.CreateDNSZoneRequest) (*weftv1.CreateDNSZoneResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	projectUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	z, created, err := s.adp.CreateDNSZone(projectUUID, req.Name, req.SoaEmail, req.Ttl)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create dns zone: %v", err)
	}
	logger.Printf("CreateDNSZone name=%s uuid=%s created=%v", z.Name, z.UUID, created)
	return &weftv1.CreateDNSZoneResponse{Zone: toDNSZoneInfo(s.adp, z), Created: created}, nil
}

func (s *weftServer) UpdateDNSZone(ctx context.Context, req *weftv1.UpdateDNSZoneRequest) (*weftv1.UpdateDNSZoneResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.authDNSZone(ctx, req.Uuid); err != nil {
		return nil, err
	}
	z, err := s.adp.UpdateDNSZone(req.Uuid, req.SoaEmail, req.Ttl)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "update dns zone: %v", err)
	}
	logger.Printf("UpdateDNSZone uuid=%s", z.UUID)
	return &weftv1.UpdateDNSZoneResponse{Zone: toDNSZoneInfo(s.adp, z)}, nil
}

func (s *weftServer) DeleteDNSZone(ctx context.Context, req *weftv1.DeleteDNSZoneRequest) (*weftv1.DeleteDNSZoneResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.authDNSZone(ctx, req.Uuid); err != nil {
		return nil, err
	}
	blocked, err := s.adp.DeleteDNSZone(req.Uuid)
	if err != nil {
		return &weftv1.DeleteDNSZoneResponse{BlockedByRecords: blocked}, status.Errorf(codes.FailedPrecondition, "delete dns zone: %v", err)
	}
	logger.Printf("DeleteDNSZone uuid=%s", req.Uuid)
	return &weftv1.DeleteDNSZoneResponse{DeletedUuid: req.Uuid}, nil
}

func toDNSZoneInfo(adp weft.VZAdapter, z weft.DNSZone) *weftv1.DNSZoneInfo {
	return &weftv1.DNSZoneInfo{
		Uuid:            z.UUID,
		ProjectUuid:     z.ProjectUUID,
		Name:            z.Name,
		SoaEmail:        z.SOAEmail,
		Ttl:             z.TTL,
		Records:         adp.DNSZoneRecordCount(z.UUID),
		CreatedAtUnixNs: z.CreatedAt.UnixNano(),
	}
}

// authDNSZone resolves a zone UUID to its owning project and
// delegates to AuthorizeProject.
func (s *weftServer) authDNSZone(ctx context.Context, uuid string) error {
	z, ok := s.adp.DNSZoneByUUID(uuid)
	if !ok {
		return status.Errorf(codes.PermissionDenied, "no access to dns zone %s", uuid)
	}
	if _, err := s.adp.AuthorizeProject(ctx, z.ProjectUUID); err != nil {
		return err
	}
	return nil
}

// authDNSRecord resolves a record UUID → parent zone → owning
// project, then delegates to AuthorizeProject.
func (s *weftServer) authDNSRecord(ctx context.Context, uuid string) error {
	rec, ok := s.adp.DNSRecordByUUID(uuid)
	if !ok {
		return status.Errorf(codes.PermissionDenied, "no access to dns record %s", uuid)
	}
	return s.authDNSZone(ctx, rec.ZoneUUID)
}

// ---- DNS Records (proto v0.8.0) -----------------------------------

func (s *weftServer) ListDNSRecords(_ context.Context, req *weftv1.ListDNSRecordsRequest) (*weftv1.ListDNSRecordsResponse, error) {
	if req.ZoneUuid == "" {
		return nil, status.Error(codes.InvalidArgument, "zone_uuid is required")
	}
	records := s.adp.DNSRecords(req.ZoneUuid)
	out := make([]*weftv1.DNSRecordInfo, 0, len(records))
	for _, rec := range records {
		out = append(out, toDNSRecordInfo(rec))
	}
	return &weftv1.ListDNSRecordsResponse{Records: out}, nil
}

func (s *weftServer) CreateDNSRecord(ctx context.Context, req *weftv1.CreateDNSRecordRequest) (*weftv1.CreateDNSRecordResponse, error) {
	if req.ZoneUuid == "" || req.Name == "" || req.Type == "" || req.Value == "" {
		return nil, status.Error(codes.InvalidArgument, "zone_uuid, name, type, value are required")
	}
	if err := s.authDNSZone(ctx, req.ZoneUuid); err != nil {
		return nil, err
	}
	rec, created, err := s.adp.CreateDNSRecord(req.ZoneUuid, req.Name, req.Type, req.Value, req.Ttl, req.Priority)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "create dns record: %v", err)
	}
	logger.Printf("CreateDNSRecord zone=%s name=%s type=%s uuid=%s created=%v", rec.ZoneUUID, rec.Name, rec.Type, rec.UUID, created)
	return &weftv1.CreateDNSRecordResponse{Record: toDNSRecordInfo(rec), Created: created}, nil
}

func (s *weftServer) UpdateDNSRecord(ctx context.Context, req *weftv1.UpdateDNSRecordRequest) (*weftv1.UpdateDNSRecordResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.authDNSRecord(ctx, req.Uuid); err != nil {
		return nil, err
	}
	rec, err := s.adp.UpdateDNSRecord(req.Uuid, req.Value, req.Ttl, req.Priority)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "update dns record: %v", err)
	}
	logger.Printf("UpdateDNSRecord uuid=%s", rec.UUID)
	return &weftv1.UpdateDNSRecordResponse{Record: toDNSRecordInfo(rec)}, nil
}

func (s *weftServer) DeleteDNSRecord(ctx context.Context, req *weftv1.DeleteDNSRecordRequest) (*weftv1.DeleteDNSRecordResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.authDNSRecord(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.DeleteDNSRecord(req.Uuid); err != nil {
		return nil, status.Errorf(codes.NotFound, "delete dns record: %v", err)
	}
	logger.Printf("DeleteDNSRecord uuid=%s", req.Uuid)
	return &weftv1.DeleteDNSRecordResponse{DeletedUuid: req.Uuid}, nil
}

func toDNSRecordInfo(rec weft.DNSRecord) *weftv1.DNSRecordInfo {
	return &weftv1.DNSRecordInfo{
		Uuid:            rec.UUID,
		ZoneUuid:        rec.ZoneUUID,
		Name:            rec.Name,
		Type:            rec.Type,
		Value:           rec.Value,
		Ttl:             rec.TTL,
		Priority:        rec.Priority,
		CreatedAtUnixNs: rec.CreatedAt.UnixNano(),
	}
}

// ---- Volume properties (v0.9.0) -----------------------------------

func (s *weftServer) GetVolumeProperty(_ context.Context, req *weftv1.GetVolumePropertyRequest) (*weftv1.GetVolumePropertyResponse, error) {
	if req.VolumeUuid == "" || req.Key == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_uuid and key are required")
	}
	p, ok := s.adp.GetVolumeProperty(req.VolumeUuid, req.Key)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "volume property not found")
	}
	return &weftv1.GetVolumePropertyResponse{Property: toVolumePropertyInfo(p)}, nil
}

func (s *weftServer) SetVolumeProperty(ctx context.Context, req *weftv1.SetVolumePropertyRequest) (*weftv1.SetVolumePropertyResponse, error) {
	if req.VolumeUuid == "" || req.Key == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_uuid and key are required")
	}
	if _, err := s.authVolume(ctx, req.VolumeUuid); err != nil {
		return nil, err
	}
	p, err := s.adp.SetVolumeProperty(req.VolumeUuid, req.Key, req.Value)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set volume property: %v", err)
	}
	return &weftv1.SetVolumePropertyResponse{Property: toVolumePropertyInfo(p)}, nil
}

func (s *weftServer) DeleteVolumeProperty(ctx context.Context, req *weftv1.DeleteVolumePropertyRequest) (*weftv1.DeleteVolumePropertyResponse, error) {
	if req.VolumeUuid == "" || req.Key == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_uuid and key are required")
	}
	if _, err := s.authVolume(ctx, req.VolumeUuid); err != nil {
		return nil, err
	}
	if err := s.adp.DeleteVolumeProperty(req.VolumeUuid, req.Key); err != nil {
		return nil, status.Errorf(codes.Internal, "delete volume property: %v", err)
	}
	return &weftv1.DeleteVolumePropertyResponse{}, nil
}

func toVolumePropertyInfo(p weft.VolumeProperty) *weftv1.VolumePropertyInfo {
	return &weftv1.VolumePropertyInfo{
		VolumeUuid:      p.VolumeUUID,
		Key:             p.Key,
		Value:           p.Value,
		UpdatedAtUnixNs: p.UpdatedAt.UnixNano(),
	}
}

// ---- Shares (v0.8 list/create/delete + v0.9 get/resize) ----------

func (s *weftServer) ListShares(_ context.Context, req *weftv1.ListSharesRequest) (*weftv1.ListSharesResponse, error) {
	projUUID := s.resolveProjectUUID(req.Project)
	shares := s.adp.Shares(projUUID)
	out := make([]*weftv1.ShareInfo, 0, len(shares))
	for _, sh := range shares {
		out = append(out, toShareInfo(sh))
	}
	return &weftv1.ListSharesResponse{Shares: out}, nil
}

func (s *weftServer) GetShare(_ context.Context, req *weftv1.GetShareRequest) (*weftv1.GetShareResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	sh, ok := s.adp.ShareByUUID(req.Uuid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "share not found")
	}
	return &weftv1.GetShareResponse{Share: toShareInfo(sh)}, nil
}

func (s *weftServer) CreateShare(ctx context.Context, req *weftv1.CreateShareRequest) (*weftv1.CreateShareResponse, error) {
	if req.Name == "" || req.SizeGb <= 0 {
		return nil, status.Error(codes.InvalidArgument, "name and size_gb (>0) are required")
	}
	projUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	if err := s.adp.EnforceTenantQuotaForShare(projUUID, int(req.SizeGb)); err != nil {
		return nil, err
	}
	sh, created, err := s.adp.CreateShare(projUUID, req.Name, req.SizeGb, req.Readonly, req.Backend)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create share: %v", err)
	}
	logger.Printf("CreateShare project=%s name=%s uuid=%s created=%v", sh.ProjectUUID, sh.Name, sh.UUID, created)
	return &weftv1.CreateShareResponse{Share: toShareInfo(sh)}, nil
}

func (s *weftServer) ResizeShare(ctx context.Context, req *weftv1.ResizeShareRequest) (*weftv1.ResizeShareResponse, error) {
	if req.Uuid == "" || req.NewSizeGb <= 0 {
		return nil, status.Error(codes.InvalidArgument, "uuid and new_size_gb (>0) are required")
	}
	if err := s.authShare(ctx, req.Uuid); err != nil {
		return nil, err
	}
	sh, err := s.adp.ResizeShare(req.Uuid, req.NewSizeGb)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "resize share: %v", err)
	}
	logger.Printf("ResizeShare uuid=%s new_size_gb=%d", sh.UUID, sh.SizeGB)
	return &weftv1.ResizeShareResponse{Share: toShareInfo(sh)}, nil
}

func (s *weftServer) DeleteShare(ctx context.Context, req *weftv1.DeleteShareRequest) (*weftv1.DeleteShareResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.authShare(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.DeleteShare(req.Uuid); err != nil {
		return nil, status.Errorf(codes.NotFound, "delete share: %v", err)
	}
	logger.Printf("DeleteShare uuid=%s", req.Uuid)
	return &weftv1.DeleteShareResponse{}, nil
}

func toShareInfo(sh weft.Share) *weftv1.ShareInfo {
	return &weftv1.ShareInfo{
		Uuid:            sh.UUID,
		Name:            sh.Name,
		ProjectUuid:     sh.ProjectUUID,
		Backend:         sh.Backend,
		SizeGb:          sh.SizeGB,
		Readonly:        sh.Readonly,
		Status:          sh.Status,
		CreatedAtUnixNs: sh.CreatedAt.UnixNano(),
	}
}

// authShare resolves a share UUID → owning project and delegates to
// AuthorizeProject.
func (s *weftServer) authShare(ctx context.Context, uuid string) error {
	sh, ok := s.adp.ShareByUUID(uuid)
	if !ok {
		return status.Errorf(codes.PermissionDenied, "no access to share %s", uuid)
	}
	if _, err := s.adp.AuthorizeProject(ctx, sh.ProjectUUID); err != nil {
		return err
	}
	return nil
}

// ---- Buckets (v0.9.0) ---------------------------------------------

func (s *weftServer) ListBuckets(_ context.Context, req *weftv1.ListBucketsRequest) (*weftv1.ListBucketsResponse, error) {
	projUUID := s.resolveProjectUUID(req.Project)
	bks := s.adp.Buckets(projUUID)
	out := make([]*weftv1.BucketInfo, 0, len(bks))
	for _, b := range bks {
		out = append(out, toBucketInfoListView(b))
	}
	return &weftv1.ListBucketsResponse{Buckets: out}, nil
}

func (s *weftServer) GetBucket(_ context.Context, req *weftv1.GetBucketRequest) (*weftv1.GetBucketResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	b, ok := s.adp.BucketByUUID(req.Uuid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "bucket not found")
	}
	return &weftv1.GetBucketResponse{Bucket: toBucketInfoFull(b)}, nil
}

func (s *weftServer) CreateBucket(ctx context.Context, req *weftv1.CreateBucketRequest) (*weftv1.CreateBucketResponse, error) {
	if req.Name == "" || req.Endpoint == "" {
		return nil, status.Error(codes.InvalidArgument, "name and endpoint are required")
	}
	projUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	if err := s.adp.EnforceTenantQuotaForBucket(projUUID); err != nil {
		return nil, err
	}
	b, created, err := s.adp.CreateBucket(projUUID, req.Name, req.Endpoint, req.Region, req.AccessKeyId, req.SecretAccessKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create bucket: %v", err)
	}
	logger.Printf("CreateBucket project=%s name=%s uuid=%s created=%v", b.ProjectUUID, b.Name, b.UUID, created)
	return &weftv1.CreateBucketResponse{Bucket: toBucketInfoFull(b), Created: created}, nil
}

func (s *weftServer) DeleteBucket(ctx context.Context, req *weftv1.DeleteBucketRequest) (*weftv1.DeleteBucketResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.authBucket(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.DeleteBucket(req.Uuid); err != nil {
		return nil, status.Errorf(codes.NotFound, "delete bucket: %v", err)
	}
	logger.Printf("DeleteBucket uuid=%s", req.Uuid)
	return &weftv1.DeleteBucketResponse{DeletedUuid: req.Uuid}, nil
}

func (s *weftServer) GetBucketPolicy(_ context.Context, req *weftv1.GetBucketPolicyRequest) (*weftv1.GetBucketPolicyResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	b, ok := s.adp.BucketByUUID(req.Uuid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "bucket not found")
	}
	return &weftv1.GetBucketPolicyResponse{Policy: b.Policy}, nil
}

func (s *weftServer) SetBucketPolicy(ctx context.Context, req *weftv1.SetBucketPolicyRequest) (*weftv1.SetBucketPolicyResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.authBucket(ctx, req.Uuid); err != nil {
		return nil, err
	}
	b, err := s.adp.SetBucketPolicy(req.Uuid, req.Policy)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "set bucket policy: %v", err)
	}
	logger.Printf("SetBucketPolicy uuid=%s", b.UUID)
	return &weftv1.SetBucketPolicyResponse{Bucket: toBucketInfoFull(b)}, nil
}

// toBucketInfoListView omits the secret_access_key + policy from
// list responses (a bucket list of N rows shouldn't drag N policy
// JSON blobs across the wire). GetBucket + Get/SetBucketPolicy
// round-trip the full blob.
func toBucketInfoListView(b weft.Bucket) *weftv1.BucketInfo {
	return &weftv1.BucketInfo{
		Uuid:            b.UUID,
		ProjectUuid:     b.ProjectUUID,
		Name:            b.Name,
		Endpoint:        b.Endpoint,
		Region:          b.Region,
		AccessKeyId:     b.AccessKeyID,
		CreatedAtUnixNs: b.CreatedAt.UnixNano(),
	}
}

func toBucketInfoFull(b weft.Bucket) *weftv1.BucketInfo {
	return &weftv1.BucketInfo{
		Uuid:            b.UUID,
		ProjectUuid:     b.ProjectUUID,
		Name:            b.Name,
		Endpoint:        b.Endpoint,
		Region:          b.Region,
		AccessKeyId:     b.AccessKeyID,
		SecretAccessKey: b.SecretAccessKey,
		Policy:          b.Policy,
		CreatedAtUnixNs: b.CreatedAt.UnixNano(),
	}
}

// authBucket resolves a bucket UUID → owning project and delegates
// to AuthorizeProject.
func (s *weftServer) authBucket(ctx context.Context, uuid string) error {
	b, ok := s.adp.BucketByUUID(uuid)
	if !ok {
		return status.Errorf(codes.PermissionDenied, "no access to bucket %s", uuid)
	}
	if _, err := s.adp.AuthorizeProject(ctx, b.ProjectUUID); err != nil {
		return err
	}
	return nil
}

// ---- SSH key catalogue (v0.9.0) -----------------------------------

func (s *weftServer) ListSSHKeyCatalogue(_ context.Context, _ *weftv1.ListSSHKeyCatalogueRequest) (*weftv1.ListSSHKeyCatalogueResponse, error) {
	keys := s.adp.SSHKeyCatalogue()
	out := make([]*weftv1.SSHKeyCatalogueEntry, 0, len(keys))
	for _, k := range keys {
		out = append(out, toSSHKeyCatEntry(k))
	}
	return &weftv1.ListSSHKeyCatalogueResponse{Keys: out}, nil
}

func (s *weftServer) AddSSHKeyCatalogue(ctx context.Context, req *weftv1.AddSSHKeyCatalogueRequest) (*weftv1.AddSSHKeyCatalogueResponse, error) {
	if req.Name == "" || req.PublicKey == "" {
		return nil, status.Error(codes.InvalidArgument, "name and public_key are required")
	}
	if err := weft.RequireAdmin(ctx, "add sshkey catalogue"); err != nil {
		return nil, err
	}
	k, added, err := s.adp.AddSSHKeyCatalogue(req.Name, req.PublicKey, req.Comment)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "add sshkey catalogue: %v", err)
	}
	logger.Printf("AddSSHKeyCatalogue name=%s uuid=%s added=%v", k.Name, k.UUID, added)
	return &weftv1.AddSSHKeyCatalogueResponse{Key: toSSHKeyCatEntry(k), Added: added}, nil
}

func (s *weftServer) RemoveSSHKeyCatalogue(ctx context.Context, req *weftv1.RemoveSSHKeyCatalogueRequest) (*weftv1.RemoveSSHKeyCatalogueResponse, error) {
	if req.Uuid == "" && req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid or name is required")
	}
	if err := weft.RequireAdmin(ctx, "remove sshkey catalogue"); err != nil {
		return nil, err
	}
	uuid := req.Uuid
	if uuid == "" {
		k, ok := s.adp.SSHKeyCatalogueByName(req.Name)
		if !ok {
			return nil, status.Errorf(codes.NotFound, "sshkey %q not found", req.Name)
		}
		uuid = k.UUID
	}
	if err := s.adp.RemoveSSHKeyCatalogue(uuid); err != nil {
		return nil, status.Errorf(codes.NotFound, "remove sshkey catalogue: %v", err)
	}
	logger.Printf("RemoveSSHKeyCatalogue uuid=%s", uuid)
	return &weftv1.RemoveSSHKeyCatalogueResponse{DeletedUuid: uuid}, nil
}

func (s *weftServer) ImportSSHKeyCatalogue(ctx context.Context, req *weftv1.ImportSSHKeyCatalogueRequest) (*weftv1.ImportSSHKeyCatalogueResponse, error) {
	if req.NamePrefix == "" || req.Blob == "" {
		return nil, status.Error(codes.InvalidArgument, "name_prefix and blob are required")
	}
	if err := weft.RequireAdmin(ctx, "import sshkey catalogue"); err != nil {
		return nil, err
	}
	imported, skipped, err := s.adp.ImportSSHKeyCatalogue(req.NamePrefix, req.Blob, req.Comment)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "import sshkey catalogue: %v", err)
	}
	out := make([]*weftv1.SSHKeyCatalogueEntry, 0, len(imported))
	for _, k := range imported {
		out = append(out, toSSHKeyCatEntry(k))
	}
	logger.Printf("ImportSSHKeyCatalogue prefix=%s imported=%d skipped=%d", req.NamePrefix, len(imported), skipped)
	return &weftv1.ImportSSHKeyCatalogueResponse{Imported: out, SkippedDuplicates: skipped}, nil
}

func toSSHKeyCatEntry(k weft.SSHKeyCatalogueEntry) *weftv1.SSHKeyCatalogueEntry {
	return &weftv1.SSHKeyCatalogueEntry{
		Uuid:          k.UUID,
		Name:          k.Name,
		PublicKey:     k.PublicKey,
		Fingerprint:   k.Fingerprint,
		Comment:       k.Comment,
		AddedAtUnixNs: k.AddedAt.UnixNano(),
	}
}

// ---- Scheduling rules (v0.9.0) ------------------------------------

func (s *weftServer) ListSchedulingRules(_ context.Context, _ *weftv1.ListSchedulingRulesRequest) (*weftv1.ListSchedulingRulesResponse, error) {
	rules := s.adp.SchedulingRules()
	out := make([]*weftv1.SchedulingRuleInfo, 0, len(rules))
	for _, r := range rules {
		out = append(out, toSchedulingRuleInfo(r))
	}
	return &weftv1.ListSchedulingRulesResponse{Rules: out}, nil
}

func (s *weftServer) CreateSchedulingRule(ctx context.Context, req *weftv1.CreateSchedulingRuleRequest) (*weftv1.CreateSchedulingRuleResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := weft.RequireAdmin(ctx, "create scheduling rule"); err != nil {
		return nil, err
	}
	r, created, err := s.adp.CreateSchedulingRule(req.Name, req.Selector, req.TargetCount, req.AntiAffinity, respawnFromProto(req.Respawn))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create scheduling rule: %v", err)
	}
	logger.Printf("CreateSchedulingRule name=%s uuid=%s created=%v", r.Name, r.UUID, created)
	return &weftv1.CreateSchedulingRuleResponse{Rule: toSchedulingRuleInfo(r), Created: created}, nil
}

func (s *weftServer) UpdateSchedulingRule(ctx context.Context, req *weftv1.UpdateSchedulingRuleRequest) (*weftv1.UpdateSchedulingRuleResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := weft.RequireAdmin(ctx, "update scheduling rule"); err != nil {
		return nil, err
	}
	r, err := s.adp.UpdateSchedulingRule(req.Uuid, req.Selector, req.TargetCount, req.AntiAffinity, respawnFromProto(req.Respawn), req.ClearRespawn)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "update scheduling rule: %v", err)
	}
	logger.Printf("UpdateSchedulingRule uuid=%s", r.UUID)
	return &weftv1.UpdateSchedulingRuleResponse{Rule: toSchedulingRuleInfo(r)}, nil
}

func (s *weftServer) DeleteSchedulingRule(ctx context.Context, req *weftv1.DeleteSchedulingRuleRequest) (*weftv1.DeleteSchedulingRuleResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := weft.RequireAdmin(ctx, "delete scheduling rule"); err != nil {
		return nil, err
	}
	if err := s.adp.DeleteSchedulingRule(req.Uuid); err != nil {
		return nil, status.Errorf(codes.NotFound, "delete scheduling rule: %v", err)
	}
	logger.Printf("DeleteSchedulingRule uuid=%s", req.Uuid)
	return &weftv1.DeleteSchedulingRuleResponse{DeletedUuid: req.Uuid}, nil
}

func toSchedulingRuleInfo(r weft.SchedulingRuleEntry) *weftv1.SchedulingRuleInfo {
	return &weftv1.SchedulingRuleInfo{
		Uuid:            r.UUID,
		Name:            r.Name,
		Selector:        r.Selector,
		TargetCount:     r.TargetCount,
		AntiAffinity:    r.AntiAffinity,
		Respawn:         respawnToProto(r.Respawn),
		CreatedAtUnixNs: r.CreatedAt.UnixNano(),
	}
}

// respawnFromProto / respawnToProto are the gRPC ↔ persistence
// converters for RespawnPolicy + HealthProbe. The persisted form
// (RespawnPolicyJSON) stays plain Go so the registry doesn't pull
// the proto runtime into JSON marshalling.

func respawnFromProto(p *weftv1.RespawnPolicy) *weft.RespawnPolicyJSON {
	if p == nil {
		return nil
	}
	return &weft.RespawnPolicyJSON{
		Enabled:        p.GetEnabled(),
		GracePeriodMs:  p.GetGracePeriodMs(),
		MaxRestarts:    p.GetMaxRestarts(),
		WindowMs:       p.GetWindowMs(),
		Backoff:        p.GetBackoff(),
		InitialDelayMs: p.GetInitialDelayMs(),
		Liveness:       healthProbeFromProto(p.GetLiveness()),
		Readiness:      healthProbeFromProto(p.GetReadiness()),
	}
}

func respawnToProto(p *weft.RespawnPolicyJSON) *weftv1.RespawnPolicy {
	if p == nil {
		return nil
	}
	return &weftv1.RespawnPolicy{
		Enabled:        p.Enabled,
		GracePeriodMs:  p.GracePeriodMs,
		MaxRestarts:    p.MaxRestarts,
		WindowMs:       p.WindowMs,
		Backoff:        p.Backoff,
		InitialDelayMs: p.InitialDelayMs,
		Liveness:       healthProbeToProto(p.Liveness),
		Readiness:      healthProbeToProto(p.Readiness),
	}
}

func healthProbeFromProto(p *weftv1.HealthProbe) *weft.HealthProbeJSON {
	if p == nil || p.GetType() == weftv1.HealthProbe_NONE {
		return nil
	}
	out := &weft.HealthProbeJSON{
		HttpPath:         p.GetHttpPath(),
		HttpPort:         p.GetHttpPort(),
		HttpMethod:       p.GetHttpMethod(),
		HttpStatusOK:     append([]int32(nil), p.GetHttpStatusOk()...),
		TcpPort:          p.GetTcpPort(),
		ExecCommand:      append([]string(nil), p.GetExecCommand()...),
		InitialDelayMs:   p.GetInitialDelayMs(),
		PeriodMs:         p.GetPeriodMs(),
		TimeoutMs:        p.GetTimeoutMs(),
		FailureThreshold: p.GetFailureThreshold(),
		SuccessThreshold: p.GetSuccessThreshold(),
	}
	switch p.GetType() {
	case weftv1.HealthProbe_HTTP:
		out.Type = "http"
	case weftv1.HealthProbe_TCP:
		out.Type = "tcp"
	case weftv1.HealthProbe_EXEC:
		out.Type = "exec"
	}
	return out
}

func healthProbeToProto(p *weft.HealthProbeJSON) *weftv1.HealthProbe {
	if p == nil {
		return nil
	}
	out := &weftv1.HealthProbe{
		HttpPath:         p.HttpPath,
		HttpPort:         p.HttpPort,
		HttpMethod:       p.HttpMethod,
		HttpStatusOk:     append([]int32(nil), p.HttpStatusOK...),
		TcpPort:          p.TcpPort,
		ExecCommand:      append([]string(nil), p.ExecCommand...),
		InitialDelayMs:   p.InitialDelayMs,
		PeriodMs:         p.PeriodMs,
		TimeoutMs:        p.TimeoutMs,
		FailureThreshold: p.FailureThreshold,
		SuccessThreshold: p.SuccessThreshold,
	}
	switch p.Type {
	case "http":
		out.Type = weftv1.HealthProbe_HTTP
	case "tcp":
		out.Type = weftv1.HealthProbe_TCP
	case "exec":
		out.Type = weftv1.HealthProbe_EXEC
	default:
		out.Type = weftv1.HealthProbe_NONE
	}
	return out
}

// ---- Registry remotes (v0.9.0) ------------------------------------

func (s *weftServer) ListRegistryRemotes(_ context.Context, _ *weftv1.ListRegistryRemotesRequest) (*weftv1.ListRegistryRemotesResponse, error) {
	remotes := s.adp.RegistryRemotes()
	out := make([]*weftv1.RegistryRemoteInfo, 0, len(remotes))
	for _, r := range remotes {
		out = append(out, toRegistryRemoteInfo(r))
	}
	return &weftv1.ListRegistryRemotesResponse{Remotes: out}, nil
}

func (s *weftServer) SetRegistryRemote(ctx context.Context, req *weftv1.SetRegistryRemoteRequest) (*weftv1.SetRegistryRemoteResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := weft.RequireAdmin(ctx, "set registry remote"); err != nil {
		return nil, err
	}
	r, created, err := s.adp.SetRegistryRemote(req.Name, req.Endpoint, req.Insecure, req.CredentialSecretRef)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "set registry remote: %v", err)
	}
	logger.Printf("SetRegistryRemote name=%s uuid=%s created=%v", r.Name, r.UUID, created)
	return &weftv1.SetRegistryRemoteResponse{Remote: toRegistryRemoteInfo(r), Created: created}, nil
}

func (s *weftServer) DeleteRegistryRemote(ctx context.Context, req *weftv1.DeleteRegistryRemoteRequest) (*weftv1.DeleteRegistryRemoteResponse, error) {
	if req.Uuid == "" && req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid or name is required")
	}
	if err := weft.RequireAdmin(ctx, "delete registry remote"); err != nil {
		return nil, err
	}
	uuid := req.Uuid
	if uuid == "" {
		r, ok := s.adp.RegistryRemoteByName(req.Name)
		if !ok {
			return nil, status.Errorf(codes.NotFound, "registry remote %q not found", req.Name)
		}
		uuid = r.UUID
	}
	if err := s.adp.DeleteRegistryRemote(uuid); err != nil {
		return nil, status.Errorf(codes.NotFound, "delete registry remote: %v", err)
	}
	logger.Printf("DeleteRegistryRemote uuid=%s", uuid)
	return &weftv1.DeleteRegistryRemoteResponse{DeletedUuid: uuid}, nil
}

// SearchRegistryRemote queries the upstream OCI Distribution
// `/v2/_catalog` endpoint of the enrolled RegistryRemote and returns
// the repositories that match `req.Query` (case-insensitive substring).
//
// The dialer lives in github.com/openweft/weft/registryclient ; it
// transparently handles a 401 + Bearer challenge by fetching a token
// at the advertised realm and retrying once, follows the `Link: ... ;
// rel="next"` pagination header, and honours the RegistryRemote's
// Insecure flag for both scheme fallback (http) and TLS verification.
//
// Authenticated catalogue access (registry credentials stored under
// CredentialSecretRef) is intentionally NOT plumbed yet — public
// catalogues (Docker Hub, ghcr.io public namespaces, Harbor) work
// today ; registries that require admin scope for `/v2/_catalog` will
// surface an HTTP-status error verbatim so operators can react.
func (s *weftServer) SearchRegistryRemote(ctx context.Context, req *weftv1.SearchRegistryRemoteRequest) (*weftv1.SearchRegistryRemoteResponse, error) {
	if req.Uuid == "" && req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid or name is required")
	}
	var (
		r  weft.RegistryRemote
		ok bool
	)
	if req.Uuid != "" {
		r, ok = s.adp.RegistryRemoteByUUID(req.Uuid)
	} else {
		r, ok = s.adp.RegistryRemoteByName(req.Name)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "registry remote not found")
	}
	client := &registryclient.CatalogClient{
		Endpoint: r.Endpoint,
		Insecure: r.Insecure,
	}
	limit := int(req.Limit)
	repos, err := client.Catalog(ctx, req.Query, limit)
	if err != nil {
		logger.Printf("SearchRegistryRemote %s : %v", r.Name, err)
		return nil, status.Errorf(codes.Unavailable, "catalogue query failed : %v", err)
	}
	return &weftv1.SearchRegistryRemoteResponse{
		RegistryName: r.Name,
		Repositories: repos,
	}, nil
}

func toRegistryRemoteInfo(r weft.RegistryRemote) *weftv1.RegistryRemoteInfo {
	return &weftv1.RegistryRemoteInfo{
		Uuid:                r.UUID,
		Name:                r.Name,
		Endpoint:            r.Endpoint,
		Insecure:            r.Insecure,
		CredentialSecretRef: r.CredentialSecretRef,
		CreatedAtUnixNs:     r.CreatedAt.UnixNano(),
	}
}

// resolveProjectUUID resolves a `project` field (UUID or display
// name) to its UUID. Empty returns "" (caller-scoped list).
func (s *weftServer) resolveProjectUUID(project string) string {
	if project == "" {
		return ""
	}
	if _, ok := s.adp.ProjectByUUID(project); ok {
		return project
	}
	for _, p := range s.adp.Projects() {
		if p.Name == project {
			return p.UUID
		}
	}
	return ""
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
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
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
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
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
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
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
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
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
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
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
	if _, err := s.adp.AuthorizeProject(ctx, req.Project); err != nil {
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

func rowToProto(r wefthcl.Row) *weftv1.VMInfo {
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
