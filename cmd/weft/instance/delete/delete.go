// Package delete implements `weft instance delete` — the production-
// survival counterpart of weft microvm rm. `weft microvm rm` only
// matches the "weft-microvm-<refsafe>" naming the CLI-spawned
// containers carry ; everything else (plugin-installed VMs,
// infra-deploy VMs, plain CreateVM rows) needs a direct DeleteVM
// call that addresses the row by its registry name.
//
//	weft instance delete redis-ha-8aa215ad-redis-0 --project infra
//	weft instance delete <name>                       # default project
//	weft instance delete <name> --host <uuid>         # explicit host
//	weft instance delete <name> --force               # skip the prompt
//
// host_uuid auto-resolves from the inventory when --host isn't set
// so the operator doesn't have to know the placement to drop a
// row. Aliased as `rm` for symmetry with `weft microvm rm` and
// the docker/k8s muscle memory.
package delete

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft instance delete` cobra command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		project string
		host    string
		force   bool
	)
	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a VM (cross-host dispatch ; rolls back boot artefacts on the owning agent)",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if !force {
				fmt.Fprintf(os.Stderr, "About to delete VM %q. This is irreversible — type yes to confirm : ", name)
				reader := bufio.NewReader(os.Stdin)
				line, _ := reader.ReadString('\n')
				if strings.TrimSpace(strings.ToLower(line)) != "yes" {
					return fmt.Errorf("aborted")
				}
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = c.DeleteVM(context.Background(), &weftv1.DeleteVMRequest{
				Name:     name,
				Project:  project,
				HostUuid: host,
			})
			if err != nil {
				return err
			}
			fmt.Printf("deleted\t%s\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project the VM belongs to (defaults to the agent's default project)")
	cmd.Flags().StringVar(&host, "host", "", "Owning host UUID (server auto-resolves from inventory when empty)")
	cmd.Flags().BoolVar(&force, "force", false, "Skip the confirmation prompt")
	return cmd
}
