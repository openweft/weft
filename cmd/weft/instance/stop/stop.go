// Package stop implements the vzc stop sub-command.
package stop

import (
	"context"

	"github.com/openweft/weft/cmd/weft/shared"
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the stop cobra command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = c.StopVM(context.Background(), &vzdv1.StopVMRequest{Name: args[0]})
			return err
		},
	}
}
