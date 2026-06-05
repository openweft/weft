// Package dnsrecord implements the `weft dns-record` subcommand
// group : CRUD over the DNSRecord registry (zone children). Closes
// the Tier-3 CLI parity gap via proto v0.8.0.
//
//	weft dns-record ls --zone=<uuid>
//	weft dns-record create --zone=<uuid> --name=<...> --type=<A|AAAA|CNAME|MX|TXT|SRV> --value=<...> [--ttl --priority]
//	weft dns-record update <uuid> [--value --ttl --priority]
//	weft dns-record rm <uuid>
package dnsrecord

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

// Command returns the `weft dns-record` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dns-record",
		Short: "Manage DNS records (zone children)",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		createCmd(socket, sshSocket, sshKey),
		updateCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var zone, format string
	cmd := &cobra.Command{
		Use:   "ls --zone=<uuid>",
		Short: "List DNS records in a zone",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if zone == "" {
				return fmt.Errorf("--zone is required")
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListDNSRecords(context.Background(), &weftv1.ListDNSRecordsRequest{ZoneUuid: zone})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Records)
			}
			return renderTable(resp.Records)
		},
	}
	cmd.Flags().StringVar(&zone, "zone", "", "Parent zone UUID (required)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var zone, name, recordType, value string
	var ttl, priority int32
	cmd := &cobra.Command{
		Use:   "create --zone=<uuid> --name=<...> --type=<...> --value=<...>",
		Short: "Create a DNS record",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if zone == "" || name == "" || recordType == "" || value == "" {
				return fmt.Errorf("--zone, --name, --type, --value are required")
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CreateDNSRecord(context.Background(), &weftv1.CreateDNSRecordRequest{
				ZoneUuid: zone,
				Name:     name,
				Type:     recordType,
				Value:    value,
				Ttl:      ttl,
				Priority: priority,
			})
			if err != nil {
				return err
			}
			tag := "created"
			if !resp.Created {
				tag = "exists"
			}
			fmt.Printf("%s\t%s\t%s\t%s\t%s\n", tag, resp.Record.Uuid, resp.Record.Name, resp.Record.Type, resp.Record.Value)
			return nil
		},
	}
	cmd.Flags().StringVar(&zone, "zone", "", "Parent zone UUID")
	cmd.Flags().StringVar(&name, "name", "", "Record name (e.g. www, @, sub.acme.example.com)")
	cmd.Flags().StringVar(&recordType, "type", "", "A | AAAA | CNAME | MX | TXT | SRV")
	cmd.Flags().StringVar(&value, "value", "", "Record value (IP, FQDN, text, ...)")
	cmd.Flags().Int32Var(&ttl, "ttl", 0, "Per-record TTL (0 = inherit zone default)")
	cmd.Flags().Int32Var(&priority, "priority", 0, "Priority (MX/SRV only)")
	return cmd
}

func updateCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var value string
	var ttl, priority int32
	cmd := &cobra.Command{
		Use:   "update <uuid>",
		Short: "Patch a DNS record's mutable fields",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.UpdateDNSRecord(context.Background(), &weftv1.UpdateDNSRecordRequest{
				Uuid:     args[0],
				Value:    value,
				Ttl:      ttl,
				Priority: priority,
			})
			if err != nil {
				return err
			}
			fmt.Printf("updated\t%s\t%s\n", resp.Record.Uuid, resp.Record.Value)
			return nil
		},
	}
	cmd.Flags().StringVar(&value, "value", "", "Record value")
	cmd.Flags().Int32Var(&ttl, "ttl", -1, "Per-record TTL (-1 = keep current)")
	cmd.Flags().Int32Var(&priority, "priority", -1, "Priority (-1 = keep current)")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <uuid>",
		Short: "Delete a DNS record",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.DeleteDNSRecord(context.Background(), &weftv1.DeleteDNSRecordRequest{Uuid: args[0]})
			if err != nil {
				return err
			}
			fmt.Println(resp.DeletedUuid)
			return nil
		},
	}
}

func renderTable(records []*weftv1.DNSRecordInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tZONE\tNAME\tTYPE\tVALUE\tTTL\tPRIORITY\tCREATED")
	for _, r := range records {
		created := time.Unix(0, r.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			r.Uuid, r.ZoneUuid, r.Name, r.Type, r.Value, r.Ttl, r.Priority, created)
	}
	return tw.Flush()
}

func dumpJSON(records []*weftv1.DNSRecordInfo) error {
	type out struct {
		UUID      string `json:"uuid"`
		Zone      string `json:"zone_uuid"`
		Name      string `json:"name"`
		Type      string `json:"type"`
		Value     string `json:"value"`
		TTL       int32  `json:"ttl,omitempty"`
		Priority  int32  `json:"priority,omitempty"`
		CreatedAt string `json:"created_at"`
	}
	flat := make([]out, len(records))
	for i, r := range records {
		flat[i] = out{
			UUID:      r.Uuid,
			Zone:      r.ZoneUuid,
			Name:      r.Name,
			Type:      r.Type,
			Value:     r.Value,
			TTL:       r.Ttl,
			Priority:  r.Priority,
			CreatedAt: time.Unix(0, r.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
