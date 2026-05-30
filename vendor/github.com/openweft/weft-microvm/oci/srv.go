package oci

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Endpoint is one concrete OCI server endpoint resolved from an SRV record
// (or a passthrough for non-SRV hosts).
type Endpoint struct {
	Host string // "host" or "host:port"
}

// IsSRVHost reports whether h looks like an RFC 2782 service name —
// `_<service>._tcp.<zone>` or `_<service>._udp.<zone>`.
func IsSRVHost(h string) bool {
	if !strings.HasPrefix(h, "_") {
		return false
	}
	return strings.Contains(h, "._tcp.") || strings.Contains(h, "._udp.")
}

// SRVCacheTTL controls how long resolved SRV results are cached. Zero
// disables caching. Modify via the package init or tests; it is not part
// of the public API.
var SRVCacheTTL = 30 * time.Second

var (
	srvCacheMu sync.Mutex
	srvCache   = map[string]srvEntry{}
)

// ClearSRVCache wipes the in-memory SRV cache. cloud-boot-init calls
// this after rewriting /etc/resolv.conf so the next ResolveEndpoints
// re-queries against the new resolver instead of returning a stale
// result resolved via the old DNS.
func ClearSRVCache() {
	srvCacheMu.Lock()
	defer srvCacheMu.Unlock()
	srvCache = map[string]srvEntry{}
}

type srvEntry struct {
	expires time.Time
	eps     []Endpoint
}

// ResolveEndpoints returns the list of endpoints to try for host h, in RFC
// 2782 priority/weight order. If h is not an SRV name the single-element
// list `[{Host: h}]` is returned, so callers can use this unconditionally.
func ResolveEndpoints(h string) ([]Endpoint, error) {
	if !IsSRVHost(h) {
		return []Endpoint{{Host: h}}, nil
	}

	srvCacheMu.Lock()
	if e, ok := srvCache[h]; ok && time.Now().Before(e.expires) {
		out := make([]Endpoint, len(e.eps))
		copy(out, e.eps)
		srvCacheMu.Unlock()
		return out, nil
	}
	srvCacheMu.Unlock()

	eps, err := lookupSRV(h)
	if err != nil {
		return nil, err
	}
	if SRVCacheTTL > 0 {
		srvCacheMu.Lock()
		srvCache[h] = srvEntry{expires: time.Now().Add(SRVCacheTTL), eps: eps}
		srvCacheMu.Unlock()
	}
	return eps, nil
}

// lookupSRVImpl is the actual DNS query. Replaceable from tests; the default
// uses Go's resolver. It is a function variable rather than a package-level
// function so the parts-validation logic inside lookupSRV is reachable from
// unit tests without touching DNS.
var lookupSRVImpl = func(ctx context.Context, svc, proto, name string) (string, []*net.SRV, error) {
	return net.DefaultResolver.LookupSRV(ctx, svc, proto, name)
}

func lookupSRV(h string) ([]Endpoint, error) {
	parts := strings.SplitN(h, ".", 3)
	if len(parts) < 3 || !strings.HasPrefix(parts[0], "_") || !strings.HasPrefix(parts[1], "_") {
		return nil, fmt.Errorf("oci: invalid SRV host %q", h)
	}
	svc := strings.TrimPrefix(parts[0], "_")
	proto := strings.TrimPrefix(parts[1], "_")
	zone := parts[2]

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, srvs, err := lookupSRVImpl(ctx, svc, proto, zone)
	if err != nil {
		return nil, fmt.Errorf("oci: srv %s: %w", h, err)
	}
	if len(srvs) == 0 {
		return nil, fmt.Errorf("oci: srv %s returned no records", h)
	}
	return orderSRV(srvs, rand.New(rand.NewSource(time.Now().UnixNano()))), nil
}

// orderSRV applies RFC 2782 ordering: group by priority ascending, then run a
// weighted random selection within each group. The randomness lets two
// machines that resolve the same record at the same instant land on
// different replicas, distributing load.
func orderSRV(rec []*net.SRV, rng *rand.Rand) []Endpoint {
	byPrio := map[uint16][]*net.SRV{}
	var prios []uint16
	for _, r := range rec {
		if _, ok := byPrio[r.Priority]; !ok {
			prios = append(prios, r.Priority)
		}
		byPrio[r.Priority] = append(byPrio[r.Priority], r)
	}
	sort.Slice(prios, func(i, j int) bool { return prios[i] < prios[j] })

	out := make([]Endpoint, 0, len(rec))
	for _, p := range prios {
		out = append(out, weightedShuffle(byPrio[p], rng)...)
	}
	return out
}

// weightedShuffle implements the RFC 2782 selection algorithm within a single
// priority group: cumulative weights, pick uniformly in [0, sum], take the
// first entry whose running weight exceeds the pick, repeat.
func weightedShuffle(grp []*net.SRV, rng *rand.Rand) []Endpoint {
	pool := make([]*net.SRV, len(grp))
	copy(pool, grp)
	var out []Endpoint
	for len(pool) > 0 {
		sum := 0
		for _, r := range pool {
			sum += int(r.Weight)
		}
		var idx int
		switch {
		case sum == 0:
			idx = rng.Intn(len(pool))
		default:
			pick := rng.Intn(sum + 1)
			running := 0
			for i, r := range pool {
				running += int(r.Weight)
				if running >= pick {
					idx = i
					break
				}
			}
		}
		r := pool[idx]
		out = append(out, Endpoint{
			Host: strings.TrimRight(r.Target, ".") + ":" + strconv.FormatUint(uint64(r.Port), 10),
		})
		pool = append(pool[:idx], pool[idx+1:]...)
	}
	return out
}
