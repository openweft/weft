package cluster

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSH is the chosen cluster access model: weft up reaches each hypervisor
// over SSH to install/start its agent and drive the per-host deploy. The
// command rendering here is pure (and tested); the live transport (Apply)
// uses golang.org/x/crypto/ssh.

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
func renderAction(c *Cluster, a Action) (hostID, command string) {
	seed := c.Seed()
	switch a.Kind {
	case EnsureHost:
		hv := ""
		for _, h := range c.Hosts {
			if h.ID == a.Host && h.Hypervisor != "" {
				hv = " --hypervisor=" + h.Hypervisor
			}
		}
		// Prepend the driver-plugin OCI config so the agent pulls the right
		// weft-driver-* from the configured registry when it's not pre-installed.
		env := ""
		if e := c.Drivers.Env(); len(e) > 0 {
			env = strings.Join(e, " ") + " "
		}
		if a.Host == seed.ID {
			return a.Host, env + "weft agent --server" + hv + "   # seed control-plane"
		}
		return a.Host, fmt.Sprintf("%sweft agent --client --control-plane=%s%s   # join seed", env, seed.Address, hv)
	case MeshSync:
		return seed.ID, "# mesh: publish overlay peer set to [" + strings.Join(a.Hosts, ",") + "] (wgcoord + mesh.PublishAll)"
	case PlaceReplica:
		return a.Host, fmt.Sprintf("weft infra deploy %s   # replica %d (dc=%s)", a.Service, a.Replica, a.DC)
	case GrowQuorum:
		return seed.ID, fmt.Sprintf("# grow %s quorum %d→%d (etcd member-add / nats route)", a.Service, a.From, a.To)
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
func Apply(c *Cluster, p *Plan, logf func(string, ...any)) error {
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
