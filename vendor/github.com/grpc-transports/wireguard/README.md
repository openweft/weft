# wireguard

WireGuard transport layer for gRPC, designed for inter-VM communication regardless of physical location. The server exposes a `net.Listener` whose connections are reached over a WireGuard overlay; the client provides a `grpc.DialOption` that tunnels gRPC through the same overlay.

Two backends ship side-by-side; pick the one that matches the environment.

| | `BackendUserspace` (default) | `BackendKernel` |
|---|---|---|
| **OS** | any (Linux / darwin / Windows) | Linux only |
| **Privileges** | none | `CAP_NET_ADMIN` |
| **Data path** | wireguard-go + gVisor netstack, in-process | kernel WireGuard module (`net/wireguard.ko`) |
| **Visibility** | invisible to host tools (no netdev, no host route) | regular wg* netdev, visible to `ip`, `iptables`, `ss` |
| **Throughput** | ~1-3 Gbps single-core (Go AEAD) | line-rate (kernel AES-NI / ChaCha20 ASM) |
| **When** | dev, multi-OS, sandboxed | inside Linux microVMs with `CONFIG_WIREGUARD` |

The microVM case is the one that motivated kernel mode: weft microVMs ship a kernel built with `CONFIG_WIREGUARD=y`, so re-implementing crypto + TCP/IP in userspace is wasted CPU and hides the overlay from `iptables`. Set `Backend: BackendKernel` in those VMs.

## Module

```
github.com/grpc-transports/wireguard
```

## When to use

- Inter-VM gRPC across hosts / availability zones where no overlay (Tailscale, Cilium, host-level WireGuard) is already in place
- Micro-VM workloads provisioned by a central controller that can distribute keys
- Workloads where SSH-style per-user keys are a poor fit (ephemeral compute, no human auth)

For VM ↔ VM on the same host, prefer vsock. For workloads with a human-driven CLI client, prefer [`ssh`](https://github.com/grpc-transports/ssh).

## Kernel backend

```go
lis, err := wgtransport.ListenWireGuard("10.0.0.1:50051", wgtransport.ServerConfig{
    Backend:        wgtransport.BackendKernel,    // ← kernel WireGuard
    InterfaceName:  "wg-svc",                     // optional, auto if empty
    PrivateKeyPath: "/etc/weft/wg_priv",
    LocalIP:        netip.MustParseAddr("10.0.0.1"),
    ListenPort:     51820,
    PeersPath:      "/etc/weft/wg_peers",
})
```

The bring-up runs five netlink + wgctrl steps. **No shell, no `iproute2`, no `wireguard-tools` required in the rootfs** — everything is pure Go via [`github.com/vishvananda/netlink`](https://github.com/vishvananda/netlink) (raw netlink syscalls for link/addr/route) and [`golang.zx2c4.com/wireguard/wgctrl`](https://pkg.go.dev/golang.zx2c4.com/wireguard/wgctrl) (Donenfeld's official Go library for the kernel WireGuard netlink contract).

The table below shows the equivalent shell command (operator mental model, what `wg-quick` would run) next to the actual Go call site:

| | Equivalent shell | Actual Go call |
|---|---|---|
| 1 | `ip link add wg-svc type wireguard` | `netlink.LinkAdd(&GenericLink{LinkType: "wireguard"})` |
| 2 | `wg set wg-svc private-key … listen-port … peer …` | `wgctrl.Client.ConfigureDevice(ifname, wgtypes.Config{…})` |
| 3 | `ip addr add 10.0.0.1/32 dev wg-svc` | `netlink.AddrAdd(link, &Addr{IPNet: …})` |
| 4 | `ip route add <peer-allowed-ip> dev wg-svc` | `netlink.RouteAdd(&Route{LinkIndex, Dst, Scope: SCOPE_LINK})` |
| 5 | `ip link set wg-svc up` | `netlink.LinkSetUp(link)` |

Caveats:

- **Linux only** — non-Linux builds compile but return an error at runtime if `Backend = BackendKernel`.
- **Privileges** — the calling process needs `CAP_NET_ADMIN`. Running as root works; the cleanest path is `setcap 'cap_net_admin=ep' /path/to/binary` or a systemd unit with `AmbientCapabilities=CAP_NET_ADMIN`.
- **Conflicting interfaces** — if `InterfaceName` names an interface that already exists, the bring-up reuses it. Useful for operator-managed wg0 setups where the netdev should persist across daemon restarts. Auto-generated names (`wg-<8 hex>`) are deleted on `Close()`.
- **Routing scope** — the assigned LocalIP gets a `/32` (or `/128` for v6) — a deliberately narrow scope so we don't hijack traffic that shouldn't go over the overlay. Operators wanting a broader on-link subnet should add routes after the fact.

## API

### Server

```go
type ServerConfig struct {
    PrivateKeyPath string      // base64 Curve25519 key (auto-generated if missing)
    LocalIP        netip.Addr  // virtual IP on the overlay
    ListenPort     uint16      // UDP underlay port (0 = ephemeral)
    Peers          []Peer      // authorized clients (use PeersPath as alternative)
    PeersPath      string      // path to peer file (one peer per line)
    MTU            int         // 0 = default (1420)
    Logger         *log.Logger
}

// ListenWireGuard brings up a userspace WireGuard device, listens for TCP
// connections on addr (an ip:port on the overlay) via in-process netstack,
// and returns a net.Listener suitable for grpc.Server.Serve.
func ListenWireGuard(addr string, cfg ServerConfig) (net.Listener, error)
```

### Client

```go
type ClientConfig struct {
    PrivateKeyPath string
    LocalIP        netip.Addr
    Peer           Peer        // server peer; Endpoint must be set
    MTU            int
    Logger         *log.Logger
}

// DialOption returns a grpc.DialOption that tunnels all gRPC traffic over
// WireGuard to the overlay address addr (ip:port).
func DialOption(addr string, cfg ClientConfig) (grpc.DialOption, error)
```

### Peer

```go
type Peer struct {
    PublicKey           string         // base64 (32 bytes)
    AllowedIPs          []netip.Prefix // overlay prefixes reachable via this peer
    Endpoint            string         // "host:port" underlay (required on client)
    PersistentKeepalive uint16         // seconds; 0 = disabled
}
```

### Peer file format

One peer per line, whitespace-separated:

```
<base64-pubkey> <allowed-ip>[,<allowed-ip>...] [<endpoint:port>] [<keepalive>]
```

## Usage

**Server (VM A, virtual IP `10.0.0.1`, UDP port 51820):**

```go
lis, err := wgtransport.ListenWireGuard("10.0.0.1:50051", wgtransport.ServerConfig{
    PrivateKeyPath: "~/.weft/wg_priv",
    LocalIP:        netip.MustParseAddr("10.0.0.1"),
    ListenPort:     51820,
    PeersPath:      "~/.weft/wg_peers",
})
grpcServer.Serve(lis)
```

**Client (VM B, virtual IP `10.0.0.2`):**

```go
opt, err := wgtransport.DialOption("10.0.0.1:50051", wgtransport.ClientConfig{
    PrivateKeyPath: "~/.weft/wg_priv",
    LocalIP:        netip.MustParseAddr("10.0.0.2"),
    Peer: wgtransport.Peer{
        PublicKey:           "<server-pubkey-base64>",
        AllowedIPs:          []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
        Endpoint:            "vm-a.dc1.example:51820",
        PersistentKeepalive: 25,
    },
})
conn, err := grpc.Dial("passthrough:///target", opt)
```

## Used by

- [`openweft/weft-client`](https://github.com/openweft/weft-client) — cross-host VM-to-VM gRPC dial path (consumes this lib via go modules)
- [`openweft/weft`](https://github.com/openweft/weft) — agent-side WireGuard listener for inter-DC mesh
