// Package microvm implements the `weft microvm` sub-command group —
// the Docker-style "OCI image → microVM" front-end. It wires the
// shared `github.com/openweft/weft-microvm` library (host-side rootfs
// prep + RegisterMicroVM/StartVM over gRPC) for the run/pull/init-build
// verbs, and the shared gRPC client (`shared.Client`) for the
// inventory verbs (ls/rm/logs) so the rendering stays consistent with
// the rest of the `weft` CLI.
//
// CLI agnosticism: every verb is `weft microvm <verb>`. No
// hypervisor-prefixed names (no `vz-*`) leak into the surface — the
// microVM abstraction is driver-neutral.
package microvm

import (
	"github.com/spf13/cobra"
)

// vmNamePrefix is the prefix the microVM run path stamps on every VM
// it registers, keeping the microVM namespace disjoint from classic
// VMs in the same agent inventory. Mirrors the `weft-microvm-` prefix the
// original weft-microvm runner used so `ls`/`rm`/`logs` filter identically.
const vmNamePrefix = "weft-microvm-"

// Command returns the `microvm` cobra command with its sub-commands.
// The connection-flag pointers are shared with the rest of the client
// tree (see cmd/weft/main.go).
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "microvm",
		Short: "Manage microVMs (Docker-style: OCI image → microVM)",
	}
	cmd.AddCommand(
		runCmd(socket),
		pullCmd(),
		pullKernelCmd(),
		pullPodInitrdCmd(),
		lsCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
		logsCmd(socket, sshSocket, sshKey),
		initBuildCmd(),
		podInitBuildCmd(),
	)
	return cmd
}
