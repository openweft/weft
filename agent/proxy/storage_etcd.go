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

// storageEtcdConfig builds the Caddy JSON `storage` block for etcd. It is
// only consumed when the Caddy binary the supervisor launches has the
// `caddy-storage-etcd` module compiled in. Operators get such a binary via
//
//	xcaddy build --with github.com/gsmlg-dev/caddy-storage-etcd
//
// and point `Options.CaddyBinary` at it. Without the module, Caddy rejects
// the /load POST with a structured error which surfaces verbatim through
// Supervisor.Apply — the operator sees exactly what's missing.
//
// Schema reference: github.com/gsmlg-dev/caddy-storage-etcd README. KeyPrefix
// stays under `/weft/proxy/caddy` so the certificate keys don't collide with
// the route-table keys at `/weft/proxy/routes` (same etcd cluster, separate
// namespaces).
func storageEtcdConfig(endpoints []string) map[string]any {
	return map[string]any{
		"module":      "etcd3",
		"endpoints":   endpoints,
		"key_prefix":  "/weft/proxy/caddy",
		"lock":        "/weft/proxy/caddy/locks",
		"timeout":     "10s",
	}
}
