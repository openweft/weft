// Package setproperties implements `weft instance set-properties` —
// V0.1.8 SchedulingRule property-based selector authoring + reserved-
// key system gates (deployment.type=ci|ha, …).
//
//	weft instance set-properties <name> deployment.type=ha role=loom
//	weft instance set-properties <name> --clear        # drop every property
//	weft instance set-properties <name> --project p2 deployment.type=ci
//
// The legacy `set-labels` cobra alias is kept for one deprecation cycle
// — the alias prints a stderr deprecation notice and otherwise behaves
// identically.
package setproperties

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft instance set-properties` cobra command
// (aliased `set-labels` for one deprecation cycle).
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		project string
		clear   bool
	)
	cmd := &cobra.Command{
		Use:     "set-properties <name> [k=v ...]",
		Aliases: []string{"set-labels"},
		Short:   "Set the property map on a VM (V0.1.8)",
		Long: `Set the property map on a VM atomically. Existing properties are REPLACED
(not merged) by the new set ; pass --clear to drop every property.

Properties drive SchedulingRule property-based selectors (e.g.
'selector=role=loom') and the reserved-key system gates :

  deployment.type=ha          respawn-eligible long-lived service
  deployment.type=ci          disposable workload, excluded from
                              etcd backup + skipped by respawn

Examples :

  weft instance set-properties web-1 role=web tier=prod
  weft instance set-properties ci-runner-7 deployment.type=ci
  weft instance set-properties web-1 --clear`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.CalledAs() == "set-labels" {
				fmt.Fprintln(os.Stderr, "weft instance set-labels: deprecated, use 'set-properties'")
			}
			name := args[0]
			pairs := args[1:]
			var properties map[string]string
			if !clear {
				properties = map[string]string{}
				for _, p := range pairs {
					k, v, ok := strings.Cut(p, "=")
					if !ok || k == "" {
						return fmt.Errorf("bad property pair %q (want k=v)", p)
					}
					properties[k] = v
				}
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.SetVMProperties(context.Background(), &weftv1.SetVMPropertiesRequest{
				Project:    project,
				Name:       name,
				Properties: properties,
			})
			if err != nil {
				return err
			}
			fmt.Printf("set\t%s\tproperties=%d\n", resp.Vm.Name, len(resp.Vm.Properties))
			for k, v := range resp.Vm.Properties {
				fmt.Printf("  %s = %s\n", k, v)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project (UUID or name) ; empty = caller's default")
	cmd.Flags().BoolVar(&clear, "clear", false, "Drop every property instead of setting new ones")
	return cmd
}
