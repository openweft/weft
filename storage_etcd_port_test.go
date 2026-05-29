package weft

// storage_etcd_port_test.go is the freePort helper realisation —
// kept in its own file so net.Listen doesn't leak into the body
// of the embedded-etcd test (which already imports a lot).

import (
	"net"
)

type tcpZeroListener struct {
	l *net.TCPListener
}

func openZeroListener() (freePortListener, error) {
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return nil, err
	}
	return &tcpZeroListener{l: l}, nil
}

func (z *tcpZeroListener) close() error { return z.l.Close() }
func (z *tcpZeroListener) port() int    { return z.l.Addr().(*net.TCPAddr).Port }
