// Package property implements `weft instance property` :
// CRUD over the per-VM annotation registry exposed by weft's
// WeftAgent.{List,Set,Delete}VMProperty RPCs.
//
//	weft instance property ls   <vm>                              list keys
//	weft instance property set  <vm> <key>=<value> [--guest]      upsert
//	weft instance property rm   <vm> <key>                        delete
//
// Properties are per-VM annotations operators can attach without
// involving the hypervisor — owner, environment, runbook URL, …
// Setting --guest exposes a property to the in-guest weft-microvm-agent
// for the VM to read at boot or via the NATS subject (see the
// weft.boot/script convention).
//
// --project narrows the operation to a specific namespace when the
// VM name is ambiguous across projects ; default is the empty
// project ("" = the implicit default).
package property

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft instance property` cobra group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "property",
		Short: "Manage per-VM annotations (key / value, optionally guest-readable)",
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
		Short: "List a VM's properties",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListVMProperties(context.Background(), &weftv1.ListVMPropertiesRequest{
				VmName: args[0], Project: project,
			})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Properties)
			}
			return renderTable(resp.Properties)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Narrow to a project namespace")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

// setCmd accepts `key=value`. Empty values are valid (operator might
// want a "tombstone" property) ; the server's SetVMProperty
// validates the key is non-empty.
func setCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		project       string
		guestReadable bool
	)
	cmd := &cobra.Command{
		Use:   "set <vm> <key>=<value>",
		Short: "Create or update a property",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			key, value, err := splitKV(args[1])
			if err != nil {
				return err
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.SetVMProperty(context.Background(), &weftv1.SetVMPropertyRequest{
				VmName: args[0], Project: project,
				Property: &weftv1.VMProperty{
					Key: key, Value: value, GuestReadable: guestReadable,
				},
			})
			if err != nil {
				return err
			}
			tag := "set"
			if resp.Property.GuestReadable {
				tag = "set+guest"
			}
			fmt.Printf("%s\t%s\t%s=%s\n", tag, args[0], resp.Property.Key, resp.Property.Value)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Narrow to a project namespace")
	cmd.Flags().BoolVar(&guestReadable, "guest", false, "Expose this property to the in-guest weft-microvm-agent")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "rm <vm> <key>",
		Short: "Delete a property (idempotent)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.DeleteVMProperty(context.Background(), &weftv1.DeleteVMPropertyRequest{
				VmName: args[0], Project: project, Key: args[1],
			}); err != nil {
				return err
			}
			fmt.Printf("deleted\t%s\t%s\n", args[0], args[1])
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Narrow to a project namespace")
	return cmd
}

// splitKV parses "key=value" ; the value half may be empty
// ("tombstone" property) but the key half can't be — same shape
// as `docker label k=v`.
func splitKV(s string) (string, string, error) {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return "", "", errors.New(`expected "key=value" (key must be non-empty)`)
	}
	return s[:i], s[i+1:], nil
}

func renderTable(properties []*weftv1.VMProperty) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tVALUE\tGUEST\tUPDATED_AT")
	for _, p := range properties {
		guest := "-"
		if p.GuestReadable {
			guest = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Key, p.Value, guest, p.UpdatedAt)
	}
	return tw.Flush()
}

func dumpJSON(properties []*weftv1.VMProperty) error {
	type out struct {
		Key           string `json:"key"`
		Value         string `json:"value"`
		GuestReadable bool   `json:"guest_readable"`
		UpdatedAt     string `json:"updated_at"`
	}
	flat := make([]out, len(properties))
	for i, p := range properties {
		flat[i] = out{p.Key, p.Value, p.GuestReadable, p.UpdatedAt}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
