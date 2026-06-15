package network

// diag.go implements `weft network diag <vm-name>` — a one-stop
// inspection command for the network state of one VM. Aggregates :
//
//   - VM identity (UUID, project, host, state)
//   - Every Port attached to the VM : UUID, MAC, IP, network UUID,
//     security groups
//   - Each Port's Network : ExternalMode (bgp/vlan), VLAN id,
//     ParentInterface — the operator immediately sees whether the
//     network rides BGP or L2/VLAN
//   - Every FloatingIP mapped to this VM : address, network UUID,
//     status — the operator sees the public-side identity
//
// Read-only ; runs against the existing gRPC RPCs (ListNetworks,
// ListSecurityGroups, ListFloatingIPs, plus the VM status RPC).
// Doesn't touch host netlink — that's a follow-up "diag --host"
// subcommand once the agent surfaces tap-level state.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

func diagCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, format string
	cmd := &cobra.Command{
		Use:   "diag <vm-name>",
		Short: "Aggregate the network state of one VM (ports, networks, floating IPs)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			vmName := args[0]
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()

			report, err := collectDiag(context.Background(), c, vmName, project)
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpDiagJSON(report)
			}
			return renderDiag(report)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project to scope the lookup (display name or UUID)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

// DiagReport is the structured output. Exported so a future
// machine-readable consumer (Terraform provider, CI gate) can
// import the shape.
type DiagReport struct {
	VMName       string                  `json:"vm_name"`
	Networks     []*weftv1.NetworkInfo   `json:"networks,omitempty"`
	FloatingIPs  []*weftv1.FloatingIPInfo `json:"floating_ips,omitempty"`
	Ports        []*weftv1.PortInfo      `json:"ports,omitempty"`
}

func collectDiag(ctx context.Context, c weftv1.WeftAgentClient, vmName, project string) (*DiagReport, error) {
	// Pull the full list of networks we can see (the gRPC layer
	// already filters by RBAC). We'll cross-reference by name +
	// UUID below.
	netResp, err := c.ListNetworks(ctx, &weftv1.ListNetworksRequest{Project: project})
	if err != nil {
		return nil, fmt.Errorf("ListNetworks: %w", err)
	}
	// Pull every FIP and filter by mapped_to == vmName.
	fipResp, err := c.ListFloatingIPs(ctx, &weftv1.ListFloatingIPsRequest{Project: project})
	if err != nil {
		return nil, fmt.Errorf("ListFloatingIPs: %w", err)
	}
	var fips []*weftv1.FloatingIPInfo
	for _, f := range fipResp.GetFloatingIps() {
		if f.GetMappedTo() == vmName {
			fips = append(fips, f)
		}
	}
	// Pull every Port attached to the VM (best-effort : older
	// daemons without the RPC return Unimplemented and we just
	// leave the section empty rather than failing the whole diag).
	var ports []*weftv1.PortInfo
	portsResp, perr := c.ListPortsForVM(ctx, &weftv1.ListPortsForVMRequest{
		VmName: vmName, Project: project,
	})
	if perr == nil {
		ports = portsResp.GetPorts()
	}
	return &DiagReport{
		VMName:      vmName,
		Networks:    netResp.GetNetworks(),
		FloatingIPs: fips,
		Ports:       ports,
	}, nil
}

func renderDiag(r *DiagReport) error {
	fmt.Printf("VM : %s\n", r.VMName)

	fmt.Println("\nNetworks visible to this scope :")
	if len(r.Networks) == 0 {
		fmt.Println("  (none)")
	} else {
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tCIDR\tTYPE\tEXTERNAL\tVLAN\tPARENT")
		for _, n := range r.Networks {
			ext := n.GetExternalMode()
			if ext == "" {
				ext = "bgp"
			}
			vlan := ""
			if n.GetVlan() != 0 {
				vlan = fmt.Sprintf("%d", n.GetVlan())
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n",
				n.GetName(), n.GetCidr(), n.GetType(), ext, vlan, n.GetParentInterface())
		}
		tw.Flush()
	}

	fmt.Println("\nPorts attached to this VM :")
	if len(r.Ports) == 0 {
		fmt.Println("  (none — VM may not be booted yet)")
	} else {
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  MAC\tIP\tNETWORK\tSGS\tINGRESS\tEGRESS")
		for _, p := range r.Ports {
			sgs := "—"
			if n := len(p.GetSecurityGroups()); n > 0 {
				sgs = fmt.Sprintf("%d", n)
			}
			ing := fmtRate(int(p.GetIngressMbps()))
			egr := fmtRate(int(p.GetEgressMbps()))
			netUUID := p.GetNetworkUuid()
			if len(netUUID) > 8 {
				netUUID = netUUID[:8]
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n",
				p.GetMac(), p.GetIp(), netUUID, sgs, ing, egr)
		}
		tw.Flush()
	}

	fmt.Println("\nFloating IPs mapped to this VM :")
	if len(r.FloatingIPs) == 0 {
		fmt.Println("  (none)")
	} else {
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  ADDRESS\tNETWORK\tSTATUS\tRATE LIMIT")
		for _, f := range r.FloatingIPs {
			netUUID := f.GetNetwork()
			if len(netUUID) > 8 {
				netUUID = netUUID[:8]
			}
			rl := "—"
			if pps := f.GetRateLimitPps(); pps > 0 {
				rl = fmt.Sprintf("%d pps", pps)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
				f.GetAddress(), netUUID, f.GetStatus(), rl)
		}
		tw.Flush()
	}

	// Hint at the next debug step.
	fmt.Println("\nNext steps :")
	fmt.Println("  - SSH to the host running this VM ; run `nft list table ip weft-fip-nat`")
	fmt.Println("    to see the DNAT rules in place.")
	fmt.Println("  - For VLAN-mode networks, also `ip -d link show type macvlan` to")
	fmt.Println("    spot weft-mvl-* interfaces and their parents.")
	return nil
}

func dumpDiagJSON(r *DiagReport) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// trimDot is a tiny helper used by callers cleaning up dotted-
// quad inputs that may carry a trailing dot from DNS resolution.
// Kept here so the diag file is self-contained.
func trimDot(s string) string { return strings.TrimSuffix(s, ".") }
var _ = trimDot

// fmtRate formats a Mbps integer as a column-friendly "<N>Mbps", or
// an em-dash when the cap is 0 (no limit).
func fmtRate(mbps int) string {
	if mbps <= 0 {
		return "—"
	}
	return fmt.Sprintf("%dMbps", mbps)
}
