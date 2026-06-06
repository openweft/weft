// Package dnszone implements the `weft dns-zone` subcommand group :
// CRUD over the DNSZone registry (per-project authoritative apex).
// Closes the Tier-3 CLI parity gap via proto v0.8.0.
//
//	weft dns-zone ls [--project=<...>]
//	weft dns-zone show <uuid|name>
//	weft dns-zone create --project=<...> --name=<fqdn> [--soa-email --ttl]
//	weft dns-zone update <uuid> [--soa-email --ttl]
//	weft dns-zone rm <uuid>
package dnszone

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft dns-zone` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dns-zone",
		Short: "Manage DNS zones (per-project authoritative apex)",
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
	var project, format string
	cmd := &cobra.Command{
		Use:   "ls",
		Aliases: []string{"list"},
		Short: "List DNS zones",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListDNSZones(context.Background(), &weftv1.ListDNSZonesRequest{Project: project})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Zones)
			}
			return renderTable(resp.Zones)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Filter to one project (name or UUID)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func showCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "show <uuid|name>",
		Short: "Show a single DNS zone",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			req := &weftv1.GetDNSZoneRequest{}
			if looksLikeUUID(args[0]) {
				req.Uuid = args[0]
			} else {
				req.Name = args[0]
			}
			resp, err := c.GetDNSZone(context.Background(), req)
			if err != nil {
				return err
			}
			return renderTable([]*weftv1.DNSZoneInfo{resp.Zone})
		},
	}
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, name, soaEmail string
	var ttl int32
	cmd := &cobra.Command{
		Use:   "create --project=<...> --name=<fqdn>",
		Short: "Create a DNS zone",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CreateDNSZone(context.Background(), &weftv1.CreateDNSZoneRequest{
				Project:  project,
				Name:     name,
				SoaEmail: soaEmail,
				Ttl:      ttl,
			})
			if err != nil {
				return err
			}
			tag := "created"
			if !resp.Created {
				tag = "exists"
			}
			fmt.Printf("%s\t%s\t%s\t%d\n", tag, resp.Zone.Uuid, resp.Zone.Name, resp.Zone.Ttl)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project (name or UUID)")
	cmd.Flags().StringVar(&name, "name", "", "Zone FQDN (e.g. acme.example.com)")
	cmd.Flags().StringVar(&soaEmail, "soa-email", "", "SOA rname")
	cmd.Flags().Int32Var(&ttl, "ttl", 0, "Default TTL (0 → 3600)")
	return cmd
}

func updateCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var soaEmail string
	var ttl int32
	cmd := &cobra.Command{
		Use:   "update <uuid>",
		Short: "Patch a DNS zone's mutable fields",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.UpdateDNSZone(context.Background(), &weftv1.UpdateDNSZoneRequest{
				Uuid:     args[0],
				SoaEmail: soaEmail,
				Ttl:      ttl,
			})
			if err != nil {
				return err
			}
			fmt.Printf("updated\t%s\t%s\n", resp.Zone.Uuid, resp.Zone.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&soaEmail, "soa-email", "", "SOA rname")
	cmd.Flags().Int32Var(&ttl, "ttl", -1, "Default TTL (-1 = keep current)")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <uuid>",
		Short: "Delete an empty DNS zone (refuses while records still attach)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.DeleteDNSZone(context.Background(), &weftv1.DeleteDNSZoneRequest{Uuid: args[0]})
			if err != nil {
				if resp != nil && resp.BlockedByRecords > 0 {
					return fmt.Errorf("delete refused : %d record(s) still in zone", resp.BlockedByRecords)
				}
				return err
			}
			fmt.Println(resp.DeletedUuid)
			return nil
		},
	}
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

func renderTable(zones []*weftv1.DNSZoneInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tPROJECT\tNAME\tSOA_EMAIL\tTTL\tRECORDS\tCREATED")
	for _, z := range zones {
		created := time.Unix(0, z.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			z.Uuid, dashIf(z.ProjectUuid), z.Name, dashIf(z.SoaEmail), z.Ttl, z.Records, created)
	}
	return tw.Flush()
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dumpJSON(zones []*weftv1.DNSZoneInfo) error {
	type out struct {
		UUID      string `json:"uuid"`
		Project   string `json:"project_uuid,omitempty"`
		Name      string `json:"name"`
		SOAEmail  string `json:"soa_email,omitempty"`
		TTL       int32  `json:"ttl"`
		Records   int32  `json:"records"`
		CreatedAt string `json:"created_at"`
	}
	flat := make([]out, len(zones))
	for i, z := range zones {
		flat[i] = out{
			UUID:      z.Uuid,
			Project:   z.ProjectUuid,
			Name:      z.Name,
			SOAEmail:  z.SoaEmail,
			TTL:       z.Ttl,
			Records:   z.Records,
			CreatedAt: time.Unix(0, z.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}

