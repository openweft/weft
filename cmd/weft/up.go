// up.go wires `weft up` — the operator-facing, convergent cluster
// bring-up. It reads a cluster.hcl (1 host = single-node, 3 hosts = 3-DC),
// crosses it with the topo-sorted infra DAG, and computes the ordered set of
// actions to converge the cluster to the described state. Re-running after
// editing the HCL (e.g. adding two hosts) yields only the delta — that's how
// "extend a single host to a cluster" works.
//
// `weft up` is the orchestrator; per host it composes the existing per-host
// primitive (`weft infra bootstrap` / the infra package) plus the cross-host
// concerns (overlay mesh, etcd/nats quorum, replica placement).
package main

import (
	"fmt"

	"github.com/openweft/weft/cluster"
	"github.com/openweft/weft/infra"
	"github.com/spf13/cobra"
)

func newUpCmd() *cobra.Command {
	var file string
	var apply bool
	var showSSH bool

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Bring up (or extend) a weft cluster from a cluster.hcl",
		Long: `Reads a cluster description and converges the cluster to it.

  1 host  → single-node (every infra service collapses to one replica)
  3 hosts → 3-DC cluster (one etcd/nats replica per DC, WireGuard mesh)

up is convergent: re-running after adding hosts to the HCL applies only
the delta (join hosts, grow etcd/nats quorum, extend the mesh), so a
single host can be grown into a 3-DC cluster in place.

Without --apply it prints the plan (dry-run). Each placement is executed
per host through the same infra deploy primitive as 'weft infra bootstrap'.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			cl, err := cluster.Load(file)
			if err != nil {
				return err
			}
			root := moduleRoot()
			services, err := cl.ResolveServices(root)
			if err != nil {
				return fmt.Errorf("resolve infra services: %w", err)
			}
			plans, err := infra.LoadAllPlans(root, services)
			if err != nil {
				return err
			}
			ordered, err := infra.TopologicalSort(plans)
			if err != nil {
				return err
			}

			// Dry-run plans against an empty (bootstrap) state. Reconciling
			// against a *live* cluster's observed state — which turns this
			// into the extend/grow delta — is the execution path: it queries
			// the running control plane for current hosts + placements, and
			// lands together with the host-access model.
			plan, err := cluster.Build(cl, ordered, cluster.State{})
			if err != nil {
				return err
			}
			fmt.Print(plan.String())

			if showSSH || apply {
				fmt.Println("\nSSH execution plan (per host):")
				for _, hp := range cluster.RenderSSH(cl, plan) {
					fmt.Printf("  ssh %s@%s\n", hp.Target.User, hp.Target.Addr)
					for _, step := range hp.Steps {
						fmt.Printf("      %s\n", step)
					}
				}
			}

			if apply {
				fmt.Println("\napplying over SSH…")
				return cluster.Apply(cl, plan, root, func(f string, a ...any) { logger.Printf(f, a...) })
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "cluster.hcl", "path to the cluster description (HCL)")
	cmd.Flags().BoolVar(&showSSH, "ssh", false, "also print the per-host SSH execution plan")
	cmd.Flags().BoolVar(&apply, "apply", false, "execute the plan over SSH (needs the hosts reachable with the configured keys)")
	return cmd
}
