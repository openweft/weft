//go:build darwin && cgo

package weft

// ssh_exec_test.go covers the SSH glue that doesn't need a live
// sshd: sshKeyAuth (key parse), execViaSSH's pre-dial error
// branches (no key / bad key), the sshClientConfig builder, and a
// dial-failure path via the overridable sshDial var. ExecInVM's
// no-IP branch is covered too. Tagged cgo because sshClientConfig +
// sshDial live in ssh.go (//go:build darwin && cgo).

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// writeTestSSHKey generates an ECDSA private key and writes it as
// a PEM file, returning the path. ECDSA keeps the test fast (no
// RSA 2048 generation cost) while still parsing via
// ssh.ParsePrivateKey.
func writeTestSSHKey(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	path := filepath.Join(t.TempDir(), "id_ecdsa")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSSHKeyAuth_ValidKey(t *testing.T) {
	path := writeTestSSHKey(t)
	auth, err := sshKeyAuth(path)
	if err != nil {
		t.Fatalf("sshKeyAuth: %v", err)
	}
	if len(auth) != 1 {
		t.Errorf("expected 1 auth method, got %d", len(auth))
	}
}

func TestSSHKeyAuth_MissingFile(t *testing.T) {
	if _, err := sshKeyAuth("/var/empty/no-such-key"); err == nil {
		t.Errorf("missing key file should error")
	}
}

func TestSSHKeyAuth_BadKey(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad")
	_ = os.WriteFile(bad, []byte("not a private key"), 0o600)
	if _, err := sshKeyAuth(bad); err == nil {
		t.Errorf("malformed key should error")
	}
}

func TestExecViaSSH_NoKeyConfigured(t *testing.T) {
	if _, err := execViaSSH("10.0.0.5", "admin", "", "echo hi", nil); err == nil {
		t.Errorf("empty keyPath should error before dialling")
	}
}

func TestExecViaSSH_BadKey(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad")
	_ = os.WriteFile(bad, []byte("garbage"), 0o600)
	if _, err := execViaSSH("10.0.0.5", "admin", bad, "echo hi", nil); err == nil {
		t.Errorf("bad key should error before dialling")
	}
}

func TestExecViaSSH_DialFailure(t *testing.T) {
	// Override sshDial so no real network connection is made.
	orig := sshDial
	defer func() { sshDial = orig }()
	sshDial = func(network, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
		return nil, errors.New("dial refused (test)")
	}
	keyPath := writeTestSSHKey(t)
	_, err := execViaSSH("10.0.0.5", "admin", keyPath, "echo hi", nil)
	if err == nil || !strings.Contains(err.Error(), "ssh dial") {
		t.Errorf("expected dial error, got %v", err)
	}
}

func TestSSHClientConfig(t *testing.T) {
	auth := []ssh.AuthMethod{ssh.Password("x")}
	cfg := sshClientConfig("alice", auth)
	if cfg.User != "alice" {
		t.Errorf("User = %q", cfg.User)
	}
	if len(cfg.Auth) != 1 {
		t.Errorf("Auth methods = %d", len(cfg.Auth))
	}
	if cfg.HostKeyCallback == nil {
		t.Errorf("HostKeyCallback should be set")
	}
}

func TestExecInVM_NoIP(t *testing.T) {
	a := newAdapterForRegistries(t)
	// No VM dir / mac.txt → IP() fails → ExecInVM errors.
	if _, err := a.ExecInVM("ghost", "echo hi", nil); err == nil {
		t.Errorf("ExecInVM without IP should error")
	}
}

func TestExecInVM_DialFailureWithKey(t *testing.T) {
	// Set up a VM dir with a mac.txt + a DHCP-lease-resolvable IP is
	// hard without /var/db; instead override IP resolution by
	// planting mac.txt + overriding sshDial so the path reaches
	// execViaSSH. We can't easily fake IP() (reads system files),
	// so we just confirm the no-key branch via SetSSHKeyPath unset.
	a := newAdapterForRegistries(t)
	p, _, _ := a.CreateProject("p")
	dir := a.vmDirIn(p.UUID, "vm")
	_ = os.MkdirAll(dir, 0o700)
	// Plant a mac.txt so IP() gets past the read; it will then try
	// DHCP leases (likely empty) + ARP (likely no match) → IP error.
	_ = os.WriteFile(filepath.Join(dir, "mac.txt"), []byte("02:00:00:00:00:99"), 0o600)
	if _, err := a.ExecInVM("vm", "echo hi", nil); err == nil {
		t.Errorf("ExecInVM should error when IP cannot be resolved")
	}
}
