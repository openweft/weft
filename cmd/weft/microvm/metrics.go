// metrics.go implements `weft microvm metrics` — read the latest
// per-VM telemetry snapshot (cpu / mem / net / disk + uptime) the
// agent has on file. Today the agent returns the zero shape until
// the hypervisor-driver sampling pipeline lands ; the wire surface
// is in place so future runtime telemetry plumbing is a server-side
// change only.
package microvm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

func metricsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		vmUUID  string
		name    string
		project string
		format  string
	)
	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Show the latest telemetry snapshot for a microVM",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if vmUUID == "" && name == "" {
				return fmt.Errorf("--vm (UUID) or --name (with optional --project) is required")
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.GetMicroVMMetrics(context.Background(), &weftv1.GetMicroVMMetricsRequest{
				VmUuid:  vmUUID,
				Name:    name,
				Project: project,
			})
			if err != nil {
				return err
			}
			if format == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"vm_uuid":             resp.VmUuid,
					"sampled_at_unix_ns":  resp.SampledAtUnixNs,
					"cpu_percent":         resp.CpuPercent,
					"mem_used_mib":        resp.MemUsedMib,
					"mem_total_mib":       resp.MemTotalMib,
					"net_rx_bps":          resp.NetRxBps,
					"net_tx_bps":          resp.NetTxBps,
					"disk_read_bps":       resp.DiskReadBps,
					"disk_write_bps":      resp.DiskWriteBps,
					"uptime_ms":           resp.UptimeMs,
				})
			}
			fmt.Printf("VM            %s\n", resp.VmUuid)
			if resp.SampledAtUnixNs == 0 {
				fmt.Println("Sample        (no sample taken yet — runtime telemetry pipeline pending)")
			} else {
				fmt.Printf("Sample (ns)   %d\n", resp.SampledAtUnixNs)
			}
			fmt.Printf("CPU           %.2f%%\n", resp.CpuPercent)
			fmt.Printf("Memory        %d / %d MiB\n", resp.MemUsedMib, resp.MemTotalMib)
			fmt.Printf("Network       rx=%d  tx=%d bps\n", resp.NetRxBps, resp.NetTxBps)
			fmt.Printf("Disk          read=%d  write=%d bps\n", resp.DiskReadBps, resp.DiskWriteBps)
			fmt.Printf("Uptime        %d ms\n", resp.UptimeMs)
			return nil
		},
	}
	cmd.Flags().StringVar(&vmUUID, "vm", "", "Target VM UUID")
	cmd.Flags().StringVar(&name, "name", "", "Target VM name (fallback when --vm isn't set)")
	cmd.Flags().StringVar(&project, "project", "", "Project (name or UUID) scoping --name")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}
