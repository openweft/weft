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
//	weft host set-properties <uuid> k=v[,k=v…]                     replace properties (aliased "set-labels")
//	weft host rm <uuid>                                            delete (operator-managed drain first)
package host

import (
	"bufio"
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
capability lists (network types, volume backends, properties) — the
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
		setPropertiesCmd(socket, sshSocket, sshKey),
		cordonCmd(socket, sshSocket, sshKey),
		uncordonCmd(socket, sshSocket, sshKey),
		drainCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

// drainCmd is the production-survival convenience : sets the host's
// state to "draining" (the scheduler stops admitting new placements)
// AND lists every VM currently owned by the host so the operator
// knows what still needs to be migrated / deleted before a reboot
// or decommission. Idempotent — re-running on an already-draining
// host re-prints the inventory without an error.
//
// --evict deletes every VM marked `deployment.type=ci` (disposable
// build runners) up-front so the operator only has to think about
// the HA workloads. HA VMs are never auto-deleted ; the operator
// migrates them via the per-plugin runbook (etcd-Patroni-style
// failover for postgres-ha, simple delete+respawn for stateless,
// etc.).
//
//	weft host drain dc2-r1-h1
//	weft host drain dc2-r1-h1 --evict        # also drop ci VMs
//	weft host drain dc2-r1-h1 --force        # skip the prompt
func drainCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var (
		byHostname bool
		evict      bool
		force      bool
	)
	cmd := &cobra.Command{
		Use:   "drain <uuid|hostname>",
		Short: "Set a host to draining (no new placements) + list VMs still on it",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			uuid := args[0]
			hostname := args[0]
			if !byHostname && looksLikeUUID(args[0]) {
				resp, err := c.GetHost(context.Background(), &weftv1.GetHostRequest{Uuid: args[0]})
				if err == nil {
					hostname = resp.Host.Hostname
				}
			} else {
				resp, err := c.GetHost(context.Background(), &weftv1.GetHostRequest{Hostname: args[0]})
				if err != nil {
					return fmt.Errorf("GetHost %q: %w", args[0], err)
				}
				uuid = resp.Host.Uuid
				hostname = resp.Host.Hostname
			}
			if !force {
				fmt.Fprintf(os.Stderr, "About to drain host %s (uuid=%s). Type yes to confirm : ", hostname, uuid)
				reader := bufio.NewReader(os.Stdin)
				line, _ := reader.ReadString('\n')
				if strings.TrimSpace(strings.ToLower(line)) != "yes" {
					return fmt.Errorf("aborted")
				}
			}
			if _, err := c.SetHostState(context.Background(), &weftv1.SetHostStateRequest{Uuid: uuid, State: "draining"}); err != nil {
				return fmt.Errorf("SetHostState draining: %w", err)
			}
			fmt.Printf("host %s state -> draining\n", hostname)

			// Walk the VM inventory + report (or evict) what's still on this host.
			vms, err := c.ListVMs(context.Background(), &weftv1.ListVMsRequest{})
			if err != nil {
				return fmt.Errorf("ListVMs: %w", err)
			}
			var remaining []*weftv1.VMInfo
			var evicted []string
			for _, v := range vms.Vms {
				if v.HostUuid != uuid {
					continue
				}
				if evict && v.Properties["deployment.type"] == "ci" {
					if _, derr := c.DeleteVM(context.Background(), &weftv1.DeleteVMRequest{
						Name: v.Name, Project: v.Project, HostUuid: uuid,
					}); derr != nil {
						fmt.Fprintf(os.Stderr, "evict %s : %v\n", v.Name, derr)
					} else {
						evicted = append(evicted, v.Name)
					}
					continue
				}
				remaining = append(remaining, v)
			}
			if len(evicted) > 0 {
				fmt.Printf("\nEvicted %d CI VM(s) :\n", len(evicted))
				for _, n := range evicted {
					fmt.Printf("  - %s\n", n)
				}
			}
			if len(remaining) == 0 {
				fmt.Println("\nHost is empty. Safe to reboot / decommission.")
				return nil
			}
			fmt.Printf("\n%d VM(s) still on %s — migrate or delete before reboot :\n", len(remaining), hostname)
			for _, v := range remaining {
				dep := v.Properties["deployment.type"]
				if dep == "" {
					dep = "-"
				}
				fmt.Printf("  - %-40s project=%s deployment=%s\n", v.Name, v.Project, dep)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&byHostname, "by-hostname", false, "Treat the positional arg as a hostname (default: auto-detect UUID vs hostname)")
	cmd.Flags().BoolVar(&evict, "evict", false, "Delete deployment.type=ci VMs on the host (disposable workloads only)")
	cmd.Flags().BoolVar(&force, "force", false, "Skip the confirmation prompt")
	return cmd
}

// looksLikeUUID is a cheap heuristic to disambiguate the positional
// arg without forcing the operator to flag --by-hostname every time :
// 36 chars + a few dashes = treat as UUID, otherwise hostname.
func looksLikeUUID(s string) bool {
	return len(s) == 36 && strings.Count(s, "-") == 4
}

// cordonCmd flips a host's `cordoned` flag on — the scheduler stops
// considering it for new placements, existing VMs keep running.
// Idempotent : calling on an already-cordoned host succeeds without
// emitting a duplicate event. Implements the upgrade-runbook
// primitive previously documented as proposed.
func cordonCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var byHostname bool
	cmd := &cobra.Command{
		Use:   "cordon <uuid|hostname>",
		Short: "Stop scheduling new VMs onto a host (existing VMs keep running)",
		Long: `Set the host's "cordoned" flag — the scheduler immediately drops
it from candidate sets for new placements. Existing VMs stay put,
the host stays Active + reachable. Pair with "weft host uncordon"
to reverse.

Typical use is the rolling-upgrade workflow (see
docs/operations/upgrade.md) : cordon the host, drain workloads,
restart the agent, uncordon.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runCordon(*socket, *sshSocket, *sshKey, args[0], byHostname, true)
		},
	}
	cmd.Flags().BoolVar(&byHostname, "by-hostname", false, "Treat the positional arg as a hostname (default: UUID)")
	return cmd
}

// uncordonCmd reverses cordonCmd. Idempotent on already-uncordoned
// hosts.
func uncordonCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var byHostname bool
	cmd := &cobra.Command{
		Use:   "uncordon <uuid|hostname>",
		Short: "Re-enable scheduling onto a previously cordoned host",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runCordon(*socket, *sshSocket, *sshKey, args[0], byHostname, false)
		},
	}
	cmd.Flags().BoolVar(&byHostname, "by-hostname", false, "Treat the positional arg as a hostname (default: UUID)")
	return cmd
}

// runCordon resolves <uuid|hostname> → UUID and issues SetHostCordoned.
// Lifted into a helper so cordon + uncordon share the resolve path
// (the registry side keys on UUID, but operators write hostnames
// far more often).
func runCordon(socket, sshSocket, sshKey, ident string, byHostname, cordoned bool) error {
	c, conn, err := shared.Client(socket, sshSocket, sshKey)
	if err != nil {
		return err
	}
	defer conn.Close()
	uuid := ident
	if byHostname {
		resp, err := c.GetHost(context.Background(), &weftv1.GetHostRequest{Hostname: ident})
		if err != nil {
			return fmt.Errorf("GetHost: %w", err)
		}
		uuid = resp.Host.Uuid
	}
	_, err = c.SetHostCordoned(context.Background(), &weftv1.SetHostCordonedRequest{Uuid: uuid, Cordoned: cordoned})
	if err != nil {
		return fmt.Errorf("SetHostCordoned: %w", err)
	}
	verb := "uncordoned"
	if cordoned {
		verb = "cordoned"
	}
	fmt.Printf("host %s %s\n", ident, verb)
	return nil
}

func registerCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var hostname, az, rack, endpoint, hypervisor, architecture, uuid string
	var propertiesRaw, labelsRaw string
	var networkTypes, volumeBackends []string
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register a host (or refresh its metadata; idempotent by UUID)",
		Long: `Register a hypervisor instance in the Host registry. Idempotent: if
the UUID already exists, the entry is refreshed (AZ/rack/properties/etc.
overwritten ; CreatedAt preserved). Without --uuid the server mints
a fresh one.

The rack tag carves a sub-AZ failure domain — set it on every host
in multi-rack clusters so the scheduler's "rack: different" rule can
actually succeed.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if hostname == "" {
				return fmt.Errorf("--hostname is required")
			}
			// Deprecation seam : --labels was renamed to --properties. Honour
			// the legacy flag when set, but print a stderr notice so scripts
			// migrate. --properties wins if both are supplied.
			raw := propertiesRaw
			if raw == "" && labelsRaw != "" {
				fmt.Fprintln(os.Stderr, "weft host register: --labels is deprecated, use --properties")
				raw = labelsRaw
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
			if raw != "" {
				properties, err := parseProperties(raw)
				if err != nil {
					return err
				}
				req.Properties = properties
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
	cmd.Flags().StringVar(&propertiesRaw, "properties", "", "Comma-separated k=v property pairs (e.g. gpu=h100,zone=secure)")
	cmd.Flags().StringVar(&labelsRaw, "labels", "", "Deprecated alias for --properties")
	_ = cmd.Flags().MarkHidden("labels")
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var azFilter, format string
	cmd := &cobra.Command{
		Use:   "ls",
		Aliases: []string{"list"},
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

func setPropertiesCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:     "set-properties <uuid> k=v[,k=v…]",
		Aliases: []string{"set-labels"},
		Short:   "Replace a host's properties atomically (pass empty to clear)",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.CalledAs() == "set-labels" {
				fmt.Fprintln(os.Stderr, "weft host set-labels: deprecated, use 'set-properties'")
			}
			properties, err := parseProperties(args[1])
			if err != nil {
				return err
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = c.SetHostProperties(context.Background(), &weftv1.SetHostPropertiesRequest{Uuid: args[0], Properties: properties})
			if err != nil {
				return fmt.Errorf("SetHostProperties: %w", err)
			}
			fmt.Printf("host %s properties updated (%d entries)\n", args[0], len(properties))
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

// parseProperties accepts "k=v,k=v,..." (or empty for no properties).
// Whitespace around keys/values is trimmed.
func parseProperties(raw string) (map[string]string, error) {
	out := map[string]string{}
	if raw == "" {
		return out, nil
	}
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("property %q is not k=v shape", pair)
		}
		k := strings.TrimSpace(kv[0])
		v := strings.TrimSpace(kv[1])
		if k == "" {
			return nil, fmt.Errorf("empty property key in %q", pair)
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
