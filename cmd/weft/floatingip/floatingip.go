// Package floatingip implements the `weft floating-ip` subcommand
// group : allocate / release / map / unmap / list floating IPs over
// the FloatingIP RPCs that landed earlier in the proto. Pre-existing
// webui surface ; this package closes the Tier 3 gap surfaced by the
// CLI parity audit.
//
//	weft floating-ip ls [--project=<name|uuid>]              list every FIP
//	weft floating-ip show <uuid>                             single row
//	weft floating-ip status [<uuid>]                         control-plane + host-side check
//	weft floating-ip allocate --network=<name|uuid> [--project=<...>]
//	weft floating-ip release <uuid>
//	weft floating-ip map <uuid> --target=<name> [--kind=vm|lb]
//	weft floating-ip unmap <uuid>
//
// `show` is local-only : the proto has no GetFloatingIP RPC, so we
// call ListFloatingIPs and filter client-side. Fine for an operator
// CLI ; the webui keeps its own scoped query path. `status` extends
// `show` with a best-effort host-side check (VMStatus + the nftables
// rules the NAT reconciler would have programmed) so operators can
// confirm a mapping took effect end-to-end.
package floatingip

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft floating-ip` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "floating-ip",
		Short:   "Manage floating IPs (allocate, release, map to VM/LB)",
		Aliases: []string{"fip"},
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		showCmd(socket, sshSocket, sshKey),
		statusCmd(socket, sshSocket, sshKey),
		allocateCmd(socket, sshSocket, sshKey),
		releaseCmd(socket, sshSocket, sshKey),
		mapCmd(socket, sshSocket, sshKey),
		unmapCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, format string
	cmd := &cobra.Command{
		Use:   "ls",
		Aliases: []string{"list"},
		Short: "List floating IPs",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListFloatingIPs(context.Background(), &weftv1.ListFloatingIPsRequest{Project: project})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpFIPsJSON(resp.FloatingIps)
			}
			return renderFIPsTable(resp.FloatingIps)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Filter to one project (name or UUID)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func showCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "show <uuid>",
		Short: "Show a single floating IP",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListFloatingIPs(context.Background(), &weftv1.ListFloatingIPsRequest{})
			if err != nil {
				return err
			}
			var found *weftv1.FloatingIPInfo
			for _, f := range resp.FloatingIps {
				if f.Uuid == args[0] || f.Address == args[0] {
					found = f
					break
				}
			}
			if found == nil {
				return fmt.Errorf("no floating ip with uuid or address %q", args[0])
			}
			if format == "json" {
				return dumpFIPsJSON([]*weftv1.FloatingIPInfo{found})
			}
			return renderFIPsTable([]*weftv1.FloatingIPInfo{found})
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

// statusCmd implements `weft floating-ip status [<uuid>]`. With no
// argument it renders every FIP the caller can see, same column set
// as `ls`. With a UUID (or address) argument it renders the single
// matching FIP plus a host-side block : the VM the FIP is mapped to
// (when target_kind=vm), and the DNAT/SNAT pair the host's nftables
// reconciler would have programmed. The reconciler itself runs on the
// agent host and isn't observable from the CLI ; this command is a
// best-effort consistency check, NOT a live `nft list ruleset` dump.
//
// Branches for the host-side block :
//   - FIP unmapped → "not yet active (FIP unmapped)"
//   - FIP mapped to a VM, VMStatus resolves with a non-empty IP →
//     "nftables expected: dnat=<addr>→<vmIP>, snat=<vmIP>→<addr>"
//   - FIP mapped but VMStatus errors or returns no IP → "VM not
//     local to any visible host" (multi-host fleets may keep the VM
//     on a peer agent we can't reach over this socket)
//   - FIP mapped to an LB → "lb target — see `weft loadbalancer
//     show`" (no per-VM private IP to derive a NAT pair from)
func statusCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "status [<uuid>]",
		Short: "Show FIP state from the control-plane registry + host-side NAT check",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx := context.Background()
			resp, err := c.ListFloatingIPs(ctx, &weftv1.ListFloatingIPsRequest{})
			if err != nil {
				return err
			}
			// No argument : same table as `ls`, no host-side check
			// (would mean one VMStatus per row, easily abusive on
			// large fleets — operators run `show <uuid>` per-FIP
			// for that). The verb still exists for ad-hoc "what
			// does the control plane think ?" sweeps.
			if len(args) == 0 {
				if format == "json" {
					return dumpFIPsJSON(resp.FloatingIps)
				}
				return renderFIPsTable(resp.FloatingIps)
			}
			var found *weftv1.FloatingIPInfo
			for _, f := range resp.FloatingIps {
				if f.Uuid == args[0] || f.Address == args[0] {
					found = f
					break
				}
			}
			if found == nil {
				return fmt.Errorf("no floating ip with uuid or address %q", args[0])
			}
			host := resolveHostStatus(ctx, c, found)
			if format == "json" {
				return dumpFIPStatusJSON(found, host)
			}
			return renderFIPStatus(found, host)
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

// hostStatus captures the host-side picture of one FIP. Kept as a
// flat struct so the JSON dumper / human renderer share the same
// shape — no per-renderer recomputation, no risk of divergence.
type hostStatus struct {
	// Branch is the human label rendered alongside the control-plane
	// block. One of "active", "unmapped", "vm-unreachable", "lb".
	Branch string
	// VMIP is the private IP the host would DNAT to. Only set when
	// Branch == "active".
	VMIP string
	// Note is the rendered explanation line.
	Note string
}

// resolveHostStatus is the host-side companion to the control-plane
// FloatingIPInfo. The decision tree is small enough to stay inline
// rather than fan out behind an interface : empty mapped_to → unmapped,
// lb target → "lb" (no VM IP to derive), vm target → VMStatus to
// fetch the VM's IP, missing IP → vm-unreachable.
func resolveHostStatus(ctx context.Context, c weftv1.WeftAgentClient, f *weftv1.FloatingIPInfo) hostStatus {
	if f.MappedTo == "" {
		return hostStatus{Branch: "unmapped", Note: "not yet active (FIP unmapped)"}
	}
	// We don't carry a target_kind on FloatingIPInfo ; the mapped_to
	// payload is the VM/LB name. Heuristic : try VMStatus first ; on
	// error, fall through to the LB hint. Cheaper than a separate
	// "is this a VM ?" probe and matches the way `webui` renders the
	// same row.
	vm, err := c.VMStatus(ctx, &weftv1.VMStatusRequest{Name: f.MappedTo})
	if err != nil || vm == nil || vm.Vm == nil || vm.Vm.Ip == "" {
		// The mapped target either doesn't resolve as a VM on this
		// agent (LB ? remote host ?) or has no IP yet (still
		// provisioning). Both render with the same "we can't derive
		// nftables expectations from here" hint — operators wanting
		// more should query the host directly.
		return hostStatus{
			Branch: "vm-unreachable",
			Note:   fmt.Sprintf("VM not local to any visible host (target=%q)", f.MappedTo),
		}
	}
	return hostStatus{
		Branch: "active",
		VMIP:   vm.Vm.Ip,
		Note: fmt.Sprintf("nftables expected: dnat=%s→%s, snat=%s→%s",
			f.Address, vm.Vm.Ip, vm.Vm.Ip, f.Address),
	}
}

// renderFIPStatus is the human renderer. Keeps the same column
// header order as `show`, then appends a HOST: section so the two
// halves are visually separated. Stays tabwriter-flat ; no boxes,
// no ANSI — operators pipe this into grep/awk constantly.
func renderFIPStatus(f *weftv1.FloatingIPInfo, h hostStatus) error {
	if err := renderFIPsTable([]*weftv1.FloatingIPInfo{f}); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "HOST:")
	fmt.Fprintf(os.Stdout, "  %s\n", h.Note)
	return nil
}

// dumpFIPStatusJSON mirrors dumpFIPsJSON's flat shape and appends a
// "host" object so JSON consumers don't have to splice two calls.
func dumpFIPStatusJSON(f *weftv1.FloatingIPInfo, h hostStatus) error {
	type hostOut struct {
		Branch string `json:"branch"`
		VMIP   string `json:"vm_ip,omitempty"`
		Note   string `json:"note"`
	}
	type out struct {
		UUID        string  `json:"uuid"`
		Address     string  `json:"address"`
		Network     string  `json:"network,omitempty"`
		ProjectUUID string  `json:"project_uuid,omitempty"`
		MappedTo    string  `json:"mapped_to,omitempty"`
		Status      string  `json:"status"`
		AllocatedAt string  `json:"allocated_at"`
		Host        hostOut `json:"host"`
	}
	payload := out{
		UUID:        f.Uuid,
		Address:     f.Address,
		Network:     f.Network,
		ProjectUUID: f.ProjectUuid,
		MappedTo:    f.MappedTo,
		Status:      f.Status,
		AllocatedAt: time.Unix(0, f.AllocatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		Host:        hostOut{Branch: h.Branch, VMIP: h.VMIP, Note: h.Note},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

func allocateCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, network string
	cmd := &cobra.Command{
		Use:   "allocate",
		Short: "Reserve the next-available address from the network's pool",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if network == "" {
				return fmt.Errorf("--network is required")
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.AllocateFloatingIP(context.Background(), &weftv1.AllocateFloatingIPRequest{
				Project: project,
				Network: network,
			})
			if err != nil {
				return err
			}
			f := resp.FloatingIp
			fmt.Printf("allocated\t%s\t%s\t%s\t%s\n", f.Uuid, f.Address, f.Network, f.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Owning project (name or UUID ; empty = caller's default)")
	cmd.Flags().StringVar(&network, "network", "", "Edge network (name or UUID) — required")
	return cmd
}

func releaseCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "release <uuid>",
		Short: "Release a floating IP back to its network's pool",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.ReleaseFloatingIP(context.Background(), &weftv1.ReleaseFloatingIPRequest{Uuid: args[0]}); err != nil {
				return err
			}
			fmt.Println(args[0])
			return nil
		},
	}
}

func mapCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var kind, target string
	cmd := &cobra.Command{
		Use:   "map <uuid>",
		Short: "Attach an allocated FIP to a VM or load-balancer target",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if target == "" {
				return fmt.Errorf("--target is required")
			}
			if kind == "" {
				kind = "vm"
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.MapFloatingIP(context.Background(), &weftv1.MapFloatingIPRequest{
				Uuid:       args[0],
				TargetKind: kind,
				TargetName: target,
			})
			if err != nil {
				return err
			}
			f := resp.FloatingIp
			fmt.Printf("mapped\t%s\t%s\t%s\t%s\n", f.Uuid, f.Address, f.MappedTo, f.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "vm", "Target kind : vm | lb")
	cmd.Flags().StringVar(&target, "target", "", "Target name (VM name or LB name) — required")
	return cmd
}

func unmapCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "unmap <uuid>",
		Short: "Detach a floating IP from its current target",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.UnmapFloatingIP(context.Background(), &weftv1.UnmapFloatingIPRequest{Uuid: args[0]})
			if err != nil {
				return err
			}
			f := resp.FloatingIp
			fmt.Printf("unmapped\t%s\t%s\t%s\n", f.Uuid, f.Address, f.Status)
			return nil
		},
	}
}

func renderFIPsTable(fips []*weftv1.FloatingIPInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tADDRESS\tNETWORK\tPROJECT\tMAPPED_TO\tSTATUS\tALLOCATED")
	for _, f := range fips {
		allocated := time.Unix(0, f.AllocatedAtUnixNs).UTC().Format(time.RFC3339)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			f.Uuid, f.Address, dashIf(f.Network), dashIf(f.ProjectUuid), dashIf(f.MappedTo), f.Status, allocated)
	}
	return tw.Flush()
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dumpFIPsJSON(fips []*weftv1.FloatingIPInfo) error {
	type out struct {
		UUID        string `json:"uuid"`
		Address     string `json:"address"`
		Network     string `json:"network,omitempty"`
		ProjectUUID string `json:"project_uuid,omitempty"`
		MappedTo    string `json:"mapped_to,omitempty"`
		Status      string `json:"status"`
		AllocatedAt string `json:"allocated_at"`
	}
	flat := make([]out, len(fips))
	for i, f := range fips {
		flat[i] = out{
			UUID:        f.Uuid,
			Address:     f.Address,
			Network:     f.Network,
			ProjectUUID: f.ProjectUuid,
			MappedTo:    f.MappedTo,
			Status:      f.Status,
			AllocatedAt: time.Unix(0, f.AllocatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
