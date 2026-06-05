// Package floatingip implements the `weft floating-ip` subcommand
// group : allocate / release / map / unmap / list floating IPs over
// the FloatingIP RPCs that landed earlier in the proto. Pre-existing
// webui surface ; this package closes the Tier 3 gap surfaced by the
// CLI parity audit.
//
//	weft floating-ip ls [--project=<name|uuid>]              list every FIP
//	weft floating-ip show <uuid>                             single row
//	weft floating-ip allocate --network=<name|uuid> [--project=<...>]
//	weft floating-ip release <uuid>
//	weft floating-ip map <uuid> --target=<name> [--kind=vm|lb]
//	weft floating-ip unmap <uuid>
//
// `show` is local-only : the proto has no GetFloatingIP RPC, so we
// call ListFloatingIPs and filter client-side. Fine for an operator
// CLI ; the webui keeps its own scoped query path.
package floatingip

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

// Command returns the `weft floating-ip` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "floating-ip",
		Short:   "Manage floating IPs (allocate, release, map to VM/LB)",
		Aliases: []string{"fip"},
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		showCmd(socket, sshSocket, sshKey),
		allocateCmd(socket, sshSocket, sshKey),
		releaseCmd(socket, sshSocket, sshKey),
		mapCmd(socket, sshSocket, sshKey),
		unmapCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, format string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List floating IPs",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListFloatingIPs(context.Background(), &weftv1.ListFloatingIPsRequest{Project: project})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpFIPsJSON(resp.FloatingIps)
			}
			return renderFIPsTable(resp.FloatingIps)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Filter to one project (name or UUID)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func showCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "show <uuid>",
		Short: "Show a single floating IP",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListFloatingIPs(context.Background(), &weftv1.ListFloatingIPsRequest{})
			if err != nil {
				return err
			}
			var found *weftv1.FloatingIPInfo
			for _, f := range resp.FloatingIps {
				if f.Uuid == args[0] || f.Address == args[0] {
					found = f
					break
				}
			}
			if found == nil {
				return fmt.Errorf("no floating ip with uuid or address %q", args[0])
			}
			if format == "json" {
				return dumpFIPsJSON([]*weftv1.FloatingIPInfo{found})
			}
			return renderFIPsTable([]*weftv1.FloatingIPInfo{found})
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func allocateCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, network string
	cmd := &cobra.Command{
		Use:   "allocate",
		Short: "Reserve the next-available address from the network's pool",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if network == "" {
				return fmt.Errorf("--network is required")
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.AllocateFloatingIP(context.Background(), &weftv1.AllocateFloatingIPRequest{
				Project: project,
				Network: network,
			})
			if err != nil {
				return err
			}
			f := resp.FloatingIp
			fmt.Printf("allocated\t%s\t%s\t%s\t%s\n", f.Uuid, f.Address, f.Network, f.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Owning project (name or UUID ; empty = caller's default)")
	cmd.Flags().StringVar(&network, "network", "", "Edge network (name or UUID) — required")
	return cmd
}

func releaseCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "release <uuid>",
		Short: "Release a floating IP back to its network's pool",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.ReleaseFloatingIP(context.Background(), &weftv1.ReleaseFloatingIPRequest{Uuid: args[0]}); err != nil {
				return err
			}
			fmt.Println(args[0])
			return nil
		},
	}
}

func mapCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var kind, target string
	cmd := &cobra.Command{
		Use:   "map <uuid>",
		Short: "Attach an allocated FIP to a VM or load-balancer target",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if target == "" {
				return fmt.Errorf("--target is required")
			}
			if kind == "" {
				kind = "vm"
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.MapFloatingIP(context.Background(), &weftv1.MapFloatingIPRequest{
				Uuid:       args[0],
				TargetKind: kind,
				TargetName: target,
			})
			if err != nil {
				return err
			}
			f := resp.FloatingIp
			fmt.Printf("mapped\t%s\t%s\t%s\t%s\n", f.Uuid, f.Address, f.MappedTo, f.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "vm", "Target kind : vm | lb")
	cmd.Flags().StringVar(&target, "target", "", "Target name (VM name or LB name) — required")
	return cmd
}

func unmapCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "unmap <uuid>",
		Short: "Detach a floating IP from its current target",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.UnmapFloatingIP(context.Background(), &weftv1.UnmapFloatingIPRequest{Uuid: args[0]})
			if err != nil {
				return err
			}
			f := resp.FloatingIp
			fmt.Printf("unmapped\t%s\t%s\t%s\n", f.Uuid, f.Address, f.Status)
			return nil
		},
	}
}

func renderFIPsTable(fips []*weftv1.FloatingIPInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tADDRESS\tNETWORK\tPROJECT\tMAPPED_TO\tSTATUS\tALLOCATED")
	for _, f := range fips {
		allocated := time.Unix(0, f.AllocatedAtUnixNs).UTC().Format(time.RFC3339)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			f.Uuid, f.Address, dashIf(f.Network), dashIf(f.ProjectUuid), dashIf(f.MappedTo), f.Status, allocated)
	}
	return tw.Flush()
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dumpFIPsJSON(fips []*weftv1.FloatingIPInfo) error {
	type out struct {
		UUID        string `json:"uuid"`
		Address     string `json:"address"`
		Network     string `json:"network,omitempty"`
		ProjectUUID string `json:"project_uuid,omitempty"`
		MappedTo    string `json:"mapped_to,omitempty"`
		Status      string `json:"status"`
		AllocatedAt string `json:"allocated_at"`
	}
	flat := make([]out, len(fips))
	for i, f := range fips {
		flat[i] = out{
			UUID:        f.Uuid,
			Address:     f.Address,
			Network:     f.Network,
			ProjectUUID: f.ProjectUuid,
			MappedTo:    f.MappedTo,
			Status:      f.Status,
			AllocatedAt: time.Unix(0, f.AllocatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
