// Package restart implements `weft instance restart` — the atomic
// Stop-then-Start RPC the agent serves as one transaction (same host,
// no half-state if Start fails).
package restart

import (
	"context"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "restart <name>",
		Short: "Restart a VM atomically (single RPC, no client-side stop+start chain)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = c.RestartVM(context.Background(), &weftv1.RestartVMRequest{
				Name:    args[0],
				Project: project,
			})
			return err
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project the VM belongs to (defaults to the agent's default project)")
	return cmd
}
