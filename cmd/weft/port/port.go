// Package port implements the `weft port` subcommand group : a thin
// CLI wrapper over the ListPortsForVM RPC that landed in proto v0.11.6.
//
//	weft port ls --vm <name> [--project=<name|uuid>] [--format=json]
//
// Read-only today — Port records are created at VM-create time by the
// adapter ; no UpdatePort RPC in proto v0.11.6, so the CLI doesn't
// surface mutations either. Future addition will be SetPortSecurityGroups
// (the adapter has the surface ; only a proto RPC is missing).
package port

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

// Command returns the `weft port` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "port",
		Short: "Inspect VM NIC bindings (Ports)",
	}
	cmd.AddCommand(lsCmd(socket, sshSocket, sshKey))
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var vmName, vmUUID, project, format string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List Ports attached to a VM (MAC/IP/security-groups/QoS)",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if vmName == "" && vmUUID == "" {
				return fmt.Errorf("--vm or --vm-uuid is required")
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListPortsForVM(context.Background(), &weftv1.ListPortsForVMRequest{
				VmUuid: vmUUID, VmName: vmName, Project: project,
			})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpPortsJSON(resp.GetPorts())
			}
			return renderPortsTable(resp.GetPorts())
		},
	}
	cmd.Flags().StringVar(&vmName, "vm", "", "VM name (requires --project for cross-project lookup)")
	cmd.Flags().StringVar(&vmUUID, "vm-uuid", "", "VM UUID (alternative to --vm)")
	cmd.Flags().StringVar(&project, "project", "", "Project of the VM (name or UUID) ; required with --vm when ambiguous")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func renderPortsTable(ports []*weftv1.PortInfo) error {
	if len(ports) == 0 {
		fmt.Println("(no ports — VM may not be booted yet, or has no NIC attached)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MAC\tIP\tNETWORK\tSGS\tINGRESS\tEGRESS")
	for _, p := range ports {
		sgs := "—"
		if n := len(p.GetSecurityGroups()); n > 0 {
			sgs = fmt.Sprintf("%d", n)
		}
		ing := rateOrDash(int(p.GetIngressMbps()))
		egr := rateOrDash(int(p.GetEgressMbps()))
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			defaultDash(p.GetMac()), defaultDash(p.GetIp()),
			shortenUUID(p.GetNetworkUuid()), sgs, ing, egr)
	}
	return tw.Flush()
}

func dumpPortsJSON(ports []*weftv1.PortInfo) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(ports)
}

func defaultDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// shortenUUID returns the first 8 chars of a uuid-like string so the
// table stays readable. Full uuids land in --format=json.
func shortenUUID(s string) string {
	if i := strings.Index(s, "-"); i > 0 && i <= 8 {
		return s[:i]
	}
	if len(s) > 8 {
		return s[:8]
	}
	return defaultDash(s)
}

func rateOrDash(mbps int) string {
	if mbps <= 0 {
		return "—"
	}
	return fmt.Sprintf("%dMbps", mbps)
}
