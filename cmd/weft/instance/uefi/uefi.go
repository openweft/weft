// Package uefi implements `weft instance uefi` :
// CRUD over a VM's UEFI NVRAM variables, exposed by vzd's
// WeftAgent.{List,Set,Delete}UEFIVar RPCs.
//
//	weft instance uefi ls  <vm>                                     list vars
//	weft instance uefi set <vm> <name> --value <hex> [--ns <guid>]  upsert
//	weft instance uefi rm  <vm> <name> [--ns <guid>]                delete
//
// Variables live in a (namespace, name) tuple : the namespace is
// the EFI vendor GUID and defaults to the EFI Global Variable GUID
// (8be4df61-93ca-11d2-aa0d-00e098032b8c) so an operator who just
// wants to nudge BootOrder doesn't have to type the GUID.
//
// --value is hex of the raw bytes ; spaces are stripped for
// operator-friendly pastes (`--value '01 00 00 00'`). Empty is
// valid (a UEFI variable can carry an empty value). --attributes
// is the standard UEFI flag set (NonVolatile, BootServiceAccess,
// RuntimeAccess, …), comma-separated.
package uefi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/openweft/weft/cmd/weft/shared"
	vzdv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// efiGlobalNS is the EFI Global Variable GUID. Pulled in as a
// constant so we don't depend on the weft library from the CLI ; the
// server defaults to this anyway when namespace is empty.
const efiGlobalNS = "8be4df61-93ca-11d2-aa0d-00e098032b8c"

// Command returns the `weft instance uefi` cobra group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uefi",
		Short: "Manage per-VM UEFI NVRAM variables",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		setCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		project string
		format  string
	)
	cmd := &cobra.Command{
		Use:   "ls <vm>",
		Short: "List a VM's UEFI variables",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListUEFIVars(context.Background(), &vzdv1.ListUEFIVarsRequest{
				VmName: args[0], Project: project,
			})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Vars)
			}
			return renderTable(resp.Vars)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Narrow to a project namespace")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func setCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		project    string
		namespace  string
		valueHex   string
		attributes []string
	)
	cmd := &cobra.Command{
		Use:   "set <vm> <name>",
		Short: "Create or update a UEFI variable",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			ns := namespace
			if ns == "" {
				ns = efiGlobalNS
			}
			resp, err := c.SetUEFIVar(context.Background(), &vzdv1.SetUEFIVarRequest{
				VmName: args[0], Project: project,
				Var: &vzdv1.UEFIVar{
					Namespace:  ns,
					Name:       args[1],
					ValueHex:   valueHex,
					Attributes: attributes,
				},
			})
			if err != nil {
				return err
			}
			fmt.Printf("set\t%s\t%s/%s\tvalue=%q\n",
				args[0], resp.Var.Namespace, resp.Var.Name, resp.Var.ValueHex)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Narrow to a project namespace")
	cmd.Flags().StringVar(&namespace, "ns", "", `EFI vendor GUID (default: EFI Global, "8be4df61-…")`)
	cmd.Flags().StringVar(&valueHex, "value", "", "Hex of the raw bytes (spaces are stripped server-side ; empty = empty var)")
	cmd.Flags().StringSliceVar(&attributes, "attributes", nil, "Comma-separated flags (NonVolatile, BootServiceAccess, RuntimeAccess, ...)")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		project   string
		namespace string
	)
	cmd := &cobra.Command{
		Use:   "rm <vm> <name>",
		Short: "Delete a UEFI variable (idempotent)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			ns := namespace
			if ns == "" {
				ns = efiGlobalNS
			}
			if _, err := c.DeleteUEFIVar(context.Background(), &vzdv1.DeleteUEFIVarRequest{
				VmName: args[0], Project: project, Namespace: ns, Name: args[1],
			}); err != nil {
				return err
			}
			fmt.Printf("deleted\t%s\t%s/%s\n", args[0], ns, args[1])
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Narrow to a project namespace")
	cmd.Flags().StringVar(&namespace, "ns", "", "EFI vendor GUID (default: EFI Global)")
	return cmd
}

func renderTable(vars []*vzdv1.UEFIVar) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tNAME\tVALUE_HEX\tATTRIBUTES\tUPDATED_AT")
	for _, v := range vars {
		ns := v.Namespace
		if ns == efiGlobalNS {
			ns = "EFI_GLOBAL"
		}
		attrs := strings.Join(v.Attributes, ",")
		if attrs == "" {
			attrs = "-"
		}
		val := v.ValueHex
		if val == "" {
			val = "(empty)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ns, v.Name, val, attrs, v.UpdatedAt)
	}
	return tw.Flush()
}

func dumpJSON(vars []*vzdv1.UEFIVar) error {
	type out struct {
		Namespace  string   `json:"namespace"`
		Name       string   `json:"name"`
		ValueHex   string   `json:"value_hex"`
		Attributes []string `json:"attributes"`
		UpdatedAt  string   `json:"updated_at"`
	}
	flat := make([]out, len(vars))
	for i, v := range vars {
		flat[i] = out{v.Namespace, v.Name, v.ValueHex, v.Attributes, v.UpdatedAt}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
