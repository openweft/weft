// Package az implements the `weft az` subcommand group : CRUD over
// the AvailabilityZone registry (top tier of the inventory hierarchy
// AZ → Rack → Host). Promoted from webui-local persistence to a
// first-class control-plane concern in weft-proto v0.7.0 so the
// CLI + webui + every other client reach the same source of truth.
//
//	weft az ls                                      list every AZ
//	weft az show <code|uuid>                        single row + counts
//	weft az create <code> [--name --region --status]
//	weft az update <code|uuid> [--name --region --status]
//	weft az rm <code|uuid>                          refuses if racks/hosts attached
package az

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

// Command returns the `weft az` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "az",
		Short: "Manage availability zones (top tier of the inventory hierarchy)",
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
	var format string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List AZs",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListAZs(context.Background(), &weftv1.ListAZsRequest{})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpAZsJSON(resp.Azs)
			}
			return renderAZsTable(resp.Azs)
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func showCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "show <code|uuid>",
		Short: "Show a single AZ + its rack/host counts",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			req := &weftv1.GetAZRequest{}
			if LooksLikeUUID(args[0]) {
				req.Uuid = args[0]
			} else {
				req.Code = args[0]
			}
			resp, err := c.GetAZ(context.Background(), req)
			if err != nil {
				return err
			}
			return renderAZsTable([]*weftv1.AZInfo{resp.Az})
		},
	}
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var name, region, statusValue string
	cmd := &cobra.Command{
		Use:   "create <code>",
		Short: "Register a new AZ (admin-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CreateAZ(context.Background(), &weftv1.CreateAZRequest{
				Code:   args[0],
				Name:   name,
				Region: region,
				Status: statusValue,
			})
			if err != nil {
				return err
			}
			tag := "created"
			if !resp.Created {
				tag = "exists"
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", tag, resp.Az.Uuid, resp.Az.Code, resp.Az.Region)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Human-friendly display name")
	cmd.Flags().StringVar(&region, "region", "", "Region tag (e.g. eu-west-1)")
	cmd.Flags().StringVar(&statusValue, "status", "", "Initial status (default: active)")
	return cmd
}

func updateCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var name, region, statusValue string
	cmd := &cobra.Command{
		Use:   "update <code|uuid>",
		Short: "Patch an AZ's mutable fields (admin-only ; omitted flags keep current)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			uuid, err := ResolveArg(c, args[0])
			if err != nil {
				return err
			}
			resp, err := c.UpdateAZ(context.Background(), &weftv1.UpdateAZRequest{
				Uuid:   uuid,
				Name:   name,
				Region: region,
				Status: statusValue,
			})
			if err != nil {
				return err
			}
			fmt.Printf("updated\t%s\t%s\t%s\t%s\n", resp.Az.Uuid, resp.Az.Code, resp.Az.Region, resp.Az.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Display name (empty = keep current)")
	cmd.Flags().StringVar(&region, "region", "", "Region tag (empty = keep current)")
	cmd.Flags().StringVar(&statusValue, "status", "", "Status (empty = keep current)")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <code|uuid>",
		Short: "Delete an empty AZ (refuses while racks or hosts still attach)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			uuid, err := ResolveArg(c, args[0])
			if err != nil {
				return err
			}
			resp, err := c.DeleteAZ(context.Background(), &weftv1.DeleteAZRequest{Uuid: uuid})
			if err != nil {
				// Surface the blocking counts the server returned so
				// the operator sees exactly what needs draining.
				if resp != nil && (resp.BlockedByRacks > 0 || resp.BlockedByHosts > 0) {
					return fmt.Errorf("delete refused : %d rack(s), %d host(s) still attached", resp.BlockedByRacks, resp.BlockedByHosts)
				}
				return err
			}
			fmt.Println(resp.DeletedUuid)
			return nil
		},
	}
}

// ResolveArg returns a UUID from either a literal UUID or an AZ
// code. The `rack` package reuses this so an operator who passes
// `weft rack create --az-code=DC-A` doesn't have to do the
// resolution themselves.
func ResolveArg(c weftv1.WeftAgentClient, arg string) (string, error) {
	if LooksLikeUUID(arg) {
		return arg, nil
	}
	resp, err := c.GetAZ(context.Background(), &weftv1.GetAZRequest{Code: arg})
	if err != nil {
		return "", fmt.Errorf("no az with code %q : %w", arg, err)
	}
	return resp.Az.Uuid, nil
}

// LooksLikeUUID is a cheap RFC-4122 v4 shape check. Exported so the
// rack package can share the rule (`weft rack create AZ-arg ...`
// accepts either form).
func LooksLikeUUID(s string) bool {
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

func renderAZsTable(azs []*weftv1.AZInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tCODE\tNAME\tREGION\tSTATUS\tRACKS\tHOSTS\tCREATED")
	for _, a := range azs {
		created := time.Unix(0, a.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			a.Uuid, a.Code, dashIf(a.Name), dashIf(a.Region), a.Status, a.Racks, a.Hosts, created)
	}
	return tw.Flush()
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dumpAZsJSON(azs []*weftv1.AZInfo) error {
	type out struct {
		UUID      string `json:"uuid"`
		Code      string `json:"code"`
		Name      string `json:"name,omitempty"`
		Region    string `json:"region,omitempty"`
		Status    string `json:"status"`
		Racks     int32  `json:"racks"`
		Hosts     int32  `json:"hosts"`
		CreatedAt string `json:"created_at"`
	}
	flat := make([]out, len(azs))
	for i, a := range azs {
		flat[i] = out{
			UUID:      a.Uuid,
			Code:      a.Code,
			Name:      a.Name,
			Region:    a.Region,
			Status:    a.Status,
			Racks:     a.Racks,
			Hosts:     a.Hosts,
			CreatedAt: time.Unix(0, a.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
