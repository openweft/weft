// Package setlabels implements `weft instance set-labels` —
// V0.1.8 SchedulingRule label-based selector authoring + reserved-
// key system gates (deployment.type=ci|ha, …).
//
//	weft instance set-labels <name> deployment.type=ha role=loom
//	weft instance set-labels <name> --clear        # drop every label
//	weft instance set-labels <name> --project p2 deployment.type=ci
package setlabels

import (
	"context"
	"fmt"
	"strings"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft instance set-labels` cobra command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		project string
		clear   bool
	)
	cmd := &cobra.Command{
		Use:   "set-labels <name> [k=v ...]",
		Short: "Set the label map on a VM (V0.1.8)",
		Long: `Set the label map on a VM atomically. Existing labels are REPLACED
(not merged) by the new set ; pass --clear to drop every label.

Labels drive SchedulingRule label-based selectors (e.g.
'selector=role=loom') and the reserved-key system gates :

  deployment.type=ha          respawn-eligible long-lived service
  deployment.type=ci          disposable workload, excluded from
                              etcd backup + skipped by respawn

Examples :

  weft instance set-labels web-1 role=web tier=prod
  weft instance set-labels ci-runner-7 deployment.type=ci
  weft instance set-labels web-1 --clear`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			pairs := args[1:]
			var labels map[string]string
			if !clear {
				labels = map[string]string{}
				for _, p := range pairs {
					k, v, ok := strings.Cut(p, "=")
					if !ok || k == "" {
						return fmt.Errorf("bad label pair %q (want k=v)", p)
					}
					labels[k] = v
				}
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.SetVMLabels(context.Background(), &weftv1.SetVMLabelsRequest{
				Project: project,
				Name:    name,
				Labels:  labels,
			})
			if err != nil {
				return err
			}
			fmt.Printf("set\t%s\tlabels=%d\n", resp.Vm.Name, len(resp.Vm.Labels))
			for k, v := range resp.Vm.Labels {
				fmt.Printf("  %s = %s\n", k, v)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project (UUID or name) ; empty = caller's default")
	cmd.Flags().BoolVar(&clear, "clear", false, "Drop every label instead of setting new ones")
	return cmd
}
