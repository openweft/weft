// Package rack implements the `weft rack` subcommand group : CRUD
// over the Rack registry (second tier of the inventory hierarchy
// AZ → Rack → Host). Promoted to a control-plane RPC in
// weft-proto v0.7.0.
//
//	weft rack ls [--az=<code|uuid>]                list every rack
//	weft rack show <uuid>                          single row + host count
//	weft rack create <code> --az=<code|uuid> [--name --status --height-u]
//	weft rack update <uuid> [--name --status --height-u]
//	weft rack rm <uuid>                            refuses if hosts attached
package rack

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/openweft/weft/cmd/weft/az"
	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft rack` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rack",
		Short: "Manage racks (second tier of the inventory hierarchy)",
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
	var azArg, format string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List racks (--az filters to one AZ)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			azUUID := ""
			if azArg != "" {
				azUUID, err = az.ResolveArg(c, azArg)
				if err != nil {
					return err
				}
			}
			resp, err := c.ListRacks(context.Background(), &weftv1.ListRacksRequest{AzUuid: azUUID})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpRacksJSON(resp.Racks)
			}
			return renderRacksTable(resp.Racks)
		},
	}
	cmd.Flags().StringVar(&azArg, "az", "", "Filter to one AZ (code or UUID)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func showCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "show <uuid>",
		Short: "Show a single rack + its host count",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.GetRack(context.Background(), &weftv1.GetRackRequest{Uuid: args[0]})
			if err != nil {
				return err
			}
			return renderRacksTable([]*weftv1.RackInfo{resp.Rack})
		},
	}
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var azArg, name, statusValue string
	var heightU int32
	cmd := &cobra.Command{
		Use:   "create <code>",
		Short: "Register a new rack under --az (admin-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if azArg == "" {
				return fmt.Errorf("--az is required")
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			azUUID, err := az.ResolveArg(c, azArg)
			if err != nil {
				return err
			}
			resp, err := c.CreateRack(context.Background(), &weftv1.CreateRackRequest{
				AzUuid:  azUUID,
				Code:    args[0],
				Name:    name,
				Status:  statusValue,
				HeightU: heightU,
			})
			if err != nil {
				return err
			}
			tag := "created"
			if !resp.Created {
				tag = "exists"
			}
			fmt.Printf("%s\t%s\t%s\t%s\tU=%d\n", tag, resp.Rack.Uuid, resp.Rack.AzUuid, resp.Rack.Code, resp.Rack.HeightU)
			return nil
		},
	}
	cmd.Flags().StringVar(&azArg, "az", "", "Parent AZ (code or UUID) — required")
	cmd.Flags().StringVar(&name, "name", "", "Human-friendly display name")
	cmd.Flags().StringVar(&statusValue, "status", "", "Initial status (default: active)")
	cmd.Flags().Int32Var(&heightU, "height-u", 0, "Rack total U capacity (0 = unspecified)")
	return cmd
}

func updateCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var name, statusValue string
	var heightU int32 = -1
	cmd := &cobra.Command{
		Use:   "update <uuid>",
		Short: "Patch a rack's mutable fields (admin-only ; omitted flags keep current)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			// --height-u defaults to -1 (sentinel "keep current"). If
			// the operator passed an explicit value (including 0) we
			// forward it ; otherwise we re-stamp -1 so the server's
			// partial-PATCH logic short-circuits.
			h := int32(-1)
			if cobraCmd.Flags().Changed("height-u") {
				h = heightU
			}
			resp, err := c.UpdateRack(context.Background(), &weftv1.UpdateRackRequest{
				Uuid:    args[0],
				Name:    name,
				Status:  statusValue,
				HeightU: h,
			})
			if err != nil {
				return err
			}
			fmt.Printf("updated\t%s\t%s\t%s\tU=%d\n", resp.Rack.Uuid, resp.Rack.Code, resp.Rack.Status, resp.Rack.HeightU)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Display name (empty = keep current)")
	cmd.Flags().StringVar(&statusValue, "status", "", "Status (empty = keep current)")
	cmd.Flags().Int32Var(&heightU, "height-u", -1, "Total U capacity (unset = keep current ; 0 = explicit clear)")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <uuid>",
		Short: "Delete an empty rack (refuses while hosts still attach)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.DeleteRack(context.Background(), &weftv1.DeleteRackRequest{Uuid: args[0]})
			if err != nil {
				if resp != nil && resp.BlockedByHosts > 0 {
					return fmt.Errorf("delete refused : %d host(s) still attached", resp.BlockedByHosts)
				}
				return err
			}
			fmt.Println(resp.DeletedUuid)
			return nil
		},
	}
}

func renderRacksTable(racks []*weftv1.RackInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tAZ_UUID\tCODE\tNAME\tSTATUS\tHEIGHT_U\tHOSTS\tCREATED")
	for _, r := range racks {
		created := time.Unix(0, r.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			r.Uuid, r.AzUuid, r.Code, dashIf(r.Name), r.Status, r.HeightU, r.Hosts, created)
	}
	return tw.Flush()
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dumpRacksJSON(racks []*weftv1.RackInfo) error {
	type out struct {
		UUID      string `json:"uuid"`
		AZUUID    string `json:"az_uuid"`
		Code      string `json:"code"`
		Name      string `json:"name,omitempty"`
		Status    string `json:"status"`
		HeightU   int32  `json:"height_u"`
		Hosts     int32  `json:"hosts"`
		CreatedAt string `json:"created_at"`
	}
	flat := make([]out, len(racks))
	for i, r := range racks {
		flat[i] = out{
			UUID:      r.Uuid,
			AZUUID:    r.AzUuid,
			Code:      r.Code,
			Name:      r.Name,
			Status:    r.Status,
			HeightU:   r.HeightU,
			Hosts:     r.Hosts,
			CreatedAt: time.Unix(0, r.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
