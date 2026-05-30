// Package logs implements `weft instance logs` — dumps the raw
// console.log bytes for a VM via the VMLogs RPC.
//
// CLI shape:
//
//	weft instance logs <name> [--tail BYTES]
//
// Output is the byte stream the guest produced on its serial port
// (boot messages, init log, container stdout/stderr, all
// interleaved). weft does not interpret it; this command is the
// thinnest possible reader.
package logs

import (
	"context"
	"fmt"
	"os"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft instance logs` cobra command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	var tail int64
	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Dump the serial console log for a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.VMLogs(context.Background(), &weftv1.VMLogsRequest{
				Name:      args[0],
				TailBytes: tail,
			})
			if err != nil {
				return err
			}
			if _, err := os.Stdout.Write(resp.Contents); err != nil {
				return err
			}
			// Truncation hint goes to stderr so it doesn't pollute
			// piped reads of the log itself.
			if tail > 0 && resp.TotalBytes > int64(len(resp.Contents)) {
				fmt.Fprintf(os.Stderr,
					"\n[weft: truncated — showed last %d B of %d B]\n",
					len(resp.Contents), resp.TotalBytes)
			}
			return nil
		},
	}
	cmd.Flags().Int64Var(&tail, "tail", 0, "Show only the last N bytes (0 = whole file)")
	return cmd
}
