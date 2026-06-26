package plugin

// catalogue.go — operator-facing CLI to manage the etcd-hosted plugin
// catalogue. Matches the [openweft etcd embedded] design : cluster
// state (catalogues, dynamic config) lives in etcd so every agent in
// the fleet sees the same view without per-host rsync.
//
// Subcommands :
//
//   weft plugin catalogue sync [--catalogue DIR] [--etcd URL]
//       Read every plugin.hcl under DIR and publish the bytes to
//       etcd under /weft/catalogue/<name>. Idempotent (etcd Put with
//       the same value is a no-op-revision bump). Operator runs this
//       once after `weft up` to seed the catalogue across the
//       cluster, or whenever a new plugin lands in catalogue/.
//
//   weft plugin catalogue ls [--etcd URL]
//       Show what's currently in etcd. Handy for verifying a sync
//       landed without spinning up the agent / TUI.
//
// The etcd URL defaults to $WEFT_ETCD_URL or http://127.0.0.1:2379
// (single-host dev). Multi-endpoint via comma-separated list.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openweft/weft/pluginstore"
	"github.com/spf13/cobra"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// catalogueCmd is the `weft plugin catalogue` subcommand tree.
func catalogueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalogue",
		Short: "Manage the etcd-hosted plugin catalogue",
		Long: `The plugin catalogue ships as HCL manifests under catalogue/<name>/
plugin.hcl in the source tree. At runtime every agent in the cluster
reads the catalogue from etcd (cf. openweft etcd embedded design),
so manifests need to be pushed there once after bring-up.

  sync   read the local catalogue/ tree and publish it to etcd
  ls     list what's currently in etcd`,
	}
	cmd.AddCommand(catalogueSyncCmd(), catalogueListCmd())
	return cmd
}

func catalogueSyncCmd() *cobra.Command {
	var catalogueDir string
	var etcdEndpoints string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Publish the local catalogue/ tree to etcd",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			root := resolveCatalogue(catalogueDir)
			manifests, err := readManifestBytes(root)
			if err != nil {
				return err
			}
			if len(manifests) == 0 {
				return fmt.Errorf("no plugin.hcl files found under %s", root)
			}
			cli, err := dialEtcd(etcdEndpoints)
			if err != nil {
				return err
			}
			defer cli.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := pluginstore.WriteManifestsToEtcd(ctx, cli, "", manifests); err != nil {
				return err
			}
			names := make([]string, 0, len(manifests))
			for n := range manifests {
				names = append(names, n)
			}
			sort.Strings(names)
			fmt.Printf("synced %d plugin(s) to etcd %s%s\n",
				len(manifests), strings.Join(cli.Endpoints(), ","), pluginstore.EtcdCataloguePrefix)
			for _, n := range names {
				fmt.Println("  -", n)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&catalogueDir, "catalogue", "", "Catalogue root directory (default: $WEFT_CATALOGUE_DIR or ./catalogue)")
	cmd.Flags().StringVar(&etcdEndpoints, "etcd", "", "Comma-separated etcd endpoints (default: $WEFT_ETCD_URL or http://127.0.0.1:2379)")
	return cmd
}

func catalogueListCmd() *cobra.Command {
	var etcdEndpoints string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List plugins currently published to etcd",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cli, err := dialEtcd(etcdEndpoints)
			if err != nil {
				return err
			}
			defer cli.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cat, err := pluginstore.LoadCatalogueFromEtcd(ctx, cli, "")
			if err != nil {
				return err
			}
			if len(cat) == 0 {
				fmt.Println("(no plugins in etcd ; run `weft plugin catalogue sync`)")
				return nil
			}
			names := make([]string, 0, len(cat))
			for n := range cat {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				m := cat[n]
				fmt.Printf("%-22s %-10s %-22s %s\n", m.Name, m.Version, m.Kind, m.Description)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&etcdEndpoints, "etcd", "", "Comma-separated etcd endpoints (default: $WEFT_ETCD_URL or http://127.0.0.1:2379)")
	return cmd
}

// readManifestBytes walks the catalogue tree and returns the raw
// plugin.hcl bytes per plugin name. Mirrors pluginstore.LoadCatalogue's
// directory layout (one subdir per plugin, plugin.hcl inside) but
// keeps the bytes raw so WriteManifestsToEtcd stores exactly what's
// committed in git — no parser round-trip drift.
func readManifestBytes(root string) (map[string][]byte, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read catalogue root %s: %w", root, err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), "plugin.hcl")
		data, err := os.ReadFile(path)
		if err != nil {
			// Skip subdirs without plugin.hcl (docs / scratch) —
			// LoadCatalogue treats them the same way.
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		out[e.Name()] = data
	}
	return out, nil
}

// dialEtcd resolves the endpoints flag → $WEFT_ETCD_URL → single-host
// default, then opens a clientv3.Client with a short dial timeout so
// `weft plugin catalogue sync` fails fast on a wrong host instead of
// hanging.
func dialEtcd(endpointsFlag string) (*clientv3.Client, error) {
	endpoints := endpointsFlag
	if endpoints == "" {
		endpoints = os.Getenv("WEFT_ETCD_URL")
	}
	if endpoints == "" {
		endpoints = "http://127.0.0.1:2379"
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(endpoints, ","),
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("etcd dial %s: %w", endpoints, err)
	}
	return cli, nil
}
