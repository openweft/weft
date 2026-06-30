// Package cluster implements `weft cluster status` — the single-
// command health overview an operator hits before doing anything
// else in production. Aggregates the cheap-to-fetch signals (cluster
// name, etcd-quorum hosts, host registry, VM count, plugin count,
// flavor catalogue) into one banner so the operator doesn't chain
// five separate CLIs to know "is the cluster healthy ?".
//
//	weft cluster status                  # tabular banner
//	weft cluster status --format=json    # machine-friendly
//
// The output is small enough to fit a 40-line terminal ; signals
// flagged with ⚠ on degradation (host down, plugin failed, flavor
// catalogue empty) so the operator notices without reading every
// line.
package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// StatusCommand returns the `status` cobra sub-command — exposed
// for the cmd/weft top-level wiring (it mounts onto the existing
// `weft cluster` parent declared in verify_images.go).
func StatusCommand(socket, sshSocket, sshKey *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Cluster health overview (hosts, VMs, plugins, etcd quorum, flavor catalogue)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx := context.Background()
			snap, err := collect(ctx, c)
			if err != nil {
				return err
			}
			if format == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(snap)
			}
			return render(snap)
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

// snapshot is the aggregated cluster-state record. JSON-friendly so
// `weft cluster status --format=json` plugs into operator scripts +
// alerting without re-parsing the table.
type snapshot struct {
	Name           string         `json:"cluster_name"`
	LocalHostUUID  string         `json:"local_host_uuid,omitempty"`
	ControlPlanes  []string       `json:"control_plane_host_uuids,omitempty"`
	Hosts          []hostSummary  `json:"hosts"`
	HostsTotal     int            `json:"hosts_total"`
	HostsActive    int            `json:"hosts_active"`
	HostsConnected int            `json:"hosts_connected"`
	VMs            int            `json:"vms_total"`
	VMsRunning     int            `json:"vms_running"`
	VMsByHost      map[string]int `json:"vms_by_host"`
	Plugins        int            `json:"plugins_installed"`
	Flavors        int            `json:"flavors_in_catalogue"`
	Warnings       []string       `json:"warnings,omitempty"`
}

type hostSummary struct {
	Hostname  string `json:"hostname"`
	UUID      string `json:"uuid"`
	AZ        string `json:"az"`
	Rack      string `json:"rack"`
	State     string `json:"state"`
	Connected bool   `json:"connected"`
	VMCount   int    `json:"vm_count"`
}

// collect fans out the cheap reads in parallel-ish (sequential is
// fine ; the slowest of these is ListHosts which is already <50ms
// on a 6-host cluster). Best-effort : a missing optional surface
// (no Flavor registry, no plugins wired) lands as 0 + a warning
// rather than failing the whole call.
func collect(ctx context.Context, c weftv1.WeftAgentClient) (*snapshot, error) {
	out := &snapshot{VMsByHost: map[string]int{}}

	// Cluster name + control-plane membership.
	if info, err := c.GetClusterInfo(ctx, &weftv1.GetClusterInfoRequest{}); err == nil {
		out.Name = info.ClusterName
		out.LocalHostUUID = info.LocalHostUuid
		out.ControlPlanes = info.ControlPlaneHostUuids
	} else {
		out.Warnings = append(out.Warnings, "GetClusterInfo: "+err.Error())
	}

	// Hosts + connectivity.
	hostsResp, err := c.ListHosts(ctx, &weftv1.ListHostsRequest{})
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	connected := map[string]bool{}
	for _, u := range hostsResp.ConnectedHostUuids {
		connected[u] = true
	}
	for _, h := range hostsResp.Hosts {
		out.Hosts = append(out.Hosts, hostSummary{
			Hostname:  h.Hostname,
			UUID:      h.Uuid,
			AZ:        h.Az,
			Rack:      h.Rack,
			State:     h.State,
			Connected: connected[h.Uuid],
		})
		out.HostsTotal++
		if h.State == "active" {
			out.HostsActive++
		}
		if connected[h.Uuid] {
			out.HostsConnected++
		} else {
			out.Warnings = append(out.Warnings, fmt.Sprintf("host %s not in connected_host_uuids", h.Hostname))
		}
	}

	// VMs + per-host count.
	vmsResp, err := c.ListVMs(ctx, &weftv1.ListVMsRequest{})
	if err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}
	hostnameByUUID := map[string]string{}
	for _, h := range hostsResp.Hosts {
		hostnameByUUID[h.Uuid] = h.Hostname
	}
	for _, v := range vmsResp.Vms {
		out.VMs++
		if v.State == weftv1.VMState_VM_STATE_RUNNING {
			out.VMsRunning++
		}
		key := hostnameByUUID[v.HostUuid]
		if key == "" {
			key = "<unknown>"
		}
		out.VMsByHost[key]++
	}
	// Stamp VMCount on each hostSummary now that we have the count.
	for i := range out.Hosts {
		out.Hosts[i].VMCount = out.VMsByHost[out.Hosts[i].Hostname]
	}

	// Plugins.
	if instResp, perr := c.ListInstalledPlugins(ctx, &weftv1.ListInstalledPluginsRequest{}); perr == nil {
		out.Plugins = len(instResp.Instances)
		for _, p := range instResp.Instances {
			if p.Status == "failed" || p.Status == "degraded" {
				out.Warnings = append(out.Warnings, fmt.Sprintf("plugin %s/%s status=%s", p.Name, p.InstanceUuid[:8], p.Status))
			}
		}
	} else {
		out.Warnings = append(out.Warnings, "ListInstalledPlugins: "+perr.Error())
	}

	// Flavor catalogue.
	if flv, ferr := c.ListFlavors(ctx, &weftv1.ListFlavorsRequest{}); ferr == nil {
		out.Flavors = len(flv.Flavors)
		if out.Flavors == 0 {
			out.Warnings = append(out.Warnings, "flavor catalogue is empty — admission rejects every CreateVM")
		}
	}

	sort.SliceStable(out.Hosts, func(i, j int) bool {
		if out.Hosts[i].AZ != out.Hosts[j].AZ {
			return out.Hosts[i].AZ < out.Hosts[j].AZ
		}
		return out.Hosts[i].Hostname < out.Hosts[j].Hostname
	})
	return out, nil
}

// render prints the tabular banner. Three sections : a top-line
// summary, the per-host table, then any warnings. Designed to fit
// 24 lines on a 6-host cluster so the operator sees everything at
// once without scrolling.
func render(s *snapshot) error {
	bullet := "●"
	if s.Name == "" {
		s.Name = "<unnamed>"
	}
	fmt.Printf("Cluster : %s\n", s.Name)
	fmt.Printf("Hosts   : %d total (%d active, %d connected)\n", s.HostsTotal, s.HostsActive, s.HostsConnected)
	fmt.Printf("VMs     : %d total (%d running)\n", s.VMs, s.VMsRunning)
	fmt.Printf("Plugins : %d installed\n", s.Plugins)
	fmt.Printf("Flavors : %d in catalogue\n", s.Flavors)
	fmt.Println()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  \tHOSTNAME\tAZ\tRACK\tSTATE\tVMS")
	for _, h := range s.Hosts {
		mark := bullet
		if !h.Connected || h.State != "active" {
			mark = "⚠"
		}
		rack := h.Rack
		if rack == "" {
			rack = "—"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%d\n",
			mark, h.Hostname, h.AZ, rack, h.State, h.VMCount)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(s.Warnings) > 0 {
		fmt.Println()
		fmt.Println("Warnings :")
		for _, w := range s.Warnings {
			fmt.Println("  ⚠ " + w)
		}
	}
	// Indicate global health on the last line so a shell scripter can
	// `weft cluster status | tail -1 | grep OK`.
	if len(s.Warnings) == 0 && s.HostsConnected == s.HostsTotal && s.HostsActive == s.HostsTotal {
		fmt.Println("\nOK")
	} else {
		fmt.Println("\nDEGRADED")
	}
	_ = strings.Join // keep linter quiet ; future templates may use it
	return nil
}
