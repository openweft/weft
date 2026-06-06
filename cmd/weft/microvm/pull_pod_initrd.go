// pull_pod_initrd.go implements `weft microvm pull-pod-initrd` — fetches the
// shared pod-mode initramfs from an OCI artifact (built by the
// openweft/weft-microvm-init release workflow) and writes it into the local
// weft-microvm data dir so the agent's pod boot path picks it up.
//
// Sibling of `weft microvm pull-kernel`; same shape, different artifact +
// destination path. The 4-arch OCI index published by the upstream workflow
// is resolved to the runtime.GOARCH-matching per-arch manifest.
package microvm

import (
	"github.com/openweft/weft-microvm"
	"github.com/spf13/cobra"
)

// pullPodInitrdCmd returns the `weft microvm pull-pod-initrd` command.
func pullPodInitrdCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pull-pod-initrd REF",
		Short: "Pull the shared pod-mode initramfs (weft-init + crun) from an OCI artifact",
		Long: `Fetches the pod-mode initramfs from an OCI artifact (produced by the
openweft/weft-microvm-init release workflow) and writes it to
$XDG_DATA_HOME/weft-microvm/pod-initrd — the path locatePodBoot expects.

The artifact's media types follow the openweft pod-initrd convention :

    application/vnd.openweft.microvm.pod-initrd        (artifactType)
    application/vnd.openweft.microvm.pod-initrd.cpio.gz (layer)

The reference resolves to a 4-arch OCI index ; the puller picks the
manifest matching runtime.GOARCH, then fetches its cpio.gz layer.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return microvm.PullPodInitrd(args[0])
		},
	}
}
