package proxy

import (
	"os"
	"strings"
)

// StorageEtcdEnvKey is the env var an operator sets to opt into the etcd-
// backed certificate storage. Value: comma-separated etcd endpoints
// (`http://10.0.0.11:2379,http://10.0.0.12:2379,http://10.0.0.13:2379`).
// When unset, Caddy falls back to filesystem storage rooted at the
// supervisor's StateDir — fine for single-host dev, a coordination tax in
// 3-DC where every host re-mints its own certs on first reload.
const StorageEtcdEnvKey = "WEFT_PROXY_STORAGE_ETCD_ENDPOINTS"

// EtcdStorageEndpoints returns the operator-configured endpoints. Empty
// slice = use the filesystem default. Trims each entry to be tolerant of
// trailing spaces and stray commas.
func EtcdStorageEndpoints() []string {
	v := strings.TrimSpace(os.Getenv(StorageEtcdEnvKey))
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// The Caddy JSON `storage` block for the etcd-backed adapter now
// lives in etcdstorage.go (`storageEtcdInTreeConfig`), which wraps
// the in-tree `EtcdStorage` and registers as the `weft_etcd` Caddy
// storage module via the xcaddy build at
// `github.com/openweft/weft-proxy`. The darkweak `etcd` adapter is
// no longer the target — keeping the cert-store implementation
// in-tree means weft owns its key layout (`/weft/proxy/caddy/`),
// lock semantics, and rate-limit-safe ACME orchestration.
