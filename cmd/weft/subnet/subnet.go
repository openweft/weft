// Package subnet implements the `weft subnet` subcommand group :
// CRUD over the Subnet registry (per-network IP scopes). Closes the
// Tier-3 CLI parity gap surfaced by the webui audit, promoted to a
// first-class control-plane concern in weft-proto v0.8.0.
//
//	weft subnet ls [--network=<uuid>] [--project=<...>]
//	weft subnet show <uuid>
//	weft subnet create --network=<uuid> --cidr=<...> [--gateway --dns --name --description]
//	weft subnet update <uuid> [--name --description --gateway --dns --clear-dns]
//	weft subnet rm <uuid>
package subnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft subnet` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "subnet",
		Short: "Manage subnets (per-network IP scopes)",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		showCmd(socket, sshSocket, sshKey),
		createCmd(socket, sshSocket, sshKey),
		updateCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var network, project, format string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List subnets",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListSubnets(context.Background(), &weftv1.ListSubnetsRequest{
				NetworkUuid: network,
				Project:     project,
			})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Subnets)
			}
			return renderTable(resp.Subnets)
		},
	}
	cmd.Flags().StringVar(&network, "network", "", "Filter to subnets of one parent network UUID")
	cmd.Flags().StringVar(&project, "project", "", "Filter to one project (name or UUID)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func showCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "show <uuid>",
		Short: "Show a single subnet",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.GetSubnet(context.Background(), &weftv1.GetSubnetRequest{Uuid: args[0]})
			if err != nil {
				return err
			}
			return renderTable([]*weftv1.SubnetInfo{resp.Subnet})
		},
	}
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var network, cidr, gateway, name, description, dns string
	cmd := &cobra.Command{
		Use:   "create --network=<uuid> --cidr=<...>",
		Short: "Create a subnet under a parent network",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if network == "" || cidr == "" {
				return fmt.Errorf("--network and --cidr are required")
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CreateSubnet(context.Background(), &weftv1.CreateSubnetRequest{
				NetworkUuid: network,
				Name:        name,
				Description: description,
				Cidr:        cidr,
				Gateway:     gateway,
				DnsServers:  splitCSV(dns),
			})
			if err != nil {
				return err
			}
			tag := "created"
			if !resp.Created {
				tag = "exists"
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", tag, resp.Subnet.Uuid, resp.Subnet.Cidr, resp.Subnet.NetworkUuid)
			return nil
		},
	}
	cmd.Flags().StringVar(&network, "network", "", "Parent network UUID (required)")
	cmd.Flags().StringVar(&cidr, "cidr", "", "Subnet CIDR (immutable, required)")
	cmd.Flags().StringVar(&gateway, "gateway", "", "Default gateway")
	cmd.Flags().StringVar(&name, "name", "", "Subnet display name")
	cmd.Flags().StringVar(&description, "description", "", "Subnet description")
	cmd.Flags().StringVar(&dns, "dns", "", "Comma-separated DNS server list")
	return cmd
}

func updateCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var name, description, gateway, dns string
	var clearDNS bool
	cmd := &cobra.Command{
		Use:   "update <uuid>",
		Short: "Patch a subnet's mutable fields (omitted flags keep current)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.UpdateSubnet(context.Background(), &weftv1.UpdateSubnetRequest{
				Uuid:            args[0],
				Name:            name,
				Description:     description,
				Gateway:         gateway,
				ClearDnsServers: clearDNS,
				DnsServers:      splitCSV(dns),
			})
			if err != nil {
				return err
			}
			fmt.Printf("updated\t%s\t%s\n", resp.Subnet.Uuid, resp.Subnet.Cidr)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Display name")
	cmd.Flags().StringVar(&description, "description", "", "Description")
	cmd.Flags().StringVar(&gateway, "gateway", "", "Default gateway")
	cmd.Flags().StringVar(&dns, "dns", "", "Comma-separated DNS servers (replaces list)")
	cmd.Flags().BoolVar(&clearDNS, "clear-dns", false, "Clear the DNS server list")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <uuid>",
		Short: "Delete a subnet",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.DeleteSubnet(context.Background(), &weftv1.DeleteSubnetRequest{Uuid: args[0]})
			if err != nil {
				return err
			}
			fmt.Println(resp.DeletedUuid)
			return nil
		},
	}
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func renderTable(subnets []*weftv1.SubnetInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tNETWORK\tCIDR\tGATEWAY\tNAME\tDNS\tCREATED")
	for _, s := range subnets {
		created := time.Unix(0, s.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Uuid, s.NetworkUuid, s.Cidr, dashIf(s.Gateway), dashIf(s.Name), dashIf(strings.Join(s.DnsServers, ",")), created)
	}
	return tw.Flush()
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dumpJSON(subnets []*weftv1.SubnetInfo) error {
	type out struct {
		UUID        string   `json:"uuid"`
		Network     string   `json:"network_uuid"`
		Project     string   `json:"project_uuid"`
		Name        string   `json:"name,omitempty"`
		Description string   `json:"description,omitempty"`
		CIDR        string   `json:"cidr"`
		Gateway     string   `json:"gateway,omitempty"`
		DNSServers  []string `json:"dns_servers,omitempty"`
		CreatedAt   string   `json:"created_at"`
	}
	flat := make([]out, len(subnets))
	for i, s := range subnets {
		flat[i] = out{
			UUID:        s.Uuid,
			Network:     s.NetworkUuid,
			Project:     s.ProjectUuid,
			Name:        s.Name,
			Description: s.Description,
			CIDR:        s.Cidr,
			Gateway:     s.Gateway,
			DNSServers:  s.DnsServers,
			CreatedAt:   time.Unix(0, s.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
