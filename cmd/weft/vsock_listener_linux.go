//go:build linux

package main

// vsock_listener_linux.go binds an AF_VSOCK listener on the host so
// guest microVMs can dial the agent's gRPC server. The listener
// returns net.Conn instances ; the existing grpc.Server.Serve()
// loop is transport-agnostic — it picks up vsock conns the same
// way it picks up Unix or TCP ones.
//
// Wire shape :
//   host listens : socket(AF_VSOCK, SOCK_STREAM) + bind to
//     {family=AF_VSOCK, cid=VMADDR_CID_ANY, port=hostPort} + listen()
//   guest dials  : VsockCIDHost (2) + hostPort  (see weft-microvm-
//                  init/pkg/transport/vsock.go Dial())
//
// AF_VSOCK is a Linux-only address family ; cmd/weft/vsock_listener_
// other.go provides a no-op constructor for darwin/freebsd builds so
// the call site (main.go) doesn't need a build-tag fork.

import (
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// AF_VSOCK on Linux. Not exposed by stdlib syscall on every Go
// version ; the constant is stable across kernels.
const afVsock = 40

// CID values reserved by the kernel.
const (
	vsockCIDAny  uint32 = 0xffffffff // VMADDR_CID_ANY
	vsockCIDHost uint32 = 2          // VMADDR_CID_HOST
)

// sockaddrVM mirrors struct sockaddr_vm (linux/vm_sockets.h).
type sockaddrVM struct {
	family   uint16
	reserved uint16
	port     uint32
	cid      uint32
	zero     [4]byte
}

// vsockListener satisfies net.Listener over AF_VSOCK. Accept returns
// a net.FileConn-wrapped vsock socket ; Close shuts down the underlying
// fd. addr() returns a stable label for logging.
type vsockListener struct {
	fd     int
	port   uint32
	closed atomic.Bool
}

// listenVsock binds AF_VSOCK on (CID_ANY, port). port=0 lets the
// kernel pick an ephemeral port — operators usually set a stable
// one in cluster.hcl so guests can hard-code it in their cmdline.
func listenVsock(port uint32) (net.Listener, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	sa := sockaddrVM{family: afVsock, port: port, cid: vsockCIDAny}
	if _, _, errno := syscall.RawSyscall(
		syscall.SYS_BIND, uintptr(fd),
		uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa),
	); errno != 0 {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("vsock bind (port=%d): %w", port, errno)
	}
	if err := syscall.Listen(fd, 64); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	return &vsockListener{fd: fd, port: port}, nil
}

// Accept blocks until a guest connection lands + returns it wrapped
// in a vsockConn that surfaces the peer's CID via RemoteAddr(). The
// gRPC server stack then propagates that addr to handlers via the
// standard peer.FromContext() machinery, so handlers like
// GuestPodPlane.Attach can refuse calls that didn't come from a
// guest VM (peer CID == CID_HYPERVISOR / CID_LOCAL / CID_HOST means
// the conn isn't from a guest — those are reserved values).
func (l *vsockListener) Accept() (net.Conn, error) {
	if l.closed.Load() {
		return nil, net.ErrClosed
	}
	var sa sockaddrVM
	salen := unsafe.Sizeof(sa)
	cfd, _, errno := syscall.Syscall(
		syscall.SYS_ACCEPT, uintptr(l.fd),
		uintptr(unsafe.Pointer(&sa)),
		uintptr(unsafe.Pointer(&salen)),
	)
	if errno != 0 {
		if l.closed.Load() {
			return nil, net.ErrClosed
		}
		return nil, fmt.Errorf("vsock accept: %w", errno)
	}
	f := os.NewFile(cfd, fmt.Sprintf("vsock://%d:%d", sa.cid, sa.port))
	conn, err := net.FileConn(f)
	_ = f.Close() // FileConn dups the fd
	if err != nil {
		return nil, fmt.Errorf("vsock fileconn: %w", err)
	}
	return &vsockConn{Conn: conn, peerCID: sa.cid, peerPort: sa.port, localPort: l.port}, nil
}

// vsockConn wraps the net.FileConn so RemoteAddr() returns a
// vsockAddr the gRPC handlers can read via peer.FromContext().
type vsockConn struct {
	net.Conn
	peerCID   uint32
	peerPort  uint32
	localPort uint32
}

func (c *vsockConn) RemoteAddr() net.Addr {
	return vsockAddr{cid: c.peerCID, port: c.peerPort}
}

func (c *vsockConn) LocalAddr() net.Addr {
	return vsockAddr{cid: vsockCIDAny, port: c.localPort}
}

func (l *vsockListener) Close() error {
	if !l.closed.CompareAndSwap(false, true) {
		return nil
	}
	return syscall.Close(l.fd)
}

// Addr returns a stable label used by gRPC + the log statements
// at boot. The "vsock://" scheme isn't a real URL ; it's a
// human-readable handle.
func (l *vsockListener) Addr() net.Addr { return vsockAddr{port: l.port} }

// vsockAddr identifies one end of a vsock connection. CID is the
// kernel's "context id" — VMADDR_CID_HYPERVISOR=0, CID_LOCAL=1,
// CID_HOST=2 are reserved ; guest CIDs are 3+ (assigned at VM
// launch by the hypervisor backend). Handlers cast a peer.Addr to
// vsockAddr to read these.
type vsockAddr struct {
	cid  uint32
	port uint32
}

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("vsock://%d:%d", a.cid, a.port) }
func (a vsockAddr) CID() uint32     { return a.cid }
func (a vsockAddr) Port() uint32    { return a.port }


// vsockSupported reports whether the running kernel exposes AF_VSOCK
// at all. Cheap probe : open + immediately close a socket. Returns
// false on non-Linux (the build tag handles that ; this is the
// runtime-feature check for Linux without the vsock kmod loaded).
func vsockSupported() bool {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM, 0)
	if err != nil {
		return false
	}
	_ = syscall.Close(fd)
	return true
}
