// Package pod implements the `weft pod` sub-command group — the
// operator-facing surface for publishing a guestv1.PodSpec into the
// agent's in-memory registry. The agent serves the spec back to the
// in-guest weft-init via the GuestPodPlane HelloAck on the vsock
// stream. See pod_specs.go + cmd/weft/pod_specs_handler.go for the
// server side.
package pod

import (
	"github.com/spf13/cobra"
)

// Command returns the `pod` cobra command tree. socket / sshSocket /
// sshKey pointers are shared with the rest of the client tree (see
// cmd/weft/main.go).
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pod",
		Short: "Publish + inspect operator-supplied PodSpecs for microVMs",
	}
	cmd.AddCommand(
		setSpecCmd(socket, sshSocket, sshKey),
		getSpecCmd(socket, sshSocket, sshKey),
	)
	return cmd
}
