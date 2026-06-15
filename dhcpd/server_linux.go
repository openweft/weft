//go:build linux

package dhcpd

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// LinuxServer is the real DHCPv4 server. It binds UDP/67 on the
// configured Interface (via SO_BINDTODEVICE so only broadcast
// traffic from that bridge / VLAN is delivered to the socket) and
// replies via UDP/68 — broadcast when the client's BROADCAST flag
// is set, unicast otherwise.
//
// Lifecycle : NewLinuxServer prepares the socket lazily ; Run does
// the actual bind + recv loop ; Run unblocks when ctx is cancelled
// or the socket errors out.
type LinuxServer struct {
	opts Options

	logger *slog.Logger

	mu   sync.Mutex
	conn *net.UDPConn // populated by Run, cleared on shutdown
}

// NewLinuxServer validates opts and returns a server ready for Run.
// Doesn't open any sockets yet — that happens in Run so a caller
// can construct the server early in start-up and surface socket
// errors via the same `Run` channel as everything else.
func NewLinuxServer(opts Options) (*LinuxServer, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return &LinuxServer{
		opts:   opts,
		logger: slog.Default().With("component", "dhcpd", "iface", opts.Interface),
	}, nil
}

// SetLogger swaps the slog.Logger used for handler-loop diagnostics.
// Nil restores the package default.
func (s *LinuxServer) SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.Default().With("component", "dhcpd", "iface", s.opts.Interface)
	}
	s.logger = l
}

// Run binds the socket and processes packets until ctx is done.
// All transient errors (parse failures, source misses) are logged
// at debug level and the loop continues — only socket-fatal errors
// break out.
func (s *LinuxServer) Run(ctx contextLike) error {
	if ctx == nil {
		return errors.New("dhcpd.LinuxServer.Run: nil ctx")
	}
	conn, err := s.listen()
	if err != nil {
		return fmt.Errorf("dhcpd: bind :67 on %s: %w", s.opts.Interface, err)
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()

	// One goroutine closes the conn when ctx is done so the
	// blocking ReadFromUDP returns with an error and the recv loop
	// exits cleanly.
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		c := s.conn
		s.conn = nil
		s.mu.Unlock()
		if c != nil {
			_ = c.Close()
		}
	}()

	s.logger.Info("dhcpd listening", "iface", s.opts.Interface, "server_ip", s.opts.ServerIP)

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			// Closed socket on ctx-cancel is the expected exit path.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("dhcpd: read: %w", err)
		}
		s.handle(conn, buf[:n])
	}
}

// listen opens UDP/67 bound to the configured interface. We
// construct the socket manually (vs `net.ListenUDP`) so we can
// call SO_BINDTODEVICE + SO_BROADCAST + SO_REUSEADDR before the
// kernel commits the binding.
func (s *LinuxServer) listen() (*net.UDPConn, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}

	cleanup := func() { _ = unix.Close(fd) }

	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		cleanup()
		return nil, fmt.Errorf("SO_REUSEADDR: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BROADCAST, 1); err != nil {
		cleanup()
		return nil, fmt.Errorf("SO_BROADCAST: %w", err)
	}
	// SO_BINDTODEVICE pins the socket to the named kernel interface
	// — only frames arriving on that NIC / bridge / VLAN reach us,
	// and replies go out the same path. Requires CAP_NET_RAW (the
	// usual setup for weft-agent ; granted via systemd or running
	// as root).
	if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, s.opts.Interface); err != nil {
		cleanup()
		return nil, fmt.Errorf("SO_BINDTODEVICE %s: %w", s.opts.Interface, err)
	}

	sa := &unix.SockaddrInet4{Port: 67} // INADDR_ANY because the device binding already restricts the path
	if err := unix.Bind(fd, sa); err != nil {
		cleanup()
		return nil, fmt.Errorf("bind :67: %w", err)
	}

	// Hand the fd to the os/net stack so we get ReadFromUDP /
	// WriteToUDP ergonomics + the runtime poller.
	f := os.NewFile(uintptr(fd), fmt.Sprintf("dhcpd-%s", s.opts.Interface))
	c, err := net.FilePacketConn(f)
	_ = f.Close() // FilePacketConn dup'd the fd ; the original os.File is no longer needed
	if err != nil {
		// FilePacketConn already closed the dup on error.
		return nil, fmt.Errorf("FilePacketConn: %w", err)
	}
	uc, ok := c.(*net.UDPConn)
	if !ok {
		_ = c.Close()
		return nil, fmt.Errorf("dhcpd: unexpected conn type %T", c)
	}
	return uc, nil
}

// handle parses one inbound packet and runs Decide. Errors don't
// kill the server ; they get logged. All policy lives in Decide
// (build-tag-free) so unit tests on darwin cover the same code
// path the linux loop runs.
func (s *LinuxServer) handle(conn *net.UDPConn, raw []byte) {
	start := time.Now()
	defer func() { recordHandleDuration(time.Since(start).Seconds()) }()

	pkt, err := Parse(raw)
	if err != nil {
		s.logger.Debug("parse failed", "err", err, "len", len(raw))
		recordPacket("drop_parse_err")
		return
	}
	d, err := Decide(pkt, s.opts)
	if err != nil {
		s.logger.Warn("decide", "mac", d.MAC, "err", err)
		recordPacket("drop_decide_err")
		return
	}
	if d.Reply == nil {
		s.logger.Debug("dropped", "mac", d.MAC, "msg_type", pkt.MessageType())
		// Distinguish unknown-mac (Resolve→false) from message
		// types we silently ignore by sentinel : d.MAC is set on
		// every non-malformed packet, and MsgType is 0 here. The
		// cleanest signal we have without re-parsing is whether
		// the inbound was a DHCP message type we'd otherwise
		// reply to.
		if mt := pkt.MessageType(); mt == MsgDiscover || mt == MsgRequest {
			recordPacket("drop_unknown_mac")
		} else {
			recordPacket("drop_unsupported")
		}
		return
	}
	if err := s.send(conn, pkt, d.Reply); err != nil {
		s.logger.Warn("send", "mac", d.MAC, "msg_type", d.MsgType, "err", err)
		recordPacket("send_err")
		return
	}
	s.logger.Info("sent", "mac", d.MAC, "msg_type", d.MsgType)
	switch d.MsgType {
	case MsgOffer:
		recordPacket("offer")
	case MsgAck:
		recordPacket("ack")
	case MsgNak:
		recordPacket("nak")
	}
}

// send writes the reply on UDP/68. v0 strategy : always broadcast
// to 255.255.255.255 (covers every switch, doesn't depend on the
// client already having ARP'd us). When the BROADCAST flag is
// off the spec lets us unicast to chaddr ; we punt that until
// there's a concrete client that needs it.
//
// SO_BINDTODEVICE ensures the broadcast goes out the right NIC
// even though the destination address is the global broadcast.
func (s *LinuxServer) send(conn *net.UDPConn, _ *Packet, payload []byte) error {
	dst := &net.UDPAddr{IP: net.IPv4bcast, Port: 68}
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := conn.WriteToUDP(payload, dst)
	return err
}

// ServerIP exposes the configured server identifier (for tests +
// for callers that want to log it once at start-up).
func (s *LinuxServer) ServerIP() netip.Addr { return s.opts.ServerIP }
