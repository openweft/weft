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
	var host string
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
			ctx := context.Background()
			// Auto-resolve host_uuid from the registry when --host
			// wasn't supplied so the operator doesn't have to know
			// the placement to restart a VM. Falls back to the
			// local-only path when ListVMs doesn't surface the row
			// (legacy VMs without an inventory record).
			if host == "" {
				if resp, lerr := c.ListVMs(ctx, &weftv1.ListVMsRequest{}); lerr == nil {
					for _, v := range resp.Vms {
						if v.Name == args[0] && (project == "" || v.Project == project || v.ProjectUuid == project) {
							host = v.HostUuid
							break
						}
					}
				}
			}
			_, err = c.RestartVM(ctx, &weftv1.RestartVMRequest{
				Name:     args[0],
				Project:  project,
				HostUuid: host,
			})
			return err
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project the VM belongs to (defaults to the agent's default project)")
	cmd.Flags().StringVar(&host, "host", "", "Owning host UUID (defaults to ListVMs auto-resolve)")
	return cmd
}
