// Package gc implements `weft instance gc` — operator-side query
// + actuator for the V0.1.12 zombie garbage collector. Reads the
// agent's running reconciler state via the V0.1.15 GetZombieReport
// gRPC and renders one row per zombie with its classification.
//
// The agent reconciler runs on its own ticker (default 5min,
// tunable via WEFT_ZOMBIE_GC_SWEEP_INTERVAL). This CLI shows what
// the agent currently sees — no client-side heuristics involved.
package gc

import (
	"context"
	"fmt"
	"time"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft instance gc` cobra command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Show VM zombies as classified by the agent's reconciler (V0.1.15)",
		Long: `Show VM zombies as currently classified by the agent's running
zombiegc reconciler.

Zombie kinds :
  local           — VM record points at this host but no process
  ci_cross_host   — deployment.type=ci on a Down host (auto-deleted
                    after WEFT_ZOMBIE_GC_CI_GRACE, default 1h)
  ha_cross_host   — non-CI on a Down host (NEVER auto-deleted)
  orphan_project  — project no longer exists in registry
  orphan_dir      — vmDir on disk with no registry record
                    (auto-deletable past WEFT_ZOMBIE_GC_ORPHAN_DIR_DELETE_AFTER)

The reconciler sweeps every WEFT_ZOMBIE_GC_SWEEP_INTERVAL (default
5min). This CLI just fetches the latest sweep result — no client-side
classification — so the output matches what Prometheus and the agent
logs see.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.GetZombieReport(context.Background(), &weftv1.GetZombieReportRequest{})
			if err != nil {
				return err
			}
			renderReport(resp)
			return nil
		},
	}
	return cmd
}

func renderReport(resp *weftv1.GetZombieReportResponse) {
	if len(resp.Zombies) == 0 {
		fmt.Printf("no zombies (deleted_total=%d, last_sweep=%s)\n",
			resp.DeletedTotal, fmtUnixNs(resp.LastSweepAtUnixNs))
		return
	}
	fmt.Printf("Last sweep : %s\nTotals     : ", fmtUnixNs(resp.LastSweepAtUnixNs))
	first := true
	for kind, n := range resp.ZombiesByKind {
		if n == 0 {
			continue
		}
		if !first {
			fmt.Print(", ")
		}
		fmt.Printf("%s=%d", kind, n)
		first = false
	}
	fmt.Printf("\nDeleted    : %d (cumulative)\n\n", resp.DeletedTotal)

	fmt.Printf("%-50s %-15s %-12s %s\n", "VM", "KIND", "DEPLOYMENT", "REASON")
	for _, z := range resp.Zombies {
		dep := z.DeploymentType
		if dep == "" {
			dep = "-"
		}
		fmt.Printf("%-50s %-15s %-12s %s\n", z.Name, z.Kind, dep, z.Reason)
	}
}

func fmtUnixNs(ns int64) string {
	if ns == 0 {
		return "never"
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339)
}
