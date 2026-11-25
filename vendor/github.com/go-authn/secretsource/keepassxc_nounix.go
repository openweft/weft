//go:build !unix

package secretsource

import "net"

// checkSocket and checkPeer: no Unix sockets here (KeePassXC on Windows uses a
// named pipe, not implemented yet), so socketCandidates fails before these are
// reached.
func checkSocket(string) error { return nil }

func checkPeer(net.Conn) error { return nil }

// checkDir: Windows has no owner or mode bits to read; the configuration
// directory is under the user's profile, whose ACL keeps others out.
func checkDir(string) error { return nil }
