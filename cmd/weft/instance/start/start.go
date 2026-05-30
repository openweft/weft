// Package start implements the weft start sub-command.
package start

import (
	"context"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the start cobra command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "start <name>",
		Short: "Start a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = c.StartVM(context.Background(), &weftv1.StartVMRequest{Name: args[0]})
			return err
		},
	}
}
