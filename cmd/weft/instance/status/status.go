// Package status implements the weft status sub-command.
package status

import (
	"context"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the status cobra command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status <name>",
		Short: "Show status of a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.VMStatus(context.Background(), &weftv1.VMStatusRequest{Name: args[0]})
			if err != nil {
				return err
			}
			shared.RenderTable([]*weftv1.VMInfo{resp.Vm})
			return nil
		},
	}
}
