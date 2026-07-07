package pluginstore

// etcd_catalogue.go : alternative catalogue source that reads + writes
// plugin manifests under an etcd key prefix instead of the local disk
// tree. Matches the [openweft etcd embedded] design — cluster state
// lives in etcd so every agent in the 3-DC fleet sees the same
// catalogue without per-host rsync.
//
// Layout :
//
//   <prefix><plugin-name>  →  raw plugin.hcl bytes
//
// Default prefix is "/weft/catalogue/" (matches the rest of the
// /weft/* key space the cluster uses). The HCL bytes round-trip
// through ParseManifest, so the wire format is exactly what an
// operator commits to git — no schema drift between disk and etcd.

import (
	"context"
	"fmt"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// EtcdCataloguePrefix is the etcd key prefix the plugin catalogue
// keys live under. Caller can override via WriteManifestsToEtcd /
// LoadCatalogueFromEtcd's prefix argument when running multiple
// fleets against a single etcd cluster.
const EtcdCataloguePrefix = "/weft/catalogue/"

// LoadCatalogueFromEtcd reads every key under prefix and parses each
// value as a plugin.hcl manifest. Returns name → Manifest. An empty
// catalogue (no keys under prefix) returns an empty map + nil error
// so callers can distinguish "etcd is up, catalogue is empty" from
// "etcd is unreachable". Pass "" to use EtcdCataloguePrefix.
func LoadCatalogueFromEtcd(ctx context.Context, cli *clientv3.Client, prefix string) (map[string]*Manifest, error) {
	if cli == nil {
		return nil, fmt.Errorf("pluginstore: nil etcd client")
	}
	if prefix == "" {
		prefix = EtcdCataloguePrefix
	}
	gctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := cli.Get(gctx, prefix, clientv3.WithPrefix(), clientv3.WithSerializable())
	if err != nil {
		return nil, fmt.Errorf("pluginstore: etcd Get %s: %w", prefix, err)
	}
	out := make(map[string]*Manifest, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		key := string(kv.Key)
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		name := strings.TrimPrefix(key, prefix)
		// Guard against accidentally storing a manifest under a
		// nested path : the catalogue is flat by design.
		if strings.Contains(name, "/") {
			continue
		}
		m, err := ParseManifest(name+".hcl", kv.Value)
		if err != nil {
			return nil, fmt.Errorf("pluginstore: parse manifest %s: %w", key, err)
		}
		if _, dup := out[m.Name]; dup {
			return nil, fmt.Errorf("pluginstore: duplicate plugin %q at %s", m.Name, key)
		}
		out[m.Name] = m
	}
	return out, nil
}

// WriteManifestsToEtcd publishes a set of raw plugin.hcl payloads
// under prefix. Used by the bootstrap CLI (`weft catalogue sync`)
// when seeding etcd from the on-disk catalogue tree, and by future
// "publish a new plugin" workflows. Caller is responsible for
// loading the bytes (e.g. from disk) — this helper only writes.
//
// Keys whose values are byte-identical to what's already in etcd
// are still re-Put : etcd is cheap, and a no-op-write helps a
// downstream Watch fire (operators rely on a fresh revision to
// trigger a catalogue reload).
//
// Pass "" to use EtcdCataloguePrefix.
func WriteManifestsToEtcd(ctx context.Context, cli *clientv3.Client, prefix string, manifests map[string][]byte) error {
	if cli == nil {
		return fmt.Errorf("pluginstore: nil etcd client")
	}
	if prefix == "" {
		prefix = EtcdCataloguePrefix
	}
	for name, hclBytes := range manifests {
		if strings.Contains(name, "/") {
			return fmt.Errorf("pluginstore: invalid plugin name %q (path separator not allowed)", name)
		}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := cli.Put(pctx, prefix+name, string(hclBytes))
		cancel()
		if err != nil {
			return fmt.Errorf("pluginstore: etcd Put %s: %w", name, err)
		}
	}
	return nil
}

// DeleteManifestFromEtcd removes a single plugin from the etcd
// catalogue. Operators reach for this when retiring an HA topology ;
// the manifest stays in the git tree so it can be reactivated later.
// Pass "" to use EtcdCataloguePrefix.
func DeleteManifestFromEtcd(ctx context.Context, cli *clientv3.Client, prefix, name string) error {
	if cli == nil {
		return fmt.Errorf("pluginstore: nil etcd client")
	}
	if name == "" {
		return fmt.Errorf("pluginstore: empty plugin name")
	}
	if prefix == "" {
		prefix = EtcdCataloguePrefix
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := cli.Delete(dctx, prefix+name); err != nil {
		return fmt.Errorf("pluginstore: etcd Delete %s: %w", name, err)
	}
	return nil
}
