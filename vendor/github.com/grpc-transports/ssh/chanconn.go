package sshtransport

import (
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// chanConn wraps an ssh.Channel as a net.Conn.
type chanConn struct {
	ssh.Channel
	localAddr  net.Addr
	remoteAddr net.Addr
}

type sshAddr struct{ s string }

func (a sshAddr) Network() string { return "ssh" }
func (a sshAddr) String() string  { return a.s }

func wrapChannel(ch ssh.Channel, local, remote string) net.Conn {
	return &chanConn{
		Channel:    ch,
		localAddr:  sshAddr{local},
		remoteAddr: sshAddr{remote},
	}
}

func (c *chanConn) LocalAddr() net.Addr                { return c.localAddr }
func (c *chanConn) RemoteAddr() net.Addr               { return c.remoteAddr }
func (c *chanConn) SetDeadline(_ time.Time) error      { return nil }
func (c *chanConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *chanConn) SetWriteDeadline(_ time.Time) error { return nil }
