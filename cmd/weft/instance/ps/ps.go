// Package ps implements `weft instance ps` — list a micro-VM's process
// table (ps aux) by dialing the VM's Introspect gRPC service directly
// over a userspace WireGuard overlay. Unlike the other instance
// subcommands this talks to the VM, not to weft, so it takes WireGuard
// peer flags instead of the weft socket/SSH flags.
package ps

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	wgtransport "github.com/grpc-transports/wireguard"
	introspectv1 "github.com/openweft/weft-proto/introspectv1"
	"github.com/openweft/weft/overlay"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Command returns the `instance ps` cobra command.
func Command() *cobra.Command {
	var (
		coordsFile   string
		keyPath      string
		localIP      string
		peerPubKey   string
		peerEndpoint string
		allowedIPs   string
		keepalive    uint
		format       string
		timeout      time.Duration
	)

	cmd := &cobra.Command{
		Use:   "ps [vm-overlay-addr]",
		Short: "List a micro-VM's processes over WireGuard (ps aux)",
		Long: `Dials a micro-VM's Introspect gRPC service directly over a userspace
WireGuard overlay and prints its process table.

The simplest form consumes a coords file produced by 'weft overlay
provision':

    weft instance ps --coords overlay-operator.json

Otherwise give the VM's wg0 address + agent port as <vm-overlay-addr>
(e.g. "10.9.0.3:51999") and the WireGuard peer flags. The connection is
end-to-end encrypted by WireGuard, so it stays confidential even from
the hypervisor host. No root or interface configuration is needed here.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, cfg, err := resolveDial(coordsFile, args, dialFlags{
				keyPath: keyPath, localIP: localIP, peerPubKey: peerPubKey,
				peerEndpoint: peerEndpoint, allowedIPs: allowedIPs, keepalive: uint16(keepalive),
			})
			if err != nil {
				return err
			}

			opt, err := wgtransport.DialOption(target, cfg)
			if err != nil {
				return fmt.Errorf("wireguard transport: %w", err)
			}

			conn, err := grpc.NewClient("passthrough:///"+target,
				grpc.WithTransportCredentials(insecure.NewCredentials()), opt)
			if err != nil {
				return fmt.Errorf("dial %s: %w", target, err)
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			resp, err := introspectv1.NewIntrospectClient(conn).
				ListProcesses(ctx, &introspectv1.ListProcessesRequest{})
			if err != nil {
				return fmt.Errorf("list processes: %w", err)
			}

			if format == "json" {
				return renderJSON(os.Stdout, resp.Processes)
			}
			return renderTable(os.Stdout, resp.Processes)
		},
	}

	home, _ := os.UserHomeDir()
	cmd.Flags().StringVar(&coordsFile, "coords", "", "path to an overlay-operator.json from `weft overlay provision` (supplies target + all WireGuard params)")
	cmd.Flags().StringVar(&keyPath, "wg-key", home+"/.weft/wg_priv", "path to this client's WireGuard private key (generated if missing)")
	cmd.Flags().StringVar(&localIP, "wg-local-ip", "", "this client's overlay IP (required), e.g. 10.0.0.99")
	cmd.Flags().StringVar(&peerPubKey, "wg-peer-key", "", "the VM's WireGuard public key, base64 (required)")
	cmd.Flags().StringVar(&peerEndpoint, "wg-peer-endpoint", "", "the VM's underlay UDP endpoint host:port (required)")
	cmd.Flags().StringVar(&allowedIPs, "wg-allowed-ips", "", "overlay prefixes routed to the VM, comma-separated (default: the target host /32)")
	cmd.Flags().UintVar(&keepalive, "wg-keepalive", 25, "persistent keepalive seconds (0 to disable)")
	cmd.Flags().StringVar(&format, "format", "", "output format (json)")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "RPC timeout")
	return cmd
}

// dialFlags carries the explicit-flag inputs for the non-coords path.
type dialFlags struct {
	keyPath      string
	localIP      string
	peerPubKey   string
	peerEndpoint string
	allowedIPs   string
	keepalive    uint16
}

// resolveDial picks the dial target + WireGuard config from either a coords
// file (preferred) or the explicit flags + positional address.
func resolveDial(coordsFile string, args []string, f dialFlags) (string, wgtransport.ClientConfig, error) {
	if coordsFile != "" {
		c, err := overlay.LoadCoords(coordsFile)
		if err != nil {
			return "", wgtransport.ClientConfig{}, err
		}
		return coordsToConfig(c)
	}
	if len(args) != 1 {
		return "", wgtransport.ClientConfig{}, fmt.Errorf("give a <vm-overlay-addr> or --coords")
	}
	target := args[0]
	cfg, err := buildClientConfig(target, f.localIP, f.peerPubKey, f.peerEndpoint, f.allowedIPs, f.keepalive)
	if err != nil {
		return "", wgtransport.ClientConfig{}, err
	}
	cfg.PrivateKeyPath = f.keyPath
	return target, cfg, nil
}

// coordsToConfig maps a provisioned coords file onto the transport config.
// The operator's private key is carried inline in the coords.
func coordsToConfig(c overlay.Coords) (string, wgtransport.ClientConfig, error) {
	local, err := netip.ParseAddr(c.LocalIP)
	if err != nil {
		return "", wgtransport.ClientConfig{}, fmt.Errorf("coords local_ip %q: %w", c.LocalIP, err)
	}
	var prefixes []netip.Prefix
	for _, cidr := range c.AllowedIPs {
		p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return "", wgtransport.ClientConfig{}, fmt.Errorf("coords allowed_ip %q: %w", cidr, err)
		}
		prefixes = append(prefixes, p)
	}
	return c.Target, wgtransport.ClientConfig{
		PrivateKey: c.PrivateKey,
		LocalIP:    local,
		Peer: wgtransport.Peer{
			PublicKey:           c.PeerPublicKey,
			AllowedIPs:          prefixes,
			Endpoint:            c.PeerEndpoint,
			PersistentKeepalive: c.Keepalive,
		},
	}, nil
}

// buildClientConfig assembles the WireGuard client config from flags. The
// private key path is filled in by the caller. target is the VM's overlay
// "ip:port"; its host supplies the default allowed-IP.
func buildClientConfig(target, localIP, peerPubKey, peerEndpoint, allowedIPs string, keepalive uint16) (wgtransport.ClientConfig, error) {
	if localIP == "" {
		return wgtransport.ClientConfig{}, fmt.Errorf("--wg-local-ip is required")
	}
	if peerPubKey == "" {
		return wgtransport.ClientConfig{}, fmt.Errorf("--wg-peer-key is required")
	}
	if peerEndpoint == "" {
		return wgtransport.ClientConfig{}, fmt.Errorf("--wg-peer-endpoint is required")
	}

	local, err := netip.ParseAddr(localIP)
	if err != nil {
		return wgtransport.ClientConfig{}, fmt.Errorf("--wg-local-ip %q: %w", localIP, err)
	}

	var prefixes []netip.Prefix
	if allowedIPs == "" {
		// Default: route only the VM's overlay address (from <target>). The
		// operator usually wants the whole subnet, but a single host route is
		// the safe minimum and keeps the command usable without extra flags.
		ap, err := netip.ParseAddrPort(target)
		if err != nil {
			return wgtransport.ClientConfig{}, fmt.Errorf("derive --wg-allowed-ips from target %q: %w (pass --wg-allowed-ips explicitly)", target, err)
		}
		vmIP := ap.Addr()
		prefixes = []netip.Prefix{netip.PrefixFrom(vmIP, vmIP.BitLen())}
	} else {
		for _, c := range strings.Split(allowedIPs, ",") {
			p, err := netip.ParsePrefix(strings.TrimSpace(c))
			if err != nil {
				return wgtransport.ClientConfig{}, fmt.Errorf("--wg-allowed-ips %q: %w", c, err)
			}
			prefixes = append(prefixes, p)
		}
	}

	return wgtransport.ClientConfig{
		LocalIP: local,
		Peer: wgtransport.Peer{
			PublicKey:           peerPubKey,
			AllowedIPs:          prefixes,
			Endpoint:            peerEndpoint,
			PersistentKeepalive: keepalive,
		},
	}, nil
}
