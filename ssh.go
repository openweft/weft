package weft

import (
	"golang.org/x/crypto/ssh"
)

// sshClientConfig returns a permissive SSH client config for VM access.
// auth must contain at least one auth method (e.g. ssh.PublicKeys(signer)).
// Password-based authentication is intentionally not supported.
func sshClientConfig(user string, auth []ssh.AuthMethod) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: user,
		Auth: auth,
		// #nosec G106 -- VMs use self-signed keys; host key checking is not applicable.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
}

// sshDial is a variable so it can be overridden in tests.
var sshDial = ssh.Dial
