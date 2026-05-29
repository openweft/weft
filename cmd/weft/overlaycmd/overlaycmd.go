// Package overlaycmd implements `weft overlay` — operator-side helpers to
// provision a micro-VM's WireGuard overlay identity (keys + addresses) and
// emit the two files that close the loop: the guest's wireguard.json and the
// operator's coords for `weft instance ps --coords`.
package overlaycmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/openweft/weft/overlay"
	"github.com/spf13/cobra"
)

// Command returns the `overlay` cobra command group.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "overlay",
		Short: "Provision WireGuard overlay identities for micro-VMs",
	}
	cmd.AddCommand(provisionCmd())
	return cmd
}

func provisionCmd() *cobra.Command {
	var (
		outDir       string
		stateDir     string
		subnet       string
		endpointHost string
		vmIndex      int
		operatorIdx  int
		listenPort   uint
		agentPort    uint
		keepalive    uint
	)

	home, _ := os.UserHomeDir()
	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Mint a VM's overlay keypair + addresses and write wireguard.json + coords",
		Long: `Pairs a micro-VM with the operator on the WireGuard overlay and writes
two files into --out:

  wireguard.json          → stage into the VM's config share (next to
                            pod.json); weft-init brings up wg0 from it.
  overlay-operator.json   → pass to 'weft instance ps --coords' to reach
                            the VM's agent over the overlay.

The operator keypair is created once under --state-dir and reused, so the
operator keeps one identity across every VM it provisions.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			op, err := overlay.EnsureOperatorKey(stateDir)
			if err != nil {
				return fmt.Errorf("operator key: %w", err)
			}
			coords, err := overlay.Provision(outDir, overlay.Config{
				Subnet:        subnet,
				EndpointHost:  endpointHost,
				ListenPort:    uint16(listenPort),
				AgentPort:     uint16(agentPort),
				VMIndex:       vmIndex,
				OperatorIndex: operatorIdx,
				Keepalive:     uint16(keepalive),
			}, op)
			if err != nil {
				return err
			}
			fmt.Printf("provisioned overlay for VM index %d:\n", vmIndex)
			fmt.Printf("  guest config : %s\n", filepath.Join(outDir, overlay.GuestFileName))
			fmt.Printf("  operator     : %s\n", filepath.Join(outDir, overlay.CoordsFileName))
			fmt.Printf("  reach it with: weft instance ps --coords %s\n", filepath.Join(outDir, overlay.CoordsFileName))
			fmt.Printf("  vm overlay   : %s\n", coords.Target)
			return nil
		},
	}

	cmd.Flags().StringVar(&outDir, "out", ".", "directory to write wireguard.json + overlay-operator.json")
	cmd.Flags().StringVar(&stateDir, "state-dir", filepath.Join(home, ".weft"), "directory holding the stable operator key")
	cmd.Flags().StringVar(&subnet, "subnet", overlay.DefaultSubnet, "overlay CIDR")
	cmd.Flags().StringVar(&endpointHost, "endpoint-host", "", "host the operator reaches the VM's WireGuard port at (required)")
	cmd.Flags().IntVar(&vmIndex, "vm-index", 0, "the VM's host index in the subnet (required, >= 1)")
	cmd.Flags().IntVar(&operatorIdx, "operator-index", overlay.DefaultOperatorIndex, "the operator's host index in the subnet")
	cmd.Flags().UintVar(&listenPort, "listen-port", uint(overlay.DefaultListenPort), "the VM's WireGuard UDP listen port")
	cmd.Flags().UintVar(&agentPort, "agent-port", uint(overlay.DefaultAgentPort), "the VM agent's gRPC port")
	cmd.Flags().UintVar(&keepalive, "keepalive", 25, "operator→VM persistent keepalive seconds (0 to disable)")
	_ = cmd.MarkFlagRequired("endpoint-host")
	return cmd
}
