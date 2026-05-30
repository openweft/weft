// Package host implements the `weft host` CLI subcommand group :
// CRUD over the platform's Host registry. The Host inventory
// drives multi-host placement (per [[weft-placement-rules]]) —
// operators register each hypervisor instance and the scheduler
// picks among them honouring the per-replica PlacementRule.
//
//	weft host register --hostname <h> [--az <az>] [--rack <r>] …  register a host
//	weft host ls [--az <az>]                                       list hosts
//	weft host show <uuid|hostname>                                 fetch one host
//	weft host set-state <uuid> active|draining|down                gate scheduling
//	weft host set-labels <uuid> k=v[,k=v…]                         replace labels
//	weft host rm <uuid>                                            delete (operator-managed drain first)
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft host` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Manage the Host inventory (hypervisor instances scheduled against)",
		Long: `The Host registry tracks every hypervisor instance in the cluster.
Each entry carries its AZ, rack, hypervisor kind, architecture, and
capability lists (network types, volume backends, labels) — the
scheduler consumes these when honouring placement rules from infra
plans or VM CreateSpecs.

Hosts self-register through the per-host weft agent ; operators can
also register them explicitly with "weft host register".`,
	}
	cmd.AddCommand(
		registerCmd(socket, sshSocket, sshKey),
		lsCmd(socket, sshSocket, sshKey),
		showCmd(socket, sshSocket, sshKey),
		setStateCmd(socket, sshSocket, sshKey),
		setLabelsCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func registerCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var hostname, az, rack, endpoint, hypervisor, architecture, uuid string
	var labelsRaw string
	var networkTypes, volumeBackends []string
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register a host (or refresh its metadata; idempotent by UUID)",
		Long: `Register a hypervisor instance in the Host registry. Idempotent: if
the UUID already exists, the entry is refreshed (AZ/rack/labels/etc.
overwritten ; CreatedAt preserved). Without --uuid the server mints
a fresh one.

The rack tag carves a sub-AZ failure domain — set it on every host
in multi-rack clusters so the scheduler's "rack: different" rule can
actually succeed.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if hostname == "" {
				return fmt.Errorf("--hostname is required")
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			req := &weftv1.RegisterHostRequest{
				Uuid:           uuid,
				Hostname:       hostname,
				Az:             az,
				Rack:           rack,
				Endpoint:       endpoint,
				Hypervisor:     hypervisor,
				Architecture:   architecture,
				NetworkTypes:   networkTypes,
				VolumeBackends: volumeBackends,
			}
			if labelsRaw != "" {
				labels, err := parseLabels(labelsRaw)
				if err != nil {
					return err
				}
				req.Labels = labels
			}
			resp, err := c.RegisterHost(context.Background(), req)
			if err != nil {
				return fmt.Errorf("RegisterHost: %w", err)
			}
			fmt.Printf("registered host %s (%s) in az=%q rack=%q\n", resp.Host.Hostname, resp.Host.Uuid, resp.Host.Az, resp.Host.Rack)
			return nil
		},
	}
	cmd.Flags().StringVar(&uuid, "uuid", "", "Stable host UUID (empty = mint a new one)")
	cmd.Flags().StringVar(&hostname, "hostname", "", "Hostname (required, must be unique cluster-wide)")
	cmd.Flags().StringVar(&az, "az", "", "Availability zone (e.g. dc1)")
	cmd.Flags().StringVar(&rack, "rack", "", "Sub-AZ failure domain (ToR switch / PDU)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "host:port of the agent's gRPC listener (multi-host deployments)")
	cmd.Flags().StringVar(&hypervisor, "hypervisor", "", "Hypervisor kind: apple-vz | qemu-kvm | …")
	cmd.Flags().StringVar(&architecture, "architecture", "", "arm64 | amd64 | riscv64 | loongarch64")
	cmd.Flags().StringSliceVar(&networkTypes, "network-types", nil, "Supported network types (comma-separated): nat,bridged,mesh,…")
	cmd.Flags().StringSliceVar(&volumeBackends, "volume-backends", nil, "Supported volume backends (comma-separated): file,ceph,…")
	cmd.Flags().StringVar(&labelsRaw, "labels", "", "Comma-separated k=v label pairs (e.g. gpu=h100,zone=secure)")
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var azFilter, format string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List hosts (optionally filtered by --az)",
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListHosts(context.Background(), &weftv1.ListHostsRequest{Az: azFilter})
			if err != nil {
				return fmt.Errorf("ListHosts: %w", err)
			}
			if format == "json" {
				return jsonDump(resp)
			}
			connected := make(map[string]bool, len(resp.ConnectedHostUuids))
			for _, uuid := range resp.ConnectedHostUuids {
				connected[uuid] = true
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "UUID\tHOSTNAME\tAZ\tRACK\tHYP\tARCH\tSTATE\tCONNECTED\tLAST-SEEN")
			for _, h := range resp.Hosts {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					h.Uuid, h.Hostname, dashIf(h.Az), dashIf(h.Rack),
					dashIf(h.Hypervisor), dashIf(h.Architecture),
					dashIf(h.State), connectedFlag(connected[h.Uuid]),
					formatTime(h.LastSeenAtUnixNs))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&azFilter, "az", "", "Only show hosts in this AZ")
	cmd.Flags().StringVar(&format, "format", "", "Output format: empty (table) or json")
	return cmd
}

func showCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var byHostname bool
	cmd := &cobra.Command{
		Use:   "show <uuid|hostname>",
		Short: "Show one host's full details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			req := &weftv1.GetHostRequest{}
			if byHostname {
				req.Hostname = args[0]
			} else {
				req.Uuid = args[0]
			}
			resp, err := c.GetHost(context.Background(), req)
			if err != nil {
				return fmt.Errorf("GetHost: %w", err)
			}
			return jsonDump(resp.Host)
		},
	}
	cmd.Flags().BoolVar(&byHostname, "by-hostname", false, "Treat the positional arg as a hostname (default: UUID)")
	return cmd
}

func setStateCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "set-state <uuid> <active|draining|down>",
		Short: "Transition a host's state",
		Long: `Active   — accepts new VM placements.
Draining — no new placements; existing VMs keep running (operator drain).
Down     — heartbeat aged past TTL OR explicit operator decommission.`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = c.SetHostState(context.Background(), &weftv1.SetHostStateRequest{Uuid: args[0], State: args[1]})
			if err != nil {
				return fmt.Errorf("SetHostState: %w", err)
			}
			fmt.Printf("host %s state -> %s\n", args[0], args[1])
			return nil
		},
	}
}

func setLabelsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "set-labels <uuid> k=v[,k=v…]",
		Short: "Replace a host's labels atomically (pass empty to clear)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			labels, err := parseLabels(args[1])
			if err != nil {
				return err
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = c.SetHostLabels(context.Background(), &weftv1.SetHostLabelsRequest{Uuid: args[0], Labels: labels})
			if err != nil {
				return fmt.Errorf("SetHostLabels: %w", err)
			}
			fmt.Printf("host %s labels updated (%d entries)\n", args[0], len(labels))
			return nil
		},
	}
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <uuid>",
		Short: "Remove a host from the registry (does not stop its VMs — drain first)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = c.DeleteHost(context.Background(), &weftv1.DeleteHostRequest{Uuid: args[0]})
			if err != nil {
				return fmt.Errorf("DeleteHost: %w", err)
			}
			fmt.Printf("host %s removed\n", args[0])
			return nil
		},
	}
}

// parseLabels accepts "k=v,k=v,..." (or empty for no labels).
// Whitespace around keys/values is trimmed.
func parseLabels(raw string) (map[string]string, error) {
	out := map[string]string{}
	if raw == "" {
		return out, nil
	}
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("label %q is not k=v shape", pair)
		}
		k := strings.TrimSpace(kv[0])
		v := strings.TrimSpace(kv[1])
		if k == "" {
			return nil, fmt.Errorf("empty label key in %q", pair)
		}
		out[k] = v
	}
	return out, nil
}

func dashIf(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func connectedFlag(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func formatTime(unixNs int64) string {
	if unixNs == 0 {
		return "—"
	}
	return time.Unix(0, unixNs).Format(time.RFC3339)
}

func jsonDump(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
