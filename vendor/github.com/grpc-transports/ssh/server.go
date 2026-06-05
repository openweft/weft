package sshtransport

import (
	"fmt"
	"log"
	"net"
	"os"

	"golang.org/x/crypto/ssh"
)

const grpcChannelType = "grpc"

// ServerConfig holds the SSH server configuration.
type ServerConfig struct {
	// HostKey is the path to the SSH host private key (PEM). Generated on first
	// start if the file does not exist.
	HostKeyPath string
	// AuthorizedKeysPath is the path to a file in authorized_keys format.
	// When empty, public-key file-based auth is skipped — AuthCallback (if
	// set) is then the only auth path.
	AuthorizedKeysPath string
	// AuthCallback, when non-nil, is consulted BEFORE AuthorizedKeysPath.
	// If it returns a non-nil *ssh.Permissions and a nil error, the client
	// is authorised and no fallback is attempted. If it returns an error,
	// the server falls back to AuthorizedKeysPath. Useful for plugging in
	// non-file-based verifiers — OpenPubkey, FIDO2/WebAuthn, sigstore, an
	// internal cert authority — without forking the module. See README for
	// an OpenPubkey integration sketch.
	AuthCallback func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error)
	// Logger is used for connection-level messages. Defaults to log.Default().
	Logger *log.Logger
}

// ListenSSH starts a listener on addr (e.g. "unix:/path" or "tcp:0.0.0.0:2222")
// and returns a net.Listener that yields one net.Conn per incoming SSH "grpc"
// channel. Pass the returned listener directly to grpc.Server.Serve.
func ListenSSH(addr string, cfg ServerConfig) (net.Listener, error) {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}

	hostKey, err := loadOrCreateHostKey(cfg.HostKeyPath)
	if err != nil {
		return nil, fmt.Errorf("host key: %w", err)
	}

	authorized, err := loadAuthorizedKeys(cfg.AuthorizedKeysPath)
	if err != nil {
		return nil, fmt.Errorf("authorized_keys: %w", err)
	}

	sshCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			// 1. Try the user-supplied verifier first (OpenPubkey / FIDO2 / …).
			//    A nil error wins outright; a non-nil error falls through to
			//    the file-based check so a misconfigured callback can't lock
			//    out operators with a legit authorized_keys entry.
			if cfg.AuthCallback != nil {
				if perms, err := cfg.AuthCallback(conn, key); err == nil && perms != nil {
					return perms, nil
				}
			}
			// 2. authorized_keys file (the classic SSH path).
			if isAuthorized(key, authorized) {
				return &ssh.Permissions{
					Extensions: map[string]string{"pubkey-fp": ssh.FingerprintSHA256(key)},
				}, nil
			}
			return nil, fmt.Errorf("key %s not authorized", ssh.FingerprintSHA256(key))
		},
	}
	sshCfg.AddHostKey(hostKey)

	network, address := splitAddr(addr)
	lis, err := net.Listen(network, address)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}

	conns := make(chan net.Conn, 64)
	go acceptLoop(lis, sshCfg, conns, cfg.Logger)
	return &chanListener{ch: conns, addr: lis.Addr()}, nil
}

// acceptLoop accepts raw TCP/Unix connections and negotiates SSH on each.
func acceptLoop(lis net.Listener, sshCfg *ssh.ServerConfig, out chan<- net.Conn, logger *log.Logger) {
	for {
		raw, err := lis.Accept()
		if err != nil {
			return
		}
		go handleConn(raw, sshCfg, out, logger)
	}
}

func handleConn(raw net.Conn, sshCfg *ssh.ServerConfig, out chan<- net.Conn, logger *log.Logger) {
	defer raw.Close()
	sconn, chans, reqs, err := ssh.NewServerConn(raw, sshCfg)
	if err != nil {
		logger.Printf("sshtransport: SSH handshake from %s: %v", raw.RemoteAddr(), err)
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != grpcChannelType {
			newCh.Reject(ssh.UnknownChannelType, "only 'grpc' channel supported")
			continue
		}
		ch, requests, err := newCh.Accept()
		if err != nil {
			logger.Printf("sshtransport: accept channel: %v", err)
			continue
		}
		go ssh.DiscardRequests(requests)
		out <- wrapChannel(ch, sconn.LocalAddr().String(), sconn.RemoteAddr().String())
	}
}

// ── chanListener ─────────────────────────────────────────────────────────

type chanListener struct {
	ch   <-chan net.Conn
	addr net.Addr
}

func (l *chanListener) Accept() (net.Conn, error) {
	c, ok := <-l.ch
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}

func (l *chanListener) Close() error   { return nil }
func (l *chanListener) Addr() net.Addr { return l.addr }

// ── helpers ──────────────────────────────────────────────────────────────

func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return generateAndSaveHostKey(path)
	}
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(data)
}

func loadAuthorizedKeys(path string) ([]ssh.PublicKey, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []ssh.PublicKey
	for len(data) > 0 {
		key, _, _, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			break
		}
		keys = append(keys, key)
		data = rest
	}
	return keys, nil
}

func isAuthorized(key ssh.PublicKey, authorized []ssh.PublicKey) bool {
	for _, a := range authorized {
		if ssh.FingerprintSHA256(a) == ssh.FingerprintSHA256(key) {
			return true
		}
	}
	return false
}

// splitAddr splits "unix:/path" or "tcp:addr" into (network, address).
func splitAddr(addr string) (string, string) {
	for _, prefix := range []string{"unix:", "tcp:"} {
		if len(addr) > len(prefix) && addr[:len(prefix)] == prefix {
			return addr[:len(prefix)-1], addr[len(prefix):]
		}
	}
	return "tcp", addr
}
