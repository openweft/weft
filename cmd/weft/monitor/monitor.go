// Package monitor implements the `weft monitor` CLI subcommand
// group : visibility into the etcd-coord liveness leases that
// drive cross-host respawn failover (per [[respawn-v013-true-ha]]).
//
// One weft agent runs one monitor ; the number of live monitors
// equals the number of healthy weft-agent processes holding a
// non-expired lease at /weft/coord/hosts/<host_uuid>. A sudden
// drop signals a DC partition or rack outage.
//
//	weft monitor ls [--etcd-endpoints …]
//	weft monitor watch [--etcd-endpoints …]                       follow UP/DOWN events live
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/openweft/weft/etcdcoord"
	"github.com/spf13/cobra"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Command returns the `weft monitor` cobra group.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "monitor",
		Aliases: []string{"monitors"},
		Short:   "Inspect weft-agent monitors (HA respawn liveness)",
		Long: `weft monitor surfaces the etcd-coord liveness leases that drive
the cross-host respawn HA loop. One weft agent = one monitor ; the
count equals the number of healthy agents reachable from etcd.

A monitor's lease has a 10s TTL refreshed every 3s. When the lease
expires (process crash, network partition, kernel panic), surviving
monitors observe the DOWN event and a leader election decides which
one claims the dead host's VMs.`,
	}
	cmd.AddCommand(lsCmd(), watchCmd())
	return cmd
}

func lsCmd() *cobra.Command {
	var endpoints []string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List live monitors (etcd-coord liveness leases)",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cli, err := dialEtcd(endpoints)
			if err != nil {
				return err
			}
			defer cli.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			monitors, err := fetchMonitors(ctx, cli)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "HOST_UUID\tHOSTNAME\tHYPERVISOR\tSTARTED_AT\tUPTIME")
			now := time.Now()
			for _, m := range monitors {
				started := time.Unix(0, m.StartedAt)
				uptime := now.Sub(started).Round(time.Second)
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					m.HostUUID, dashIf(m.Hostname), dashIf(m.Hypervisor),
					started.UTC().Format(time.RFC3339), uptime,
				)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Printf("\n%d live monitor(s)\n", len(monitors))
			return nil
		},
	}
	addEtcdFlags(cmd, &endpoints)
	return cmd
}

func watchCmd() *cobra.Command {
	var endpoints []string
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Stream monitor UP/DOWN events from etcd until Ctrl-C",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cli, err := dialEtcd(endpoints)
			if err != nil {
				return err
			}
			defer cli.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w, err := etcdcoord.NewHostWatcher(ctx, cli, etcdcoord.WatcherOptions{})
			if err != nil {
				return err
			}
			fmt.Println("watching /weft/coord/hosts/ — Ctrl-C to exit")
			for ev := range w.Events() {
				kind := "UP"
				if ev.Kind == etcdcoord.HostDown {
					kind = "DOWN"
				}
				name := ev.Metadata.Hostname
				if name == "" {
					name = "(unknown)"
				}
				fmt.Printf("%s\t%s\t%s\t%s\n",
					time.Now().UTC().Format(time.RFC3339), kind, ev.HostUUID, name,
				)
			}
			return nil
		},
	}
	addEtcdFlags(cmd, &endpoints)
	return cmd
}

func addEtcdFlags(cmd *cobra.Command, endpoints *[]string) {
	cmd.Flags().StringSliceVar(endpoints, "etcd-endpoints", nil,
		"etcd client endpoints (default: $WEFT_ETCD_ENDPOINTS, then 127.0.0.1:2379)")
}

func dialEtcd(endpoints []string) (*clientv3.Client, error) {
	if len(endpoints) == 0 {
		if env := os.Getenv("WEFT_ETCD_ENDPOINTS"); env != "" {
			// Cheap split — operators routinely paste comma-separated lists.
			for _, ep := range splitComma(env) {
				if ep != "" {
					endpoints = append(endpoints, ep)
				}
			}
		}
	}
	if len(endpoints) == 0 {
		endpoints = []string{"127.0.0.1:2379"}
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("etcd dial %v: %w", endpoints, err)
	}
	return cli, nil
}

func fetchMonitors(ctx context.Context, cli *clientv3.Client) ([]etcdcoord.HostMetadata, error) {
	resp, err := cli.Get(ctx, etcdcoord.HostsPrefix, clientv3.WithPrefix(), clientv3.WithSerializable())
	if err != nil {
		return nil, fmt.Errorf("etcd get %s: %w", etcdcoord.HostsPrefix, err)
	}
	out := make([]etcdcoord.HostMetadata, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var meta etcdcoord.HostMetadata
		if err := json.Unmarshal(kv.Value, &meta); err != nil {
			// Trust the key as a fallback : the lease still proves
			// the host is up even if the value is corrupted.
			meta.HostUUID = path.Base(string(kv.Key))
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HostUUID < out[j].HostUUID })
	return out, nil
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
