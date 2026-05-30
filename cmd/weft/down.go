// down.go wires `weft down` — the inverse of `weft up`. It reads the same
// cluster.hcl, computes the teardown plan (stop replicas in reverse topo
// order → stop agents → teardown mesh → optional purge), and applies it
// over SSH. Same Plan/RenderSSH/Apply infrastructure as up — only the
// ActionKinds differ.
package main

import (
	"fmt"

	"github.com/openweft/weft/cluster"
	"github.com/openweft/weft/infra"
	"github.com/spf13/cobra"
)

func newDownCmd() *cobra.Command {
	var file string
	var apply bool
	var showSSH bool
	var purge bool

	cmd := &cobra.Command{
		Use:   "down",
		Short: "Tear down a weft cluster described by a cluster.hcl",
		Long: `Reverse of 'weft up': stops every placed infra replica, kills the
weft agent on each host, and wipes the WireGuard overlay.

Without --purge the host's identity (host UUID) and embed-etcd data are
kept, so a subsequent 'weft up' resumes against the same cluster ID. With
--purge, ~/.weft and /var/lib/weft are removed on every host.

Without --apply it prints the plan (dry-run).`,
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
			plan, err := cluster.BuildDownPlan(cl, ordered, cluster.DownOptions{Purge: purge})
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
	cmd.Flags().BoolVar(&purge, "purge", false, "after teardown, remove ~/.weft and /var/lib/weft on each host (destroys host identity + etcd data)")
	return cmd
}
