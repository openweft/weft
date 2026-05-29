// Package instance implements the vzc instance sub-command group.
package instance

import (
	"context"

	"github.com/openweft/weft/cmd/weft/instance/logs"
	"github.com/openweft/weft/cmd/weft/instance/property"
	"github.com/openweft/weft/cmd/weft/instance/ps"
	"github.com/openweft/weft/cmd/weft/instance/registermicrovm"
	"github.com/openweft/weft/cmd/weft/instance/sshkey"
	"github.com/openweft/weft/cmd/weft/instance/start"
	"github.com/openweft/weft/cmd/weft/instance/status"
	"github.com/openweft/weft/cmd/weft/instance/stop"
	"github.com/openweft/weft/cmd/weft/instance/timings"
	"github.com/openweft/weft/cmd/weft/instance/uefi"
	"github.com/openweft/weft/cmd/weft/shared"
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the instance cobra command with its sub-commands.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "instance",
		Short: "Manage VM instances",
	}
	cmd.AddCommand(
		listCmd(socket, sshSocket, sshKey),
		start.Command(socket, sshSocket, sshKey),
		stop.Command(socket, sshSocket, sshKey),
		status.Command(socket, sshSocket, sshKey),
		registermicrovm.Command(socket, sshSocket, sshKey),
		timings.Command(socket, sshSocket, sshKey),
		logs.Command(socket, sshSocket, sshKey),
		ps.Command(),
		property.Command(socket, sshSocket, sshKey),
		uefi.Command(socket, sshSocket, sshKey),
		sshkey.Command(socket, sshSocket, sshKey),
	)
	return cmd
}

func listCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List VMs managed by vzd",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListVMs(context.Background(), &vzdv1.ListVMsRequest{})
			if err != nil {
				return err
			}
			if format == "json" {
				return shared.PrintJSON(resp.Vms)
			}
			shared.RenderTable(resp.Vms)
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}
