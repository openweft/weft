// ls.go implements `weft microvm ls` — the Docker-`ps` analogue for
// microVMs. Discovery is purely agent-side: it calls ListVMs and
// keeps only the VMs whose name carries the microVM prefix that `run`
// stamps on every VM it registers. Classic VMs (cloud-boot distros,
// `weft instance` launches) are filtered out so the output stays
// focused on container workloads — `weft instance list` shows the
// full inventory. `-a` disables the filter (docker-ps-`-a` feel).
package microvm

import (
	"context"
	"strings"

	"github.com/openweft/weft/cmd/weft/shared"
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// lsCmd returns the `weft microvm ls` command.
func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		format  string
		all     bool
		project string
	)
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List microVMs",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListVMs(context.Background(), &vzdv1.ListVMsRequest{Project: project})
			if err != nil {
				return err
			}
			vms := filterMicroVMs(resp.Vms, all)
			if format == "json" {
				return shared.PrintJSON(vms)
			}
			shared.RenderTable(vms)
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include non-microVM VMs from the agent inventory")
	cmd.Flags().StringVar(&project, "project", "", "list only VMs in the given project")
	return cmd
}

// filterMicroVMs keeps only the VMs whose name carries the microVM
// prefix, unless `all` is set (then every VM passes through). Mirrors
// the original `ncl ls` filter so the two front-ends agree on which
// VMs count as microVMs.
func filterMicroVMs(vms []*vzdv1.VMInfo, all bool) []*vzdv1.VMInfo {
	if all {
		return vms
	}
	out := make([]*vzdv1.VMInfo, 0, len(vms))
	for _, vm := range vms {
		if strings.HasPrefix(vm.Name, vmNamePrefix) {
			out = append(out, vm)
		}
	}
	return out
}
