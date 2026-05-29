// Package wait implements the vzc wait sub-command.
package wait

import (
	"context"
	"fmt"

	"github.com/openweft/weft/cmd/weft/shared"
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the wait cobra command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	var timeout int
	cmd := &cobra.Command{
		Use:   "wait <name>",
		Short: "Wait until a VM has an IP address (via vzd)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.WaitVM(context.Background(), &vzdv1.WaitVMRequest{
				Name:           args[0],
				TimeoutSeconds: int32(timeout),
			})
			if err != nil {
				return err
			}
			fmt.Println(resp.Ip)
			return nil
		},
	}
	cmd.Flags().IntVar(&timeout, "timeout", 120, "Timeout in seconds")
	return cmd
}
