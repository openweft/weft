package cluster

// backup.go owns `weft cluster backup` — the survival-kit etcd
// snapshot command. The cluster state (host registry, VM inventory,
// network/SG/volume records, plugin instances, scheduling rules,
// flavor catalogue, …) all lives in etcd. Losing etcd without a
// snapshot = losing the cluster. This command produces a snapshot
// the operator can stash off-cluster.
//
// Restore intentionally lives outside this command : it requires
// the entire etcd cluster down and is a documented runbook
// (docs/operations/etcd-recovery.md). The CLI prints the restore
// recipe at the end of `backup` so the snapshot lands with its
// matching procedure.
//
// Dials etcd directly (no agent gRPC) because :
//   - the operator runs this from a control-plane host where etcd
//     is local-loopback anyway
//   - a snapshot streaming through the agent's gRPC adds 50+ lines
//     of new proto for zero operator benefit
//   - etcdctl is the universal interface ; this is the in-tree
//     wrapper so the operator doesn't have to install it separately

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// BackupCommand returns the `backup` cobra sub-command.
func BackupCommand() *cobra.Command {
	var (
		endpoints []string
		dest      string
	)
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Snapshot the cluster's etcd state into a file (survival-critical recovery primitive)",
		Long: `weft cluster backup takes a point-in-time snapshot of the etcd
cluster backing this control plane and writes it to a local file.
The snapshot includes every key under the /weft/* prefix : host
registry, VM inventory, network / SG / volume records, plugin
instances, scheduling rules, flavor catalogue, restart counters,
and the etcdcoord lease + election state.

To restore, follow docs/operations/etcd-recovery.md — restoration
requires every etcd member down and a coordinated etcdctl snapshot
restore. The snapshot file format is the standard etcd v3 boltdb
dump, so etcdctl from any 3.5+ release reads it.

  weft cluster backup
  weft cluster backup --to /var/backups/weft-2026-06-30.db
  weft cluster backup --etcd-endpoints dc1-r1-h1:2379,dc2-r1-h1:2379,dc3-r1-h1:2379`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if dest == "" {
				dest = filepath.Join(".", fmt.Sprintf("weft-cluster-%s.db", time.Now().UTC().Format("2006-01-02T15-04-05Z")))
			}
			cli, err := dialEtcdForBackup(endpoints)
			if err != nil {
				return err
			}
			defer cli.Close()

			fmt.Fprintf(os.Stderr, "weft cluster backup : streaming snapshot to %s …\n", dest)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			rd, err := cli.Snapshot(ctx)
			if err != nil {
				return fmt.Errorf("etcd Snapshot: %w", err)
			}
			defer rd.Close()
			out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
			if err != nil {
				return fmt.Errorf("create %s: %w", dest, err)
			}
			n, err := io.Copy(out, rd)
			if cerr := out.Close(); cerr != nil && err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("write snapshot: %w", err)
			}
			fmt.Fprintf(os.Stderr, "weft cluster backup : wrote %s (%d bytes)\n", dest, n)
			fmt.Fprintln(os.Stderr, "\nTo restore later (requires the entire etcd cluster DOWN) :")
			fmt.Fprintf(os.Stderr, "  1. systemctl stop weft-agent etcd      # on every host\n")
			fmt.Fprintf(os.Stderr, "  2. etcdctl snapshot restore %s \\\n", dest)
			fmt.Fprintf(os.Stderr, "        --name <member-name> --initial-cluster <name1=peer1,…> \\\n")
			fmt.Fprintf(os.Stderr, "        --initial-advertise-peer-urls <peer-url>\n")
			fmt.Fprintf(os.Stderr, "  3. systemctl start etcd weft-agent      # on every host\n")
			fmt.Fprintln(os.Stderr, "  4. weft cluster status                  # verify quorum + inventory")
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&endpoints, "etcd-endpoints", nil,
		"etcd client endpoints (default: $WEFT_ETCD_ENDPOINTS, then 127.0.0.1:2379)")
	cmd.Flags().StringVar(&dest, "to", "",
		"destination snapshot file (default: ./weft-cluster-<UTC>.db)")
	return cmd
}

// dialEtcdForBackup mirrors the monitor cmd's pattern : default to
// $WEFT_ETCD_ENDPOINTS, then localhost:2379. Backup runs from a CP
// host where one of these reaches the local member. Multi-endpoint
// arg supports the cross-DC operator who wants to take the snapshot
// from a follower (less impact on leader latency under load).
func dialEtcdForBackup(endpoints []string) (*clientv3.Client, error) {
	if len(endpoints) == 0 {
		if env := os.Getenv("WEFT_ETCD_ENDPOINTS"); env != "" {
			for _, ep := range strings.Split(env, ",") {
				ep = strings.TrimSpace(ep)
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
