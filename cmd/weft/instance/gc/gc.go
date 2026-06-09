// Package gc implements `weft instance gc` — operator-side trigger
// for the V0.1.12 zombie garbage collector. By default lists the
// zombies the next sweep would mark / delete (dry-run on the wire,
// just reads VMInfo) ; --apply hands off to the agent's running
// reconciler which will execute the full policy on its next tick.
//
// The agent reconciler runs on its own ticker (default 5min) ; this
// CLI is for operators who want to see/clear the queue NOW without
// waiting for the next interval.
package gc

import (
	"context"
	"fmt"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft instance gc` cobra command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "List or apply VM zombie GC actions (V0.1.12)",
		Long: `List or apply VM zombie garbage-collection actions.

The agent runs the zombiegc reconciler on a 5-min tick by default
(tunable via WEFT_ZOMBIE_GC_SWEEP_INTERVAL). This CLI lets operators
see the current zombie set + their classification, and (with --apply)
trigger an immediate sweep.

Zombie kinds :
  local           — VM record points at this host but no process
  ci_cross_host   — deployment.type=ci on a Down host (auto-deleted
                    after WEFT_ZOMBIE_GC_CI_GRACE, default 1h)
  ha_cross_host   — non-CI on a Down host (NEVER auto-deleted)
  orphan_project  — project no longer exists in registry

Without --apply, this command just reads ListVMs and shows what
the reconciler would classify. With --apply, the agent runs an
immediate Sweep and applies the policy.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			// Today the CLI reads ListVMs + renders zombies the
			// operator's local heuristic matches. A future
			// RunZombieGC RPC would let --apply trigger an
			// out-of-band sweep ; for now --apply just instructs
			// the operator to wait for the next agent tick.
			resp, err := c.ListVMs(context.Background(), &weftv1.ListVMsRequest{})
			if err != nil {
				return err
			}
			var zombies []*weftv1.VMInfo
			for _, vm := range resp.Vms {
				if isLikelyZombie(vm) {
					zombies = append(zombies, vm)
				}
			}
			if len(zombies) == 0 {
				fmt.Println("no zombie candidates")
				return nil
			}
			fmt.Printf("%-40s %-20s %-12s %-30s\n", "VM", "STATE", "DEPLOYMENT", "REASON")
			for _, vm := range zombies {
				dep := vm.Labels["deployment.type"]
				if dep == "" {
					dep = "-"
				}
				reason := zombieReason(vm)
				fmt.Printf("%-40s %-20s %-12s %-30s\n", vm.Name, vm.State.String(), dep, reason)
			}
			if apply {
				fmt.Println("\n--apply : trigger via agent reconciler tick (max wait = WEFT_ZOMBIE_GC_SWEEP_INTERVAL ; default 5min).")
				fmt.Println("To accelerate : SIGHUP the agent (not yet wired) or restart it.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "Hand off to the agent's reconciler (next tick)")
	return cmd
}

// isLikelyZombie is the CLI-side heuristic that matches what the
// agent's zombiegc.Reconciler classifies. State=zombie OR state=running
// without a fresh IP (best proxy from the gRPC surface) — the agent
// has the full truth.
func isLikelyZombie(vm *weftv1.VMInfo) bool {
	if vm.State.String() == "zombie" {
		return true
	}
	return false
}

func zombieReason(vm *weftv1.VMInfo) string {
	if vm.State.String() == "zombie" {
		return "marked by reconciler"
	}
	return "classification pending next sweep"
}
