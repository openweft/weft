# ssh

Transparent SSH tunnel transport layer for gRPC. The server wraps inbound SSH connections as `net.Conn` for a standard `grpc.Server`; the client provides a `grpc.DialOption` that opens gRPC channels over SSH.

Supports Ed25519 host-key auto-generation and SSH agent forwarding for client authentication.

For inter-VM gRPC where SSH's per-user auth model is a poor fit, see the sibling [`wireguard`](https://github.com/grpc-transports/wireguard).

## Module

```
github.com/grpc-transports/ssh
```

## API

### Server

```go
type ServerConfig struct {
    HostKeyPath        string      // path to Ed25519 host key (auto-generated if missing)
    AuthorizedKeysPath string      // path to authorized_keys file
    Logger             *log.Logger
}

// ListenSSH starts an SSH server and returns a net.Listener of gRPC-ready
// connections; pass directly to grpc.Server.Serve.
func ListenSSH(addr string, cfg ServerConfig) (net.Listener, error)
```

### Client

```go
// DialOption returns a grpc.DialOption that tunnels all gRPC traffic over SSH.
// keyPath: path to private key, path to .pub file (selects matching agent key),
//          or "" (all keys offered by the SSH agent).
func DialOption(addr, keyPath, knownHostsPath string) (grpc.DialOption, error)
```

## Usage

**Server side (e.g. `weft agent`):**

```go
lis, err := sshtransport.ListenSSH("unix:"+socketPath, sshtransport.ServerConfig{
    HostKeyPath:        "~/.weft/agent_host_key",
    AuthorizedKeysPath: "~/.weft/authorized_keys",
    Logger:             logger,
})
grpcServer.Serve(lis)
```

**Client side (e.g. `weft` CLI):**

```go
opt, err := sshtransport.DialOption("unix:"+sshSocket, keyPath, "")
conn, err := grpc.Dial("passthrough:///target", opt)
```

## Pluggable verifiers (OpenPubkey, FIDO2, sigstore)

`ServerConfig.AuthCallback` is a hook called BEFORE the `authorized_keys`
file check. It receives the client's public key and returns either a
`*ssh.Permissions` (authorised) or an error (fall through to the file).
This is the seam for verifiers that don't rely on long-lived static
public keys.

### OpenPubkey sketch

[OpenPubkey](https://github.com/openpubkey/openpubkey) replaces the
"pre-distribute SSH public keys" model with "verify an OIDC ID-token
signed by a trusted IdP at connect time". Useful for ephemeral microVM
workloads where there's no human to distribute keys to.

```go
import (
    "context"

    sshtransport "github.com/grpc-transports/ssh"
    "github.com/openpubkey/openpubkey/verifier"
    "github.com/openpubkey/openpubkey/providers"
    "golang.org/x/crypto/ssh"
)

func openpubkeyCallback(v *verifier.Verifier) func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
    return func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
        // Convention: the client sends the PK Token base64-encoded as the
        // SSH key comment field. Other transports (cert extension, custom
        // SSH request) work too — pick whichever your client supports.
        pkt, err := extractPKTokenFromKey(key)
        if err != nil {
            return nil, err
        }
        if err := v.VerifyPKToken(context.Background(), pkt); err != nil {
            return nil, err
        }
        return &ssh.Permissions{
            Extensions: map[string]string{
                "auth":   "openpubkey",
                "issuer": pkt.Op.Issuer(),
                "sub":    pkt.Op.Subject(),
            },
        }, nil
    }
}

// Server wiring:
v := verifier.New([]providers.OpenIdProvider{
    providers.NewGoogleOp(),     // or your enterprise IdP
})
lis, _ := sshtransport.ListenSSH("unix:/tmp/agent.sock", sshtransport.ServerConfig{
    HostKeyPath:  "/etc/weft/host_key",
    AuthCallback: openpubkeyCallback(v),
    // AuthorizedKeysPath stays optional — when set, breakglass keys still work.
})
```

A returning-error callback falls back to `AuthorizedKeysPath`, so an
operator misconfiguring the OpenPubkey verifier can't lock themselves
out if they kept a breakglass key in the file.

## Used by

- [`openweft/weft`](https://github.com/openweft/weft) — SSH-secured gRPC listener
- [`openweft/weft-client`](https://github.com/openweft/weft-client) — SSH-tunnelled gRPC client
- [`openweft/weft-webui`](https://github.com/openweft/weft-webui) — SSH-tunnelled gRPC client
- [`openweft/terraform-provider-weft`](https://github.com/openweft/terraform-provider-weft) — provider gRPC transport
