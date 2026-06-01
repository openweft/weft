// place.go — recommendation-only placement scoring for
// federation-lite. Per docs/design/federation.md §6, `weft federation
// place` returns a recommendation, not a binding lease ; the
// `openweft_nominal_binding` memory ensures we surface that
// distinction (a workload that pins `spec.cluster: …` overrides this
// scoring outright).
//
// Score factors, in order of weight :
//
//   - region match (strong) : an exact match against the constraint's
//     `region` tag bumps the candidate by 100 points. Skipped when
//     the constraint omits the field.
//   - weight (medium) : Cluster.NormalisedWeight, capped at 100 for
//     scoring purposes — operator-tunable bias for "prefer cluster X
//     when otherwise equal".
//   - freshness penalty (light) : a stale peer (last_seen > StaleTTL)
//     drops by 50 points. We don't outright filter — Snapshot
//     already classifies stale, the placer leaves it in the
//     candidate set so operator override still works.
//
// Ties break on Name ascending — deterministic output is more
// valuable than any particular tie-break heuristic at this volume.

package federation

import (
	"sort"
	"strings"
	"time"
)

// Constraints is the placement query input. All fields are optional ;
// an empty Constraints scores every candidate purely on weight +
// freshness. v0.2 ships the minimal set the design doc commits to ;
// follow-ups (GPU model availability, tenant-residency rules) layer
// on without breaking this shape.
type Constraints struct {
	// Region pins the candidate to a specific locality. Exact-match
	// against Cluster.Region ; case-sensitive. Empty = no region
	// preference.
	Region string
	// MinWeight filters out candidates whose NormalisedWeight falls
	// below this floor. Zero = no floor.
	MinWeight int
	// ExcludeNames removes specific members from consideration —
	// the operator's "I know cluster X is being upgraded, don't
	// even consider it" escape hatch.
	ExcludeNames []string
}

// Recommendation is the per-candidate output of Place. Score is the
// composite metric the placer used to rank ; the caller surfaces
// the top entry by default but the full slice is exposed so
// `--json` can dump the runner-ups.
type Recommendation struct {
	Cluster  Cluster
	Score    int
	Reasons  []string // one-line "why this score" breakdown
	LastSeen time.Time
	Stale    bool
}

// PlaceInput bundles the source data the placer reads. Separating
// from Constraints keeps the constraint expression operator-facing
// (HCL / CLI) and the input system-facing (local + peer-cached
// manifests, freshness). All slices are read-only ; the placer
// makes its own copies.
type PlaceInput struct {
	// Local is the manifest the calling cluster owns. Always
	// considered live (we trust our own etcd).
	Local *FederationManifest
	// Peers is the snapshot the Poller produces. The placer reads
	// Manifest off each entry ; stale rows count for half the
	// freshness slot.
	Peers []PeerState
	// Now is the clock used for stale arithmetic. Zero → time.Now.
	Now func() time.Time
}

// Place ranks candidate clusters against the constraints. Returns
// the recommendations in descending score order. An empty result
// means no candidate satisfied the hard filters (Region exclusion,
// MinWeight floor, ExcludeNames blacklist) — callers should surface
// that distinction to the operator.
func Place(in PlaceInput, c Constraints) []Recommendation {
	exclude := make(map[string]struct{}, len(c.ExcludeNames))
	for _, n := range c.ExcludeNames {
		exclude[n] = struct{}{}
	}
	var out []Recommendation
	if in.Local != nil {
		for _, m := range in.Local.Members {
			if _, skip := exclude[m.Name]; skip {
				continue
			}
			r, ok := scoreCandidate(m, c, false, time.Time{})
			if !ok {
				continue
			}
			out = append(out, r)
		}
	}
	for _, p := range in.Peers {
		if p.Manifest == nil {
			continue
		}
		stale := p.Status == "stale"
		for _, m := range p.Manifest.Members {
			if _, skip := exclude[m.Name]; skip {
				continue
			}
			r, ok := scoreCandidate(m, c, stale, p.LastSeen)
			if !ok {
				continue
			}
			out = append(out, r)
		}
	}
	// Sort descending by Score, then ascending by Name so the output
	// is deterministic on tie.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Cluster.Name < out[j].Cluster.Name
	})
	return out
}

func scoreCandidate(m Cluster, c Constraints, stale bool, lastSeen time.Time) (Recommendation, bool) {
	w := m.NormalisedWeight()
	if c.MinWeight > 0 && w < c.MinWeight {
		return Recommendation{}, false
	}
	score := 0
	reasons := make([]string, 0, 4)
	if c.Region != "" {
		if m.Region == c.Region {
			score += 100
			reasons = append(reasons, "region match +100")
		} else {
			score -= 25
			reasons = append(reasons, "region mismatch -25")
		}
	}
	// Cap weight contribution at 100 so a weight=1000 cluster
	// doesn't run away from a perfectly-matching weight=100 peer in
	// a different region.
	weightBonus := w
	if weightBonus > 100 {
		weightBonus = 100
	}
	score += weightBonus
	reasons = append(reasons, "weight bonus +"+itoa(weightBonus))
	if stale {
		score -= 50
		reasons = append(reasons, "stale peer -50")
	}
	return Recommendation{
		Cluster:  m,
		Score:    score,
		Reasons:  reasons,
		LastSeen: lastSeen,
		Stale:    stale,
	}, true
}

// itoa is a tiny strconv.Itoa replacement so the file stays
// dependency-minimal — keeps the package's existing import set
// (json + errors + fmt) tight.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Top returns the highest-scoring recommendation, or false when
// recs is empty. Convenience for callers that want the single
// answer ; the CLI's `weft federation place` prints both.
func Top(recs []Recommendation) (Recommendation, bool) {
	if len(recs) == 0 {
		return Recommendation{}, false
	}
	return recs[0], true
}

// ParseConstraints is a forgiving operator-friendly parser for the
// `--constraints` flag value. Accepts a comma-separated list of
// `key=value` pairs ; whitespace is stripped, keys are lower-cased.
// Unknown keys are reported via err so the operator notices a
// typo. The supported keys mirror the Constraints fields :
//
//	region=eu-west-3
//	min_weight=50
//	exclude=eu-paris,us-iad
func ParseConstraints(expr string) (Constraints, error) {
	var c Constraints
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return c, nil
	}
	parts := strings.Split(expr, ",")
	// Walk into pairs ; we re-glue any comma that appeared inside an
	// `exclude=` value by accumulating successive bare tokens onto
	// the previous key.
	var lastKey string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, "=") {
			if lastKey == "exclude" {
				c.ExcludeNames = append(c.ExcludeNames, p)
				continue
			}
			return c, errFmt("federation: malformed constraint %q (expected key=value)", p)
		}
		k, v, _ := strings.Cut(p, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		lastKey = k
		switch k {
		case "region":
			c.Region = v
		case "min_weight":
			n, err := atoi(v)
			if err != nil {
				return c, errFmt("federation: min_weight %q: %v", v, err)
			}
			c.MinWeight = n
		case "exclude":
			if v != "" {
				c.ExcludeNames = append(c.ExcludeNames, v)
			}
		default:
			return c, errFmt("federation: unknown constraint key %q", k)
		}
	}
	return c, nil
}

func atoi(s string) (int, error) {
	if s == "" {
		return 0, errFmt("empty")
	}
	n := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch < '0' || ch > '9' {
			return 0, errFmt("non-digit %q", ch)
		}
		n = n*10 + int(ch-'0')
	}
	return n, nil
}

// errFmt is a tiny fmt.Errorf-equivalent so this file doesn't drag
// fmt into a place.go-only import. Kept private to the package.
func errFmt(format string, args ...interface{}) error {
	return placeError(sprintf(format, args...))
}

type placeError string

func (e placeError) Error() string { return string(e) }

// sprintf is a 1-verb-deep mini fmt.Sprintf supporting %q / %v / %d
// — enough for the error strings above. Anything richer should
// reach for fmt.
func sprintf(format string, args ...interface{}) string {
	var out strings.Builder
	ai := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i == len(format)-1 {
			out.WriteByte(format[i])
			continue
		}
		i++
		verb := format[i]
		if ai >= len(args) {
			out.WriteByte('%')
			out.WriteByte(verb)
			continue
		}
		arg := args[ai]
		ai++
		switch verb {
		case 'q':
			out.WriteByte('"')
			out.WriteString(stringify(arg))
			out.WriteByte('"')
		case 'v', 's':
			out.WriteString(stringify(arg))
		case 'd':
			if n, ok := arg.(int); ok {
				out.WriteString(itoa(n))
			} else {
				out.WriteString(stringify(arg))
			}
		default:
			out.WriteByte('%')
			out.WriteByte(verb)
		}
	}
	return out.String()
}

func stringify(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case error:
		return x.Error()
	case byte:
		return string([]byte{x})
	case int:
		return itoa(x)
	default:
		return ""
	}
}
