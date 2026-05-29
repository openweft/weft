// Package pod defines the in-VM PodSpec — what the guest init is
// told to run. The on-the-wire form is weft-proto's guestv1.PodSpec;
// this Go type mirrors it so init code stays decoupled from the proto
// regeneration cycle.
//
// One PodSpec drives one micro-VM. A pod with a single Container is
// the mono-container case ; multiple Containers share the pod's
// network + IPC namespaces (the "infra"/pause container pattern).
package pod

import (
	"encoding/json"
	"fmt"
	"os"
)

// Spec is the full pod description handed to weft-init.
type Spec struct {
	PodID       string            `json:"pod_id"`
	Containers  []Container       `json:"containers"`
	Shares      []Share           `json:"shares,omitempty"`
	Network     *Network          `json:"network,omitempty"`
	WireGuard   *WireGuard        `json:"wireguard,omitempty"`
	ShareMounts []ShareMount      `json:"share_mounts,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// Container is one OCI container in the pod.
type Container struct {
	ID         string            `json:"id"`
	RootfsTag  string            `json:"rootfs_tag"`        // matches a Share.Tag (virtio-fs) OR a local dir under /run/weft/rootfs/<id>
	Command    []string          `json:"command,omitempty"` // overrides image entrypoint
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Workdir    string            `json:"workdir,omitempty"`
	User       string            `json:"user,omitempty"` // "uid:gid" or username
	Restart    RestartPolicy     `json:"restart,omitempty"`
	Mounts     []Mount           `json:"mounts,omitempty"`
	Resources  Resources         `json:"resources,omitempty"`
	Privileged bool              `json:"privileged,omitempty"`
}

// Share is a host-mounted filesystem exposed to the guest (typically virtio-fs).
// One share usually carries the OCI rootfs of one container.
type Share struct {
	Tag        string `json:"tag"`         // virtio-fs tag, e.g. "rootfs0"
	MountPoint string `json:"mount_point"` // where init mounts it inside the guest, e.g. "/run/weft/rootfs/web"
	Readonly   bool   `json:"readonly,omitempty"`
}

// Network is the pod-level network config. The init brings up eth0
// before any container starts ; containers join the pod netns.
type Network struct {
	Interface string   `json:"interface,omitempty"` // default "eth0"
	Address   string   `json:"address,omitempty"`   // CIDR, e.g. "10.0.2.15/24"
	Gateway   string   `json:"gateway,omitempty"`
	DNS       []string `json:"dns,omitempty"`
	Hostname  string   `json:"hostname,omitempty"`
}

// WireGuard configures an optional kernel WireGuard interface the init
// brings up after eth0 — the encrypted overlay a micro-VM uses to reach
// other VMs, or to be reached by an operator CLI. Keys are base64-encoded
// 32-byte Curve25519 values, matching `wg`'s textual form.
type WireGuard struct {
	Interface  string   `json:"interface,omitempty"`   // default "wg0"
	PrivateKey string   `json:"private_key"`           // base64, 32 bytes
	ListenPort uint16   `json:"listen_port,omitempty"` // UDP underlay port; 0 = ephemeral
	Address    string   `json:"address"`               // overlay CIDR, e.g. "10.9.0.1/24"
	Peers      []WGPeer `json:"peers,omitempty"`
}

// WGPeer is one authorized WireGuard peer on the overlay.
type WGPeer struct {
	PublicKey           string   `json:"public_key"`                     // base64, 32 bytes
	Endpoint            string   `json:"endpoint,omitempty"`             // underlay "host:port"
	AllowedIPs          []string `json:"allowed_ips"`                    // overlay CIDRs routed to this peer
	PersistentKeepalive uint16   `json:"persistent_keepalive,omitempty"` // seconds; 0 = disabled
}

type Mount struct {
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"` // "bind" | "tmpfs"
	Options     []string `json:"options,omitempty"`
}

type Resources struct {
	CPUShares uint64 `json:"cpu_shares,omitempty"`
	MemBytes  uint64 `json:"mem_bytes,omitempty"`
	PidsMax   int64  `json:"pids_max,omitempty"`
}

type RestartPolicy string

const (
	RestartNever     RestartPolicy = ""
	RestartOnFailure RestartPolicy = "on-failure"
	RestartAlways    RestartPolicy = "always"
)

// Load reads a Spec from a JSON file. Empty path or missing file is
// not an error at this layer — the caller decides whether a pod-less
// boot is valid (e.g. interactive debug shell).
func Load(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("pod spec %s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("pod spec %s: %w", path, err)
	}
	return &s, nil
}

func (s *Spec) Validate() error {
	if s.PodID == "" {
		return fmt.Errorf("pod_id is required")
	}
	if len(s.Containers) == 0 {
		return fmt.Errorf("at least one container is required")
	}
	seen := map[string]bool{}
	for i, c := range s.Containers {
		if c.ID == "" {
			return fmt.Errorf("container[%d]: id is required", i)
		}
		if seen[c.ID] {
			return fmt.Errorf("container[%d]: duplicate id %q", i, c.ID)
		}
		seen[c.ID] = true
		if c.RootfsTag == "" {
			return fmt.Errorf("container %q: rootfs_tag is required", c.ID)
		}
	}
	for i := range s.ShareMounts {
		if err := s.ShareMounts[i].Validate(); err != nil {
			return fmt.Errorf("share_mounts[%d]: %w", i, err)
		}
	}
	return nil
}
