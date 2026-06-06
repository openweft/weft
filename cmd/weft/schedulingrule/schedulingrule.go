// Package schedulingrule implements the `weft scheduling-rule`
// subcommand group — nominal placement rules per
// [[openweft_nominal_binding]].
//
//	weft scheduling-rule ls
//	weft scheduling-rule create --name N --selector "k=v,..." --target-count N
//	weft scheduling-rule update <uuid> [--selector --target-count --anti-affinity]
//	weft scheduling-rule rm     <uuid>
package schedulingrule

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/cmd/weft/shared"
	"github.com/spf13/cobra"
)

// Command returns the `weft scheduling-rule` parent command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "scheduling-rule",
		Aliases: []string{"sr"},
		Short:   "Manage cluster-wide nominal scheduling rules",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		createCmd(socket, sshSocket, sshKey),
		updateCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Aliases: []string{"list"},
		Short: "List scheduling rules",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListSchedulingRules(context.Background(), &weftv1.ListSchedulingRulesRequest{})
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "UUID\tNAME\tSELECTOR\tTARGET\tANTI_AFFINITY\tCREATED")
			for _, r := range resp.Rules {
				created := "-"
				if r.CreatedAtUnixNs != 0 {
					created = time.Unix(0, r.CreatedAtUnixNs).UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", r.Uuid, r.Name, dashIf(r.Selector), r.TargetCount, dashIf(r.AntiAffinity), created)
			}
			return tw.Flush()
		},
	}
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var name, selector, antiAffinity string
	var target int32
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a scheduling rule (admin-only)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CreateSchedulingRule(context.Background(), &weftv1.CreateSchedulingRuleRequest{
				Name: name, Selector: selector, TargetCount: target, AntiAffinity: antiAffinity,
			})
			if err != nil {
				return err
			}
			fmt.Printf("created\t%s\t%s\ttarget=%d\n", resp.Rule.Uuid, resp.Rule.Name, resp.Rule.TargetCount)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Rule name (unique, required)")
	cmd.Flags().StringVar(&selector, "selector", "", "Label selector \"k=v,...\"")
	cmd.Flags().Int32Var(&target, "target-count", 0, "Target replica count")
	cmd.Flags().StringVar(&antiAffinity, "anti-affinity", "", "host | az | rack")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func updateCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var selector, antiAffinity string
	var target int32 = -1
	cmd := &cobra.Command{
		Use:   "update <uuid>",
		Short: "Patch a scheduling rule (admin-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.UpdateSchedulingRule(context.Background(), &weftv1.UpdateSchedulingRuleRequest{
				Uuid: args[0], Selector: selector, TargetCount: target, AntiAffinity: antiAffinity,
			})
			if err != nil {
				return err
			}
			fmt.Printf("updated\t%s\ttarget=%d\n", resp.Rule.Uuid, resp.Rule.TargetCount)
			return nil
		},
	}
	cmd.Flags().StringVar(&selector, "selector", "", "Replace selector (empty = keep)")
	cmd.Flags().Int32Var(&target, "target-count", -1, "Replace target count (-1 = keep)")
	cmd.Flags().StringVar(&antiAffinity, "anti-affinity", "", "Replace anti-affinity (empty = keep)")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <uuid>",
		Short: "Delete a scheduling rule (admin-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.DeleteSchedulingRule(context.Background(), &weftv1.DeleteSchedulingRuleRequest{Uuid: args[0]}); err != nil {
				return err
			}
			fmt.Println(args[0])
			return nil
		},
	}
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
