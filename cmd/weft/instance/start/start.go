// Package start implements the weft start sub-command.
package start

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the start cobra command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	var pciRaw []string
	cmd := &cobra.Command{
		Use:   "start <name>",
		Short: "Start a VM",
		Long: `Start a VM. Use --pci to passthrough additional PCI(e) devices at
start time on top of any passthrough configured at create time.

The --pci flag takes a vendor:device[:count] tuple (lowercase
hex, no "0x" prefix) and may be repeated. Examples :

  --pci 8086:1572        # one Intel 82599-family NIC port
  --pci 8086:1572:2      # two such ports
  --pci 144d::1          # any Samsung NVMe SSD

The scheduler matches the request against the chosen host's
PCI inventory ; QEMU drivers attach via -device vfio-pci ;
Apple-VZ rejects the request (no PCI passthrough on macOS).

See docs/operations/pci-passthrough.md for the full workflow.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			pci, err := parsePCIFlags(pciRaw)
			if err != nil {
				return err
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = c.StartVM(context.Background(), &weftv1.StartVMRequest{
				Name:         args[0],
				RequestedPci: pci,
			})
			return err
		},
	}
	cmd.Flags().StringSliceVar(&pciRaw, "pci", nil, "PCI passthrough tuple vendor:device[:count] (lowercase hex, repeatable)")
	return cmd
}

// parsePCIFlags turns each --pci string into a PCIPassthroughRequest.
// Tuple shape : "vendor:device[:count]" — vendor required, device
// optional (empty = "any device from this vendor"), count optional
// (defaults to 1).
//
// Lifted out so it can be unit-tested without a gRPC client.
func parsePCIFlags(raw []string) ([]*weftv1.PCIPassthroughRequest, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]*weftv1.PCIPassthroughRequest, 0, len(raw))
	for _, s := range raw {
		parts := strings.Split(s, ":")
		if len(parts) < 1 || len(parts) > 3 || parts[0] == "" {
			return nil, fmt.Errorf("--pci %q: want vendor[:device[:count]]", s)
		}
		req := &weftv1.PCIPassthroughRequest{VendorId: strings.ToLower(parts[0])}
		if len(parts) >= 2 {
			req.DeviceId = strings.ToLower(parts[1])
		}
		if len(parts) == 3 && parts[2] != "" {
			n, err := strconv.Atoi(parts[2])
			if err != nil || n < 0 {
				return nil, fmt.Errorf("--pci %q: count must be a non-negative integer", s)
			}
			req.Count = int32(n)
		}
		out = append(out, req)
	}
	return out, nil
}
