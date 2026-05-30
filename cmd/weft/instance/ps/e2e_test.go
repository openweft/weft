package ps

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	wgtransport "github.com/grpc-transports/wireguard"
	introspectv1 "github.com/openweft/weft-proto/introspectv1"
	"golang.org/x/crypto/curve25519"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeIntrospect is a stand-in for the VM-side server (the real one lives
// in weft-microvm-agent and is tested there); here we only need a deterministic
// response to prove the transport + dial path.
type fakeIntrospect struct {
	introspectv1.UnimplementedIntrospectServer
}

func (fakeIntrospect) ListProcesses(context.Context, *introspectv1.ListProcessesRequest) (*introspectv1.ListProcessesResponse, error) {
	return &introspectv1.ListProcessesResponse{
		Processes: []*introspectv1.Process{
			{Pid: 1, Ppid: 0, User: "root", State: "S", Command: "/sbin/init"},
		},
	}, nil
}

// genKey writes a fresh WireGuard private key to a temp file and returns
// its path plus the base64 public key.
func genKey(t *testing.T) (privPath, pubB64 string) {
	t.Helper()
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		t.Fatal(err)
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	privPath = filepath.Join(t.TempDir(), "wg_priv")
	if err := os.WriteFile(privPath, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		t.Fatal(err)
	}
	return privPath, base64.StdEncoding.EncodeToString(pub)
}

// TestPS_EndToEndOverWireGuard runs the Introspect server behind a userspace
// WireGuard device and dials it through the exact transport path the `ps`
// command uses (buildClientConfig → wgtransport.DialOption → gRPC), proving
// the full CLI→VM chain works over an encrypted overlay.
func TestPS_EndToEndOverWireGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e WireGuard test (skipped in -short)")
	}

	const serverPort = 51899
	serverOverlay := "10.9.0.1:51999"

	serverPriv, serverPub := genKey(t)
	clientPriv, clientPub := genKey(t)

	// Server side: WireGuard listener bound to the overlay, serving Introspect.
	lis, err := wgtransport.ListenWireGuard(serverOverlay, wgtransport.ServerConfig{
		PrivateKeyPath: serverPriv,
		LocalIP:        netip.MustParseAddr("10.9.0.1"),
		ListenPort:     serverPort,
		Peers: []wgtransport.Peer{{
			PublicKey:  clientPub,
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.9.0.2/32")},
		}},
	})
	if err != nil {
		t.Fatalf("ListenWireGuard: %v", err)
	}
	defer lis.Close()

	gs := grpc.NewServer()
	introspectv1.RegisterIntrospectServer(gs, fakeIntrospect{})
	go gs.Serve(lis)
	defer gs.Stop()

	// Client side: the command's own config builder + transport.
	cfg, err := buildClientConfig(serverOverlay, "10.9.0.2", serverPub, "127.0.0.1:51899", "10.9.0.1/32", 1)
	if err != nil {
		t.Fatalf("buildClientConfig: %v", err)
	}
	cfg.PrivateKeyPath = clientPriv

	opt, err := wgtransport.DialOption(serverOverlay, cfg)
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	conn, err := grpc.NewClient("passthrough:///"+serverOverlay,
		grpc.WithTransportCredentials(insecure.NewCredentials()), opt)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := introspectv1.NewIntrospectClient(conn).ListProcesses(ctx, &introspectv1.ListProcessesRequest{})
	if err != nil {
		t.Fatalf("ListProcesses over WireGuard: %v", err)
	}
	if len(resp.Processes) != 1 || resp.Processes[0].Command != "/sbin/init" {
		t.Fatalf("unexpected response: %+v", resp.Processes)
	}

	// Sanity: render path must not error on the live response.
	if err := renderTable(io.Discard, resp.Processes); err != nil {
		t.Errorf("renderTable: %v", err)
	}
}
