package cluster

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// remoteInfraDir is the per-host directory weft up uploads infra/<svc>/plan.hcl
// into. weft infra deploy on the remote reads it via the rendered --plan flag.
// $HOME-expanded by the remote shell so the path resolves to the SSH user's
// home (admin on dev VMs, e.g. /home/admin/.weft/infra/<svc>/plan.hcl).
const remoteInfraDir = "$HOME/.weft/infra"

// SSH is the chosen cluster access model: weft up reaches each hypervisor
// over SSH to install/start its agent and drive the per-host deploy. The
// command rendering here is pure (and tested); the live transport (Apply)
// uses golang.org/x/crypto/ssh.

// controlPlanePort is the TCP port the seed agent exposes for cross-host
// gRPC. The dev-mode `--tcp-listen=:<port>` listener bridges what
// sshtransport doesn't yet do (remote SSH host); clients dial
// `--control-plane=tcp:<seed-addr>:<port>`. Production should switch to
// sshtransport once it gains cross-host support.
const controlPlanePort = "7330"

// Seed is the control-plane host — the first host in the description. The
// other hosts join it.
func (c *Cluster) Seed() Host { return c.Hosts[0] }

// SSHTarget is how weft up connects to one host.
type SSHTarget struct {
	HostID  string
	Addr    string // underlay address; ":22" appended if no port
	User    string
	KeyPath string // private key path; empty → use the SSH agent
}

// Target resolves the SSH connection details for a host from its `ssh { … }`
// block, defaulting the user to "root".
func (c *Cluster) Target(h Host) SSHTarget {
	user, key := "root", ""
	if h.SSH != nil {
		if h.SSH.User != "" {
			user = h.SSH.User
		}
		key = h.SSH.Key
	}
	return SSHTarget{HostID: h.ID, Addr: h.Address, User: user, KeyPath: key}
}

// renderAction turns one convergence Action into the host it runs on and the
// remote command weft up issues there. hosts[0] is the seed; cross-host
// actions (mesh, quorum growth) are anchored on it. The exact `weft` flags
// reflect the intended bootstrap — see README for the assumptions and the
// per-replica infra-deploy flag the executor still needs.
// heredocMarker is the bash heredoc terminator used for PushAgentConfig.
// Picked to be long + unique enough that no plausible HCL value could
// collide with it on a line of its own — hclwrite quotes string values so
// they live inside double quotes, never as a bare line.
const heredocMarker = "__WEFT_HCL_EOF__"

func renderAction(c *Cluster, a Action) (hostID, command string) {
	seed := c.Seed()
	switch a.Kind {
	case PushAgentConfig:
		// install -d is idempotent (no error if /etc/weft exists). The
		// heredoc terminator must be at the start of a line ; the rendered
		// HCL ends with a trailing newline (hclwrite invariant), so we
		// don't add an extra one.
		return a.Host, fmt.Sprintf(
			"sudo install -d /etc/weft && sudo tee /etc/weft/weft.hcl >/dev/null <<'%s'\n%s%s",
			heredocMarker, a.Config, heredocMarker,
		)
	case EnsureHost:
		hv, az, rack := "", "", ""
		for _, h := range c.Hosts {
			if h.ID != a.Host {
				continue
			}
			hv = h.Hypervisor
			az = h.DC
			rack = h.Rack
			break
		}
		// Driver-plugin OCI config — passed through as Environment=
		// in the unit file so the agent's plugin-pull path sees them.
		driverEnv := c.Drivers.Env()
		// Render the systemd unit. Per-host fields (WEFT_AZ, WEFT_RACK,
		// WEFT_HYPERVISOR) become Environment= lines ; the rest is
		// static and matches deploy/systemd/weft-agent.service.
		label := "agent"
		if a.Host == seed.ID {
			label = "seed agent"
		}
		var envLines []string
		envLines = append(envLines, "Environment=WEFT_RECONCILE_HOSTS_INTERVAL=30s")
		envLines = append(envLines, "Environment=WEFT_PHANTOM_HOST_DELETE_AGE=1h")
		if az != "" {
			envLines = append(envLines, "Environment=WEFT_AZ="+az)
		}
		if rack != "" {
			envLines = append(envLines, "Environment=WEFT_RACK="+rack)
		}
		if hv != "" {
			envLines = append(envLines, "Environment=WEFT_HYPERVISOR="+hv)
		}
		for _, e := range driverEnv {
			envLines = append(envLines, "Environment="+e)
		}
		unit := fmt.Sprintf(`[Unit]
Description=weft control-plane / hypervisor agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=admin
Group=admin
WorkingDirectory=/home/admin
%s
ExecStart=/usr/local/bin/weft agent --vsock-port=0
Restart=always
RestartSec=5s
StandardOutput=journal
StandardError=journal
ReadWritePaths=/home/admin

[Install]
WantedBy=multi-user.target
`, strings.Join(envLines, "\n"))
		// Atomic install : write the unit file, reload systemd, then
		// enable+restart. systemctl restart is idempotent (start if
		// stopped, restart if running) so re-apply keeps the daemon
		// in lock-step with the unit content.
		cmd := fmt.Sprintf(
			"sudo tee /etc/systemd/system/weft-agent.service >/dev/null <<'%s'\n%s%s\nsudo systemctl daemon-reload && sudo systemctl enable --now weft-agent.service   # %s",
			heredocMarker, unit, heredocMarker, label,
		)
		// V0.x : mirror cluster.hcl-declared properties into the runtime
		// host registry's labels map. The agent self-registers on startup
		// with an empty labels map (or whatever was persisted last) ; we
		// chain a `weft host register --uuid=<discovered>` to push the
		// operator-declared properties. UUID is unknown from cluster.hcl
		// (minted server-side) so we query `host ls` filtered by hostname.
		// Best-effort : if the shell snippet fails (agent slow to register,
		// no UUID found yet), the next `weft up --apply` retries — the
		// properties stay declarative in cluster.hcl, not lost.
		if len(a.Properties) > 0 {
			labels := propertiesToLabelsArg(a.Properties)
			cmd += fmt.Sprintf(
				" && (sleep 1 ; WEFT_HOST_UUID=$(weft host ls 2>/dev/null | awk -v h=\"$(hostname)\" '$2==h{print $1}') ; "+
					"[ -n \"$WEFT_HOST_UUID\" ] && weft host register --uuid=$WEFT_HOST_UUID --hostname=\"$(hostname)\" --labels=%s || true)",
				labels,
			)
		}
		return a.Host, cmd
	case EnsureAZ:
		// Idempotent : exit 0 if the AZ already exists. `weft az
		// create` will return an error on duplicate code ; the
		// `|| true` swallows it. Codes are kebab-case alnum so no
		// shell quoting needed beyond the literal value.
		return a.Host, fmt.Sprintf(
			"weft az create %s --name 'DC %s' 2>/dev/null || true   # idempotent AZ record",
			a.DC, a.DC,
		)
	case EnsureRack:
		return a.Host, fmt.Sprintf(
			"weft rack create %s --az %s --name 'Rack %s' 2>/dev/null || true   # idempotent rack record",
			a.Service, a.DC, a.Service,
		)
	case MeshSync:
		// Control-plane coordination step — rendered as a logged note, NOT a
		// remote shell command (same class as GrowQuorum below). The real
		// execution lives in the seed daemon: Adapter.PublishHostMesh rebuilds
		// the host-level WireGuard peer set from the live host registry and
		// fans each host's set out on weft.hostmesh.<uuid>. It runs on the
		// seed because that's where the registry + NATS connection are, and it
		// can't be a remote `ssh` exec because RenderSSH/Apply have no daemon
		// handle. The peer set carries public keys + endpoints only — every
		// host minted its own private key locally (EnsureHostWGKey) — so this
		// step moves no secret. overlaySubnet() is the subnet PublishHostMesh
		// is invoked with.
		return seed.ID, fmt.Sprintf(
			"# host-mesh sync (seed: Adapter.PublishHostMesh subnet=%s) → publish peer set to [%s]",
			c.overlaySubnet(), strings.Join(a.Hosts, ","),
		)
	case EnsureKernel:
		// Pull the shared microVM kernel OCI artifact onto the host. Same
		// idempotency story as EnsureImage: weft microvm pull-kernel is
		// standalone and the rename-into-place semantics are atomic.
		return a.Host, fmt.Sprintf("weft microvm pull-kernel %s   # shared kernel into $XDG_DATA_HOME/weft-microvm/kernel", a.Image)
	case EnsureInitrd:
		// Same shape as EnsureKernel — atomic rename into
		// $XDG_DATA_HOME/weft-microvm/pod-initrd so the next PlaceReplica's
		// pod-mode boot path picks it up.
		return a.Host, fmt.Sprintf("weft microvm pull-pod-initrd %s   # shared pod-initrd into $XDG_DATA_HOME/weft-microvm/pod-initrd", a.Image)
	case EnsureImage:
		// Pre-pull the OCI rootfs on the host. weft microvm pull is standalone
		// (no agent socket needed). microvm.Pull isn't idempotent on an already-
		// extracted rootfs (re-extracting over existing files panics mid-layer),
		// so we guard at the orchestrator level: stat the cache dir, skip pull
		// when it exists. The refsafe transform mirrors microvm.refsafe().
		refsafe := strings.NewReplacer("/", "_", ":", "_").Replace(a.Image)
		return a.Host, fmt.Sprintf(
			"[ -d $HOME/.local/share/weft-microvm/images/%s/rootfs ] && echo 'cached %s' || weft microvm pull %s   # rootfs into host cache",
			refsafe, a.Image, a.Image,
		)
	case PlaceReplica:
		// --plan points at the plan.hcl Apply uploads to each host before the
		// first PlaceReplica action lands. Without it, weft infra deploy would
		// look up its default <moduleRoot>/infra/<svc>/plan.hcl path relative
		// to the remote cwd, where the source tree isn't present.
		//
		// --replica passes the planner-decided 1-indexed replica number so
		// the deployed VM gets a distinct name per host : without it every
		// host's invocation would call its VM `infra-<svc>` and the etcd
		// registry would collapse them into one record (whichever wrote
		// first wins, the rest become invisible from the operator's view).
		return a.Host, fmt.Sprintf(
			"weft infra deploy %s --plan %s/%s/plan.hcl --replica %d   # replica %d (dc=%s)",
			a.Service, remoteInfraDir, a.Service, a.Replica, a.Replica, a.DC,
		)
	case GrowQuorum:
		return seed.ID, fmt.Sprintf("# grow %s quorum %d→%d (etcd member-add / nats route)", a.Service, a.From, a.To)
	case StopReplica:
		// Image carries the VM name (BuildDownPlan stashes VMNameFor(r) there).
		// `|| true` so a re-run on an already-gone VM still succeeds — the
		// whole teardown chain must stay best-effort idempotent.
		return a.Host, fmt.Sprintf(
			"weft microvm rm %s || true   # replica %d (dc=%s)",
			a.Image, a.Replica, a.DC,
		)
	case StopAgent:
		// Mirror of EnsureHost's `nohup weft agent &` — kill any matching
		// process, tolerate absence. pkill -x matches the exact binary name
		// so an unrelated path containing `weft` won't be hit.
		return a.Host, "pkill -x weft || true   # stop weft agent"
	case TeardownMesh:
		return a.Host, "rm -f /etc/wireguard/wg0.conf && wg-quick down wg0 2>/dev/null || true   # tear down overlay"
	case Purge:
		// Drop ~/.weft (host UUID, embed-etcd data, caches) AND /var/lib/weft
		// (system-install layout). Both `|| true` so a missing path doesn't
		// fail the whole teardown.
		return a.Host, "rm -rf $HOME/.weft /var/lib/weft || true   # purge host state"
	default:
		return seed.ID, "# " + a.String()
	}
}

// HostPlan is the per-host view of the SSH execution: the target + the
// ordered remote commands. Used by `weft up --ssh` to show what runs where.
type HostPlan struct {
	Target SSHTarget
	Steps  []string
}

// RenderSSH groups a Plan's actions into per-host SSH command sequences, in
// first-touch host order (the seed first, since it's ensured first).
func RenderSSH(c *Cluster, p *Plan) []HostPlan {
	byID := map[string]Host{}
	for _, h := range c.Hosts {
		byID[h.ID] = h
	}
	var order []string
	steps := map[string][]string{}
	for _, a := range p.Actions {
		id, cmd := renderAction(c, a)
		if _, seen := steps[id]; !seen {
			order = append(order, id)
		}
		steps[id] = append(steps[id], cmd)
	}
	out := make([]HostPlan, 0, len(order))
	for _, id := range order {
		out = append(out, HostPlan{Target: c.Target(byID[id]), Steps: steps[id]})
	}
	return out
}

// Apply executes the plan over SSH in action order (preserving cross-host
// ordering), reusing one connection per host. logf receives per-step
// progress. Lines beginning with '#' are operator/control-plane notes that
// don't run remotely (mesh/quorum coordination) — they're logged, not exec'd.
//
// NOTE: live execution needs the hosts reachable over SSH with the configured
// credentials; it can't be exercised without real hypervisors. Host-key
// verification is skipped (dev) — production must pin known_hosts.
func Apply(c *Cluster, p *Plan, moduleRoot string, logf func(string, ...any)) error {
	conns := map[string]*ssh.Client{}
	defer func() {
		for _, cl := range conns {
			cl.Close()
		}
	}()
	byID := map[string]Host{}
	for _, h := range c.Hosts {
		byID[h.ID] = h
	}
	// Tracks "<host>/<svc>" pairs whose plan.hcl has already been uploaded
	// this run, so re-iterating PlaceReplica actions for the same service on
	// a host doesn't re-upload. Across re-applies a fresh map starts empty
	// and re-uploads (cheap; one Run() per file).
	uploaded := map[string]bool{}

	for _, a := range p.Actions {
		hostID, cmd := renderAction(c, a)
		if strings.HasPrefix(strings.TrimSpace(cmd), "#") {
			logf("[%s] note: %s", hostID, strings.TrimPrefix(strings.TrimSpace(cmd), "# "))
			continue
		}
		cl, ok := conns[hostID]
		if !ok {
			var err error
			if cl, err = dial(c.Target(byID[hostID])); err != nil {
				return fmt.Errorf("ssh %s: %w", hostID, err)
			}
			conns[hostID] = cl
		}
		// Lazily upload the service's plan.hcl on the first PlaceReplica that
		// targets this host — the remote `weft infra deploy --plan …` needs it.
		if a.Kind == PlaceReplica {
			key := hostID + "/" + a.Service
			if !uploaded[key] {
				if err := uploadPlan(cl, moduleRoot, a.Service); err != nil {
					return fmt.Errorf("[%s] upload plan %s: %w", hostID, a.Service, err)
				}
				logf("[%s] uploaded infra/%s/plan.hcl", hostID, a.Service)
				uploaded[key] = true
			}
		}
		logf("[%s] $ %s", hostID, cmd)
		out, err := run(cl, cmd)
		if strings.TrimSpace(out) != "" {
			logf("[%s] %s", hostID, strings.TrimSpace(out))
		}
		if err != nil {
			return fmt.Errorf("[%s] %q: %w", hostID, cmd, err)
		}
	}
	return nil
}

// uploadPlan ships <moduleRoot>/infra/<svc>/plan.hcl into $HOME/.weft/infra/<svc>/
// on the remote host via a single SSH session: mkdir + cat-from-stdin. The
// k0sctl `files:` analog, narrowed to the one file `weft infra deploy --plan`
// actually reads (config templates are inlined as HEREDOC in plan.hcl itself).
func uploadPlan(cl *ssh.Client, moduleRoot, svc string) error {
	src := filepath.Join(moduleRoot, "infra", svc, "plan.hcl")
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	sess, err := cl.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()
	sess.Stdin = bytes.NewReader(data)
	cmd := fmt.Sprintf(
		"mkdir -p %s/%s && cat > %s/%s/plan.hcl",
		remoteInfraDir, svc, remoteInfraDir, svc,
	)
	if err := sess.Run(cmd); err != nil {
		return fmt.Errorf("write remote plan: %w", err)
	}
	return nil
}

func dial(t SSHTarget) (*ssh.Client, error) {
	auth, err := authMethods(t.KeyPath)
	if err != nil {
		return nil, err
	}
	addr := t.Addr
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "22")
	}
	cfg := &ssh.ClientConfig{
		User:            t.User,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // dev; pin known_hosts in prod
		Timeout:         10 * time.Second,
	}
	return ssh.Dial("tcp", addr, cfg)
}

// authMethods uses the configured private key. (Agent-based auth can be
// added later; for now each host's ssh { key = … } is required.)
func authMethods(keyPath string) ([]ssh.AuthMethod, error) {
	if keyPath == "" {
		return nil, fmt.Errorf("no ssh key configured for host (set ssh { key = … } in cluster.hcl)")
	}
	b, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("parse key %s: %w", keyPath, err)
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
}

// propertiesToLabelsArg renders a map[k]v as a `--labels=k=v,k=v` arg
// value for `weft host register`. Keys are sorted for determinism so
// the rendered shell command is byte-stable across runs (helps the
// pgrep idempotency guard + makes test assertions tractable).
func propertiesToLabelsArg(props map[string]string) string {
	if len(props) == 0 {
		return ""
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+props[k])
	}
	return strings.Join(parts, ",")
}

// run executes one remote command and returns its combined output.
func run(cl *ssh.Client, cmd string) (string, error) {
	sess, err := cl.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}
