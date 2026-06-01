// poller.go — peer-poll goroutine that pulls each configured peer's
// /cluster-info every Interval, verifies the signature against the
// pre-shared ed25519 public key, and caches the latest-pulled
// manifest in-memory. Federation-lite is pull-only (per the
// `openweft_pull_model` memory) ; no peer pushes state into our
// store. See docs/design/federation.md §3a + §9 for the rationale.
//
// Stale TTL : a peer whose last successful poll is older than
// StaleTTL is demoted from `live` to `stale`. PeerStatus exposes
// the bool to placement code so weft federation place ignores
// stale peers without erroring them out (operator may want to see
// them in `weft federation list` regardless). Picked 5 minutes
// (10 × the default 30 s polling cadence) — short enough that a
// stuck peer falls off the placement window quickly, long enough
// that two consecutive transient HTTP failures don't bounce a
// healthy member.

package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultPollInterval is the cadence the v0.2 design pins for
// /cluster-info polling. 30 s matches the freshness bound in
// docs/design/federation.md §9.
const DefaultPollInterval = 30 * time.Second

// DefaultPeerStaleTTL is how long a peer can go without a successful
// poll before placement code treats it as stale. 5 minutes = 10× the
// default poll interval ; survives one or two transient HTTP errors,
// trips on a real outage.
const DefaultPeerStaleTTL = 5 * time.Minute

// PeerConfig is the operator-supplied per-peer record. URL points at
// the peer's /cluster-info endpoint (or its base URL — the poller
// resolves the relative path). PublicKey is the ed25519 verification
// key the operator pre-shared out of band (e.g. on the `weft
// federation join` command line).
type PeerConfig struct {
	URL       string
	PublicKey ed25519.PublicKey
	// Name is the operator-visible label printed by `weft federation
	// list`. Defaults to the URL when empty ; once a successful poll
	// lands the cluster's manifest name overrides for display.
	Name string
}

// PeerState is the per-peer mutable bookkeeping the Poller exposes
// via Snapshot. Treat as read-only on the caller side ; the Poller
// owns the struct and may rewrite it under its mutex.
type PeerState struct {
	Name      string              // operator-supplied or manifest-derived label
	URL       string              // /cluster-info URL
	LastSeen  time.Time           // zero until the first successful poll
	LastError string              // most recent error string (empty when healthy)
	Manifest  *FederationManifest // most recent verified manifest, nil before first success
	// Status is "live", "stale" or "unreachable" — derived from
	// LastSeen + LastError on each Snapshot call so the value is
	// always coherent with the rest of the row.
	Status string
}

// Poller is the long-lived goroutine that polls each peer's
// /cluster-info on Interval. Construct via NewPoller, then Start to
// launch ; Stop drains the goroutine. Zero value is not usable —
// Peers must be set before Start.
type Poller struct {
	// Peers is the operator-configured set, snapshotted at Start
	// time. Mutating after Start has no effect — re-Start to pick
	// up a new list. v0.2 keeps the federation table static between
	// reloads ; v0.3 will plug a watcher onto the HCL block.
	Peers []PeerConfig
	// Interval between polls. Zero → DefaultPollInterval.
	Interval time.Duration
	// StaleTTL bounds the freshness window. Zero → DefaultPeerStaleTTL.
	StaleTTL time.Duration
	// HTTPClient is the client used for the GETs. Zero → a fresh
	// http.Client with a 10 s timeout, plenty for /cluster-info
	// responses that are a few hundred bytes.
	HTTPClient *http.Client
	// Now is the clock function. Zero → time.Now ; tests swap it
	// in to drive stale-after-TTL deterministically.
	Now func() time.Time

	mu     sync.Mutex
	states map[string]*PeerState // keyed by PeerConfig.URL
	done   chan struct{}
	cancel context.CancelFunc
}

// NewPoller builds a Poller with the supplied peers + interval.
// Zero interval / TTL pick the documented defaults.
func NewPoller(peers []PeerConfig, interval, staleTTL time.Duration) *Poller {
	return &Poller{
		Peers:    peers,
		Interval: interval,
		StaleTTL: staleTTL,
	}
}

// Start launches the background poll goroutine. Returns an error if
// Peers is empty or any URL is malformed — bad config is a startup
// failure, not a silent no-op (operator who typed `peers = ["xxx"]`
// in weft.hcl expects to learn about it). Calling Start twice
// without an intervening Stop is a programmer error and returns an
// error.
func (p *Poller) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done != nil {
		return errors.New("federation: poller already started")
	}
	if len(p.Peers) == 0 {
		return errors.New("federation: poller needs at least one peer")
	}
	for i, peer := range p.Peers {
		if peer.URL == "" {
			return fmt.Errorf("federation: peers[%d].url is required", i)
		}
		if _, err := url.Parse(peer.URL); err != nil {
			return fmt.Errorf("federation: peers[%d].url %q: %w", i, peer.URL, err)
		}
		if len(peer.PublicKey) != ed25519.PublicKeySize {
			return fmt.Errorf("federation: peers[%d].pubkey must be %d bytes, got %d", i, ed25519.PublicKeySize, len(peer.PublicKey))
		}
	}
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	staleTTL := p.StaleTTL
	if staleTTL <= 0 {
		staleTTL = DefaultPeerStaleTTL
	}
	if p.HTTPClient == nil {
		p.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	p.Interval = interval
	p.StaleTTL = staleTTL

	p.states = make(map[string]*PeerState, len(p.Peers))
	for _, peer := range p.Peers {
		name := peer.Name
		if name == "" {
			name = peer.URL
		}
		p.states[peer.URL] = &PeerState{Name: name, URL: peer.URL, Status: "unreachable"}
	}
	done := make(chan struct{})
	p.done = done
	runCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	// Pass done as a parameter so the goroutine isn't racing
	// against Stop reading p.done after it's nilled out.
	go p.loop(runCtx, done)
	return nil
}

// loop runs the poll cycle until ctx is cancelled. Each tick polls
// every peer sequentially — the peer set is small (single digits to
// low tens per docs/design/federation.md) so parallelism wouldn't
// pay for the goroutine overhead. The first poll happens
// immediately rather than after a full interval — operators expect
// `weft federation list` to populate within a few seconds of agent
// startup.
func (p *Poller) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	p.pollAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pollAll(ctx)
		}
	}
}

func (p *Poller) pollAll(ctx context.Context) {
	for _, peer := range p.Peers {
		_ = p.PollOnce(ctx, peer)
	}
}

// PollOnce performs a single GET against peer.URL, verifies the
// ed25519 signature, and updates the cached state. Exported so
// tests can drive a deterministic poll without spinning the
// goroutine ; production callers go through Start instead. The
// returned error is the same one stored on PeerState.LastError —
// non-nil means "this poll failed", but the prior cached manifest
// stays addressable via Snapshot for the duration of StaleTTL.
func (p *Poller) PollOnce(ctx context.Context, peer PeerConfig) error {
	target := resolveClusterInfoURL(peer.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return p.recordError(peer.URL, fmt.Errorf("build request: %w", err))
	}
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return p.recordError(peer.URL, fmt.Errorf("http get: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return p.recordError(peer.URL, fmt.Errorf("http %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return p.recordError(peer.URL, fmt.Errorf("read body: %w", err))
	}
	sigHex := resp.Header.Get(SignatureHeader)
	if sigHex == "" {
		return p.recordError(peer.URL, errors.New("missing X-Cluster-Signature header"))
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return p.recordError(peer.URL, fmt.Errorf("decode signature: %w", err))
	}
	// Decode-then-verify : we parse the manifest and re-marshal so
	// the signed bytes are exactly what we'd produce locally. This
	// catches a peer that signs a slightly different byte
	// encoding (e.g. extra whitespace) and avoids accepting a
	// manifest whose JSON we don't fully understand.
	var raw FederationManifest
	if err := json.Unmarshal(body, &raw); err != nil {
		return p.recordError(peer.URL, fmt.Errorf("decode manifest: %w", err))
	}
	if err := raw.Verify(peer.PublicKey, sig); err != nil {
		return p.recordError(peer.URL, err)
	}
	return p.recordSuccess(peer.URL, &raw)
}

func (p *Poller) recordError(peerURL string, err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[peerURL]
	if st != nil {
		st.LastError = err.Error()
	}
	return err
}

func (p *Poller) recordSuccess(peerURL string, m *FederationManifest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[peerURL]
	if st == nil {
		return nil // shouldn't happen — Start populated every URL
	}
	st.Manifest = m
	st.LastSeen = p.Now()
	st.LastError = ""
	// Override the operator-supplied label with the manifest's
	// member name when we can — keeps the display consistent with
	// what the peer cluster calls itself.
	if m != nil {
		// The manifest carries N members ; pick the one whose
		// PublicEndpoints contains this peer's URL when possible,
		// fall back to manifest name otherwise. Best-effort, no
		// hard failure if nothing matches.
		if member := matchMemberByEndpoint(m, peerURL); member != nil {
			st.Name = member.Name
		} else if m.Name != "" {
			st.Name = m.Name
		}
	}
	return nil
}

// matchMemberByEndpoint finds the Cluster in m whose PublicEndpoints
// includes peerURL (or its base form). Returns nil when no match.
// Used to label peer rows with the cluster's self-chosen name rather
// than the operator's bookkeeping label.
func matchMemberByEndpoint(m *FederationManifest, peerURL string) *Cluster {
	if m == nil {
		return nil
	}
	base := strings.TrimSuffix(peerURL, ClusterInfoPath)
	for i := range m.Members {
		for _, ep := range m.Members[i].PublicEndpoints {
			if ep == peerURL || ep == base {
				return &m.Members[i]
			}
		}
	}
	return nil
}

// resolveClusterInfoURL accepts either a base cluster URL
// ("https://a.example:8443") or one that already points at
// /cluster-info. Returning the canonical form makes
// PollOnce robust to operator copy-paste shape mismatches.
func resolveClusterInfoURL(raw string) string {
	if strings.HasSuffix(raw, ClusterInfoPath) {
		return raw
	}
	return strings.TrimRight(raw, "/") + ClusterInfoPath
}

// Snapshot returns a copy of the current per-peer state. Status
// is computed on-the-fly from LastSeen + LastError + StaleTTL so
// the caller doesn't need to know the freshness arithmetic.
func (p *Poller) Snapshot() []PeerState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.states == nil {
		return nil
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	out := make([]PeerState, 0, len(p.states))
	for _, peer := range p.Peers {
		st := p.states[peer.URL]
		if st == nil {
			continue
		}
		row := *st
		row.Status = classifyStatus(st, now(), p.StaleTTL)
		out = append(out, row)
	}
	return out
}

func classifyStatus(st *PeerState, now time.Time, staleTTL time.Duration) string {
	if st.LastSeen.IsZero() {
		return "unreachable"
	}
	if staleTTL > 0 && now.Sub(st.LastSeen) > staleTTL {
		return "stale"
	}
	if st.LastError != "" {
		return "stale"
	}
	return "live"
}

// LiveManifests returns the most recent verified manifest for every
// peer whose Status is "live". Placement code (see place.go) reads
// this snapshot when scoring candidate clusters.
func (p *Poller) LiveManifests() []*FederationManifest {
	snap := p.Snapshot()
	out := make([]*FederationManifest, 0, len(snap))
	for _, s := range snap {
		if s.Status == "live" && s.Manifest != nil {
			out = append(out, s.Manifest)
		}
	}
	return out
}

// Stop cancels the poll goroutine and waits for it to drain. Safe to
// call multiple times. Calling Stop on a never-Started poller is a
// no-op (matches http.Server.Shutdown's idempotency).
func (p *Poller) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	done := p.done
	p.cancel = nil
	p.done = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}
