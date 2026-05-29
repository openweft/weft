package sshtransport

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"google.golang.org/grpc"
)

// DialOption returns a grpc.DialOption that tunnels gRPC over an SSH
// "grpc" channel to addr.
//
// addr format: "unix:/path" or "tcp:host:port"
// keyPath: path to the SSH private key (PEM); uses ssh-agent if empty.
// knownHostsPath: path to known_hosts (or "" to skip host verification — NOT for production).
func DialOption(addr, keyPath, knownHostsPath string) (grpc.DialOption, error) {
	authMethods, err := authMethods(keyPath)
	if err != nil {
		return nil, err
	}

	hostKeyCallback, err := hostKeyCallback(knownHostsPath)
	if err != nil {
		return nil, err
	}

	sshCfg := &ssh.ClientConfig{
		User:            os.Getenv("USER"),
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
	}

	network, address := splitAddr(addr)

	return grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		raw, err := new(net.Dialer).DialContext(ctx, network, address)
		if err != nil {
			return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
		}
		sconn, chans, reqs, err := ssh.NewClientConn(raw, address, sshCfg)
		if err != nil {
			raw.Close()
			return nil, fmt.Errorf("ssh handshake: %w", err)
		}
		client := ssh.NewClient(sconn, chans, reqs)
		ch, reqs2, err := client.OpenChannel(grpcChannelType, nil)
		if err != nil {
			client.Close()
			return nil, fmt.Errorf("open grpc channel: %w", err)
		}
		go ssh.DiscardRequests(reqs2)
		return wrapChannel(ch, sconn.LocalAddr().String(), address), nil
	}), nil
}

// authMethods builds the SSH auth methods from keyPath:
//
//   - Private key file (no .pub suffix) → read and use directly, no agent.
//   - Public key file (.pub suffix)     → connect to ssh-agent, use only the
//     matching signer (selected by public key fingerprint).
//   - Empty string                       → connect to ssh-agent, offer all keys.
func authMethods(keyPath string) ([]ssh.AuthMethod, error) {
	// ── Case 1: private key file ────────────────────────────────────────────
	if keyPath != "" && !strings.HasSuffix(keyPath, ".pub") {
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("read key %s: %w", keyPath, err)
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse key %s: %w", keyPath, err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}

	// ── Cases 2 & 3: ssh-agent ──────────────────────────────────────────────
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, fmt.Errorf("no --ssh-key provided and SSH_AUTH_SOCK is not set")
	}
	agentConn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("connect to ssh-agent at %s: %w", sock, err)
	}
	ag := agent.NewClient(agentConn)

	if keyPath == "" {
		// Case 3: all keys from the agent.
		return []ssh.AuthMethod{ssh.PublicKeysCallback(ag.Signers)}, nil
	}

	// Case 2: .pub file — filter agent signers by fingerprint.
	pubData, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read public key %s: %w", keyPath, err)
	}
	wantKey, _, _, _, err := ssh.ParseAuthorizedKey(pubData)
	if err != nil {
		return nil, fmt.Errorf("parse public key %s: %w", keyPath, err)
	}
	wantFP := ssh.FingerprintSHA256(wantKey)

	return []ssh.AuthMethod{ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
		all, err := ag.Signers()
		if err != nil {
			return nil, err
		}
		for _, s := range all {
			if ssh.FingerprintSHA256(s.PublicKey()) == wantFP {
				return []ssh.Signer{s}, nil
			}
		}
		return nil, fmt.Errorf("ssh-agent has no key matching %s (%s)", keyPath, wantFP)
	})}, nil
}

func hostKeyCallback(knownHostsPath string) (ssh.HostKeyCallback, error) {
	if knownHostsPath == "" {
		// InsecureIgnoreHostKey — acceptable for local Unix socket where
		// the host key is verified implicitly by filesystem permissions.
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec
	}
	data, err := os.ReadFile(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("read known_hosts %s: %w", knownHostsPath, err)
	}
	pubKey, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse known_hosts: %w", err)
	}
	return ssh.FixedHostKey(pubKey), nil
}
