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
// darkweak/storages etcd adapter compiled in. The canonical build is
// `github.com/openweft/weft-proxy` (published binary + OCI image), which
// embeds the adapter alongside the standard Caddy modules. Operators
// point `Options.CaddyBinary` at it. Without the module, Caddy rejects
// the /load POST with a structured error which surfaces verbatim through
// Supervisor.Apply — the operator sees exactly what's missing.
//
// Schema: the darkweak adapter registers as the "etcd" Caddy storage
// module. KeyPrefix stays under `/weft/proxy/caddy` so the certificate
// keys don't collide with the route-table keys at `/weft/proxy/routes`
// (same etcd cluster, separate namespaces).
func storageEtcdConfig(endpoints []string) map[string]any {
	return map[string]any{
		"module":     "etcd",
		"endpoints":  endpoints,
		"key_prefix": "/weft/proxy/caddy",
	}
}
