// run.go implements `weft microvm run` — boots a microVM from an OCI
// image, the Docker-`run` analogue. Host-side prep + RegisterMicroVM
// + StartVM all live in the shared weft-microvm library; this file is
// the thin cobra front-end that builds microvm.Args from the flags.
package microvm

import (
	"github.com/openweft/weft-microvm"
	"github.com/spf13/cobra"
)

// runCmd returns the `weft microvm run` command. The WeftSocket field
// is sourced from the shared --socket flag so the library dials the
// same agent the rest of the CLI talks to.
func runCmd(socket *string) *cobra.Command {
	var (
		mountTag string
		detach   bool
		project  string
		pod      string
	)
	cmd := &cobra.Command{
		Use:   "run IMAGE[:TAG] [-- CMD...]",
		Short: "Boot a microVM from an OCI image",
		Long: `Boots a microVM from an OCI image (auto-pulls on cache miss).

Everything after a "--" separator overrides the image's
entrypoint+cmd, e.g.:

	weft microvm run alpine:3.21 -- sh -c "echo hi"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Split the positional args at the cobra "--" boundary:
			// args before the dash are [IMAGE]; args after the dash
			// are the command override. ArgsLenAtDash() == -1 means
			// no "--" was present (whole arg list is positional).
			image := args[0]
			var cmdOverride []string
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				cmdOverride = args[dash:]
			}
			return microvm.Run(microvm.Args{
				Image:     image,
				Cmd:       cmdOverride,
				Detach:    detach,
				MountTag:  mountTag,
				WeftSocket: *socket,
				Project:   project,
				Pod:       pod,
			})
		},
	}
	cmd.Flags().StringVar(&mountTag, "mount-tag", "", "virtio-fs tag exposed inside the guest (default rootfs0)")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "return once the VM is alive instead of streaming stdio")
	cmd.Flags().StringVar(&project, "project", "", "project namespace (empty = agent default)")
	cmd.Flags().StringVar(&pod, "pod", "", "path to a pod manifest JSON (multi-container mode)")
	return cmd
}
