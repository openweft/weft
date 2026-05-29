# wireguard

Userspace-WireGuard transport layer for gRPC, designed for inter-VM communication regardless of physical location. The server exposes a `net.Listener` whose connections are reached over a WireGuard overlay; the client provides a `grpc.DialOption` that tunnels gRPC through the same overlay.

No kernel modules, no `wg-quick`, no host-level configuration: the entire WireGuard data path (Noise handshake, encrypted UDP transport, virtual TCP/IP stack) runs in-process via [`wireguard-go`](https://github.com/WireGuard/wireguard-go) and gVisor netstack.

## Module

```
github.com/grpc-transports/wireguard
```

## When to use

- Inter-VM gRPC across hosts / availability zones where no overlay (Tailscale, Cilium, host-level WireGuard) is already in place
- Micro-VM workloads provisioned by a central controller that can distribute keys
- Workloads where SSH-style per-user keys are a poor fit (ephemeral compute, no human auth)

For VM ↔ VM on the same host, prefer vsock. For workloads with a human-driven CLI client, prefer [`ssh`](../ssh).

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

_(none yet — created as a sibling of `ssh` to cover the cross-host VM-to-VM scenario)_
