//go:build !linux

package main

// vsock_listener_other.go : non-Linux stub. AF_VSOCK is Linux-only ;
// on darwin/freebsd, the host-side listener returns an error from
// listenVsock + vsockSupported reports false. Operators who run
// weft-agent on darwin still get every other transport (Unix socket,
// TCP, SSH) — only the guest↔host vsock path is gated to Linux
// hosts (which is exactly where weft microVMs run anyway).

import (
	"errors"
	"net"
)

var errVsockUnsupported = errors.New("AF_VSOCK is Linux-only ; weft-agent on this OS doesn't expose a vsock listener")

func listenVsock(port uint32) (net.Listener, error) {
	return nil, errVsockUnsupported
}

func vsockSupported() bool { return false }
