// Package overlay is vzd's per-VM WireGuard provisioning: it pairs a VM
// with the operator (via wgcoord), writes the standalone wireguard.json the
// guest's weft-init applies, and the operator coords the CLI dials with.
//
// It deliberately produces plain JSON files rather than mutating daemon
// registry state, so provisioning is self-contained per VM directory and
// fully unit-testable. The guest file is dropped into the VM's config share
// (alongside pod.json); the coords file is read by `weft instance ps
// --coords`.
package overlay

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/openweft/weft/wgcoord"
)

// Defaults for the overlay. The subnet and ports are conventions an operator
// can override.
const (
	DefaultSubnet        = "10.9.0.0/24"
	DefaultListenPort    = uint16(51820) // VM wg underlay UDP port
	DefaultAgentPort     = uint16(51999) // VM Introspect gRPC port (weft-vm-agent)
	DefaultOperatorIndex = 254           // operator's host index in the subnet

	GuestFileName  = "wireguard.json"        // staged into the VM config share
	CoordsFileName = "overlay-operator.json" // read by `weft instance ps --coords`
	operatorKey    = "operator_wg.key"       // stable per-host operator private key
)

// GuestConfig is the standalone wireguard.json weft-init reads. Its JSON
// shape mirrors weft-microvm-init/pkg/pod.WireGuard (kept in lockstep by the
// matching field tags).
type GuestConfig struct {
	Interface  string      `json:"interface,omitempty"`
	PrivateKey string      `json:"private_key"`
	ListenPort uint16      `json:"listen_port,omitempty"`
	Address    string      `json:"address"`
	Peers      []GuestPeer `json:"peers,omitempty"`
}

type GuestPeer struct {
	PublicKey           string   `json:"public_key"`
	Endpoint            string   `json:"endpoint,omitempty"`
	AllowedIPs          []string `json:"allowed_ips"`
	PersistentKeepalive uint16   `json:"persistent_keepalive,omitempty"`
}

// Coords is everything `weft instance ps` needs to dial a VM's agent over
// the overlay.
type Coords struct {
	Target        string   `json:"target"`          // VM overlay IP : agent gRPC port
	PrivateKey    string   `json:"private_key"`     // operator's private key
	LocalIP       string   `json:"local_ip"`        // operator's overlay IP
	PeerPublicKey string   `json:"peer_public_key"` // VM's public key
	PeerEndpoint  string   `json:"peer_endpoint"`   // VM underlay host:port
	AllowedIPs    []string `json:"allowed_ips"`     // overlay prefixes routed to the VM
	Keepalive     uint16   `json:"keepalive,omitempty"`
}

// Config controls one provisioning.
type Config struct {
	Subnet        string // overlay CIDR (default DefaultSubnet)
	EndpointHost  string // VM's underlay-reachable host (required: how the operator reaches the VM's wg port)
	ListenPort    uint16 // VM wg listen port (default DefaultListenPort)
	AgentPort     uint16 // VM agent gRPC port (default DefaultAgentPort)
	VMIndex       int    // VM's host index in the subnet (>= 1)
	OperatorIndex int    // operator's host index (default DefaultOperatorIndex)
	Keepalive     uint16
}

func (c *Config) withDefaults() {
	if c.Subnet == "" {
		c.Subnet = DefaultSubnet
	}
	if c.ListenPort == 0 {
		c.ListenPort = DefaultListenPort
	}
	if c.AgentPort == 0 {
		c.AgentPort = DefaultAgentPort
	}
	if c.OperatorIndex == 0 {
		c.OperatorIndex = DefaultOperatorIndex
	}
}

// EnsureOperatorKey loads the stable operator keypair from stateDir, minting
// and persisting it on first use so the CLI keeps one identity across VMs.
func EnsureOperatorKey(stateDir string) (wgcoord.Key, error) {
	path := filepath.Join(stateDir, operatorKey)
	if b, err := os.ReadFile(path); err == nil {
		priv := string(b)
		pub, err := wgcoord.PublicFromPrivate(priv)
		if err != nil {
			return wgcoord.Key{}, fmt.Errorf("operator key %s: %w", path, err)
		}
		return wgcoord.Key{Private: priv, Public: pub}, nil
	}
	k, err := wgcoord.GenerateKey()
	if err != nil {
		return wgcoord.Key{}, err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return wgcoord.Key{}, err
	}
	if err := os.WriteFile(path, []byte(k.Private), 0o600); err != nil {
		return wgcoord.Key{}, fmt.Errorf("write operator key: %w", err)
	}
	return k, nil
}

// Provision mints the VM's keypair, pairs it with the operator, and writes
// wireguard.json + overlay-operator.json into outDir. It returns the coords
// (also persisted) for immediate use.
func Provision(outDir string, cfg Config, op wgcoord.Key) (Coords, error) {
	cfg.withDefaults()
	if cfg.EndpointHost == "" {
		return Coords{}, fmt.Errorf("overlay: EndpointHost is required")
	}
	if cfg.VMIndex < 1 {
		return Coords{}, fmt.Errorf("overlay: VMIndex must be >= 1")
	}
	subnet, err := netip.ParsePrefix(cfg.Subnet)
	if err != nil {
		return Coords{}, fmt.Errorf("overlay subnet %q: %w", cfg.Subnet, err)
	}

	vmKey, err := wgcoord.GenerateKey()
	if err != nil {
		return Coords{}, err
	}

	vm, opc, err := wgcoord.Pair(wgcoord.PairInput{
		Subnet:        subnet,
		VMKey:         vmKey,
		VMIndex:       cfg.VMIndex,
		VMEndpoint:    wgcoord.Endpoint{Host: cfg.EndpointHost, Port: cfg.ListenPort},
		ListenPort:    cfg.ListenPort,
		OperatorKey:   op,
		OperatorIndex: cfg.OperatorIndex,
		Keepalive:     cfg.Keepalive,
	})
	if err != nil {
		return Coords{}, err
	}

	guest := GuestConfig{
		Interface:  vm.Interface,
		PrivateKey: vm.PrivateKey,
		ListenPort: vm.ListenPort,
		Address:    vm.Address,
	}
	for _, p := range vm.Peers {
		guest.Peers = append(guest.Peers, GuestPeer{
			PublicKey:           p.PublicKey,
			Endpoint:            p.Endpoint,
			AllowedIPs:          p.AllowedIPs,
			PersistentKeepalive: p.PersistentKeepalive,
		})
	}

	coords := Coords{
		Target:        fmt.Sprintf("%s:%d", opc.VMOverlayIP, cfg.AgentPort),
		PrivateKey:    opc.PrivateKey,
		LocalIP:       opc.LocalIP,
		PeerPublicKey: opc.VMPublicKey,
		PeerEndpoint:  opc.VMEndpoint,
		AllowedIPs:    opc.AllowedIPs,
		Keepalive:     opc.Keepalive,
	}

	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return Coords{}, err
	}
	if err := writeJSON(filepath.Join(outDir, GuestFileName), guest); err != nil {
		return Coords{}, fmt.Errorf("write guest config: %w", err)
	}
	if err := writeJSON(filepath.Join(outDir, CoordsFileName), coords); err != nil {
		return Coords{}, fmt.Errorf("write coords: %w", err)
	}
	return coords, nil
}

// LoadCoords reads an operator coords file written by Provision.
func LoadCoords(path string) (Coords, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Coords{}, err
	}
	var c Coords
	if err := json.Unmarshal(b, &c); err != nil {
		return Coords{}, fmt.Errorf("coords %s: %w", path, err)
	}
	return c, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
