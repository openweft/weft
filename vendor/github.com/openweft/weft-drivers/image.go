package drivers

import "context"

// ImageDriver caches OCI artifacts (cloud images, UKI bundles,
// kernel/initrd pairs) on a host so CreateVM can clone from a
// local copy instead of pulling from the registry on every boot.
//
// One driver instance per (host, cache backend) — typically one
// per weft-agent, backed by a host-local directory. Future:
// driver that delegates to a per-AZ zot mirror so the same blob
// only crosses the WAN once per AZ.
type ImageDriver interface {
	HostInfo(ctx context.Context) (HostInfo, error)

	// Pull fetches the OCI ref into the local cache. Idempotent:
	// already-present is a no-op, in-progress concurrent pulls
	// of the same ref deduplicate.
	Pull(ctx context.Context, ref string) error

	// LocalPath returns the absolute path of the cached artifact.
	// Returns an error when the ref is not in cache — caller is
	// expected to Pull first.
	LocalPath(ctx context.Context, ref string) (string, error)

	// Delete removes an entry from the cache. No-op if missing.
	// The host-local cache is GC'd separately based on LRU;
	// Delete is the explicit "remove this now" hook.
	Delete(ctx context.Context, ref string) error

	// InCache is the cheap existence check the scheduler runs
	// before deciding whether to pre-pull on a placement.
	InCache(ctx context.Context, ref string) (bool, error)
}
