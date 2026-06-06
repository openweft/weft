// Package flavor implements the `weft flavor` subcommand group :
// CRUD over the cluster-wide compute-envelope catalogue exposed by
// weft's WeftAgent.{List,Get,Set,Delete}Flavor RPCs.
//
//	weft flavor ls                  list every flavor
//	weft flavor get <name>          show one flavor
//	weft flavor set <name> [flags]  create or update (admin)
//	weft flavor rm  <name>          delete (admin)
//
// Operators reach for this from `weft up` host bootstraps, from
// CI seeders that mirror prod flavors into a dev cluster, or
// interactively when the dashboard's CreateVMModal needs a new
// size class.
package flavor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft flavor` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "flavor",
		Short: "Manage the cluster-wide compute-envelope catalogue",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		getCmd(socket, sshSocket, sshKey),
		setCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "ls",
		Aliases: []string{"list"},
		Short: "List flavors",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListFlavors(context.Background(), &weftv1.ListFlavorsRequest{})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Flavors)
			}
			return renderTable(resp.Flavors)
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func getCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show one flavor's compute envelope",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.GetFlavor(context.Background(), &weftv1.GetFlavorRequest{Name: args[0]})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON([]*weftv1.Flavor{resp.Flavor})
			}
			return renderTable([]*weftv1.Flavor{resp.Flavor})
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

// setCmd creates or updates a flavor. Required values that the
// operator wants to change must be passed via flags ; the server's
// SetFlavor enforces a >0 vCPU + non-empty RAM, so a bare `set
// <name>` errors out rather than producing a zero-sized envelope.
func setCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		vcpu    int32
		ram     string
		ephemGB int32
		gpu     string
	)
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Create or update a flavor (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.SetFlavor(context.Background(), &weftv1.SetFlavorRequest{
				Flavor: &weftv1.Flavor{
					Name:        args[0],
					Vcpu:        vcpu,
					Ram:         ram,
					EphemeralGb: ephemGB,
					Gpu:         gpu,
				},
			})
			if err != nil {
				return err
			}
			fmt.Printf("set\t%s\tvcpu=%d\tram=%s\tephemeral=%dGi\tgpu=%q\n",
				resp.Flavor.Name, resp.Flavor.Vcpu, resp.Flavor.Ram,
				resp.Flavor.EphemeralGb, resp.Flavor.Gpu)
			return nil
		},
	}
	cmd.Flags().Int32Var(&vcpu, "vcpu", 0, "vCPU count (required, >0)")
	cmd.Flags().StringVar(&ram, "ram", "", `RAM (required, "4Gi" / "512Mi" / raw integer = MB)`)
	cmd.Flags().Int32Var(&ephemGB, "ephemeral-gb", 0, "Ephemeral disk size in GiB (0 = none)")
	cmd.Flags().StringVar(&gpu, "gpu", "", `GPU descriptor (empty = none ; "1×A100-40G")`)
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a flavor (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.DeleteFlavor(context.Background(), &weftv1.DeleteFlavorRequest{Name: args[0]})
			if err != nil {
				return err
			}
			fmt.Printf("deleted\t%s\n", resp.Deleted)
			return nil
		},
	}
}

func renderTable(flavors []*weftv1.Flavor) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVCPU\tRAM\tEPHEMERAL\tGPU")
	for _, f := range flavors {
		gpu := f.Gpu
		if gpu == "" {
			gpu = "-"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%dGi\t%s\n", f.Name, f.Vcpu, f.Ram, f.EphemeralGb, gpu)
	}
	return tw.Flush()
}

func dumpJSON(flavors []*weftv1.Flavor) error {
	type out struct {
		Name        string `json:"name"`
		VCPU        int32  `json:"vcpu"`
		RAM         string `json:"ram"`
		EphemeralGB int32  `json:"ephemeral_gb"`
		GPU         string `json:"gpu"`
	}
	flat := make([]out, len(flavors))
	for i, f := range flavors {
		flat[i] = out{f.Name, f.Vcpu, f.Ram, f.EphemeralGb, f.Gpu}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
