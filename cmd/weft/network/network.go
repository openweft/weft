// Package network implements the `weft network` subcommand group:
// CRUD over weft's UUID-keyed network registry.
//
//	weft network ls [--project NAME-OR-UUID] [--format json]
//	weft network create --project P --name N --cidr 10.0.0.0/24 \
//	                   [--gateway 10.0.0.1] [--dns 1.1.1.1,8.8.8.8] \
//	                   [--type nat|bridged|isolated]
//	weft network rename <UUID> <new-name>
//	weft network set-dns <UUID> <server1,server2,...>
//	weft network rm <UUID>
package network

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

func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Manage virtual networks (UUID-keyed, project-scoped)",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		createCmd(socket, sshSocket, sshKey),
		renameCmd(socket, sshSocket, sshKey),
		setDNSCmd(socket, sshSocket, sshKey),
		setSGsCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, format string
	cmd := &cobra.Command{
		Use:   "ls",
		Aliases: []string{"list"},
		Short: "List networks (optionally scoped to one project)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListNetworks(context.Background(), &weftv1.ListNetworksRequest{Project: project})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Networks)
			}
			return renderTable(resp.Networks)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Limit to one project (display name or UUID)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, name, cidr, gateway, netType string
	var dnsServers []string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new network",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CreateNetwork(context.Background(), &weftv1.CreateNetworkRequest{
				Project:    project,
				Name:       name,
				Cidr:       cidr,
				Gateway:    gateway,
				DnsServers: dnsServers,
				Type:       netType,
			})
			if err != nil {
				return err
			}
			fmt.Printf("created\t%s\t%s\t%s\t%s\n",
				resp.Network.Uuid, resp.Network.Name, resp.Network.Cidr, resp.Network.Type)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project (display name or UUID)")
	cmd.Flags().StringVar(&name, "name", "", "Network name (unique within the project)")
	cmd.Flags().StringVar(&cidr, "cidr", "", "Network CIDR (e.g. 10.0.0.0/24)")
	cmd.Flags().StringVar(&gateway, "gateway", "", "Gateway IP (optional; derived from CIDR by default)")
	cmd.Flags().StringSliceVar(&dnsServers, "dns", nil, "DNS servers (repeatable)")
	cmd.Flags().StringVar(&netType, "type", "", `Network type: "nat" (default), "bridged" or "isolated"`)
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("cidr")
	return cmd
}

func renameCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <UUID> <new-name>",
		Short: "Rename a network (UUID unchanged)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.RenameNetwork(context.Background(), &weftv1.RenameNetworkRequest{
				Uuid: args[0], NewName: args[1],
			})
			if err != nil {
				return err
			}
			fmt.Printf("renamed\t%s\t%s\n", resp.Network.Uuid, resp.Network.Name)
			return nil
		},
	}
}

func setDNSCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "set-dns <UUID> <server1[,server2,...]>",
		Short: "Replace the network's DNS-server list",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			servers := splitNonEmpty(args[1], ",")
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.SetNetworkDNS(context.Background(), &weftv1.SetNetworkDNSRequest{
				Uuid: args[0], DnsServers: servers,
			})
			if err != nil {
				return err
			}
			fmt.Printf("dns\t%s\t%s\n", resp.Network.Uuid, strings.Join(resp.Network.DnsServers, ","))
			return nil
		},
	}
}

// setSGsCmd replaces the network's default security-group list
// atomically. Pass an empty list (just the comma or "" in shell)
// to clear it. The server validates every UUID is an SG in the
// same project as the network — see networks.go in weft.
func setSGsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "set-sgs <UUID> <sg-uuid[,sg-uuid,...]>",
		Short: "Replace the network's default security-group list (atomic)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			sgs := splitNonEmpty(args[1], ",")
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.SetNetworkDefaultSecurityGroups(context.Background(), &weftv1.SetNetworkDefaultSecurityGroupsRequest{
				Uuid:               args[0],
				SecurityGroupUuids: sgs,
			})
			if err != nil {
				return err
			}
			displayed := strings.Join(resp.Network.DefaultSecurityGroupUuids, ",")
			if displayed == "" {
				displayed = "(none)"
			}
			fmt.Printf("sgs\t%s\t%s\n", resp.Network.Uuid, displayed)
			return nil
		},
	}
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <UUID>",
		Short: "Delete a network (no cascade onto attached VMs)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.DeleteNetwork(context.Background(), &weftv1.DeleteNetworkRequest{Uuid: args[0]}); err != nil {
				return err
			}
			fmt.Println(args[0])
			return nil
		},
	}
}

func splitNonEmpty(s, sep string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, sep)
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func renderTable(nets []*weftv1.NetworkInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tPROJECT_UUID\tNAME\tCIDR\tGATEWAY\tTYPE\tDNS\tDEFAULT_SGS\tCREATED")
	for _, n := range nets {
		dns := strings.Join(n.DnsServers, ",")
		if dns == "" {
			dns = "-"
		}
		gw := n.Gateway
		if gw == "" {
			gw = "-"
		}
		sgs := strings.Join(n.DefaultSecurityGroupUuids, ",")
		if sgs == "" {
			sgs = "-"
		}
		created := time.Unix(0, n.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n.Uuid, n.ProjectUuid, n.Name, n.Cidr, gw, n.Type, dns, sgs, created)
	}
	return tw.Flush()
}

func dumpJSON(nets []*weftv1.NetworkInfo) error {
	type out struct {
		UUID                string   `json:"uuid"`
		ProjectUUID         string   `json:"project_uuid"`
		Name                string   `json:"name"`
		CIDR                string   `json:"cidr"`
		Gateway             string   `json:"gateway,omitempty"`
		DNSServers          []string `json:"dns_servers,omitempty"`
		Type                string   `json:"type"`
		DefaultSecurityGroups []string `json:"default_security_group_uuids,omitempty"`
		CreatedAt           string   `json:"created_at"`
	}
	flat := make([]out, len(nets))
	for i, n := range nets {
		flat[i] = out{
			UUID:                n.Uuid,
			ProjectUUID:         n.ProjectUuid,
			Name:                n.Name,
			CIDR:                n.Cidr,
			Gateway:             n.Gateway,
			DNSServers:          n.DnsServers,
			Type:                n.Type,
			DefaultSecurityGroups: n.DefaultSecurityGroupUuids,
			CreatedAt:           time.Unix(0, n.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
