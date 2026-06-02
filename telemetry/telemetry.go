// Package telemetry implements the anonymous, opt-in heartbeat that
// an operator can enable to collect minimal fleet stats from their
// own weft cluster.
//
// Policy in one line: the openweft project does NOT collect telemetry
// by default and runs no central server. This package exists so an
// operator running their OWN aggregator (internal metrics endpoint,
// vendor stack, whatever) can wire it up without rolling a sidecar.
//
// The default state is DISABLED — `Sender.Send` is a no-op until an
// operator runs `weft telemetry enable [--endpoint URL]`. When
// enabled, a 24h ticker (driven from cmd/weft/main.go) POSTs a tiny
// JSON envelope describing the cluster shape, NEVER any PII.
//
// See docs/operations/telemetry.md for the full payload contract and
// the explicit list of fields we will never add.
package telemetry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"time"
)

// AgentVersion is the version string stamped into every payload.
// Kept as a package-level var (not const) so a release-build tooling
// pass can override it at link time without disturbing the source
// tree. Mirrors CHANGELOG.md.
var AgentVersion = "v0.1.0"

// State is the opt-in flag block persisted to RegistryStorage.
// JSON-encoded (not HCL) — this is a tiny machine-only blob, no
// human edits expected.
//
// Field tags use snake_case to match the wire payload convention.
type State struct {
	// Enabled gates the entire pipeline. Default zero-value is
	// false, which matches the "off by default" policy.
	Enabled bool `json:"enabled"`

	// Endpoint is the operator-supplied URL telemetry POSTs to.
	// Empty when the operator hasn't picked one yet ; Sender.Send
	// treats empty-endpoint as a no-op (success) so the agent
	// doesn't error-loop while the operator is still wiring
	// their aggregator.
	Endpoint string `json:"endpoint,omitempty"`

	// ClusterUUID + InstallDate together feed the anonymous_id
	// hash. Minted on first enable() ; stable thereafter so the
	// receiver can de-duplicate across heartbeats from the same
	// cluster without learning who the cluster is.
	ClusterUUID string `json:"cluster_uuid,omitempty"`
	InstallDate string `json:"install_date,omitempty"`

	// LastSentAt is updated by Sender.Send on a successful POST.
	// Surfaced by `weft telemetry status` so the operator knows
	// the pipe is live.
	LastSentAt time.Time `json:"last_sent_at,omitempty"`
}

// Snapshot is the fleet shape Source.Snapshot collects. The
// telemetry package owns the JSON shape ; the Adapter is just
// expected to fill the counts.
//
// PII boundary: every field on Snapshot is either a count, a label
// already public on the artifact (driver kind, plugin name from the
// public catalogue), or a process-level constant (Go version,
// GOOS/GOARCH). NO names, IPs, UUIDs, emails, addresses.
type Snapshot struct {
	HostCount        int      `json:"host_count"`
	VMCountRunning   int      `json:"vm_count_running"`
	Drivers          []string `json:"drivers"`
	PluginsInstalled []string `json:"plugins_installed"`
}

// Source is the read-side contract Sender pulls from. It's kept as
// an interface (not a concrete *weft.Adapter) so :
//
//  1. The telemetry package stays decoupled from the adapter — no
//     import cycle, easy to swap for a test double.
//  2. The PII review surface is exactly this interface : if a
//     reviewer wants to know "what does telemetry see", they read
//     four method signatures, not the adapter.
//
// Implementations live wherever the wiring happens (cmd/weft).
type Source interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

// Store is the persistence contract for State. It maps onto
// weft.RegistryStorage one-to-one — Load/Save returning a blob —
// but typed in terms of State so the package consumer doesn't have
// to encode JSON twice.
type Store interface {
	LoadState(ctx context.Context) (State, error)
	SaveState(ctx context.Context, s State) error
}

// Logger is the tiny logging surface we need. *log.Logger satisfies
// it directly. Intentionally narrower than the standard Logger so a
// test can pass nil and we degrade to a discard sink.
type Logger interface {
	Printf(format string, v ...any)
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

// HTTPClient is the subset of *http.Client we depend on. Carved out
// so tests can swap in a fake without standing up an httptest.Server
// when they want to assert request shape only.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Sender is the periodic heartbeat. Construct one with New and call
// Send on a 24h ticker (cmd/weft/main.go does the scheduling).
//
// Concurrency model: Send is safe to call concurrently from multiple
// goroutines, but the expected usage is one ticker goroutine. The
// internal mutex only protects start-time, which is set once.
type Sender struct {
	store  Store
	source Source
	client HTTPClient
	logger Logger
	now    func() time.Time

	startOnce sync.Once
	startTime time.Time
}

// Options bundles the wiring inputs so the construction site can
// pass them positionally without an arg-order bug.
type Options struct {
	Store  Store
	Source Source
	// Client overrides the default 10s-timeout *http.Client. Nil
	// is fine — the default is what production should use.
	Client HTTPClient
	// Logger is where retry / failure messages land. Nil = discard.
	Logger Logger
	// Now overrides time.Now for tests. Nil = time.Now.
	Now func() time.Time
}

// New wires a Sender from the options. The HTTP client defaults to a
// 10-second timeout per send (covers DNS + TLS handshake + body
// write, NOT total per-retry).
func New(opts Options) *Sender {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	logger := opts.Logger
	if logger == nil {
		logger = discardLogger{}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Sender{
		store:  opts.Store,
		source: opts.Source,
		client: client,
		logger: logger,
		now:    now,
	}
}

// MarkStart records the process start time. The first uptime_seconds
// value in the payload is now-MarkStart. Idempotent — second and
// later calls are no-ops, so cmd/weft can call it during bootstrap
// without worrying about test harnesses that reuse a Sender.
func (s *Sender) MarkStart(t time.Time) {
	s.startOnce.Do(func() { s.startTime = t })
}

// BuildPayload assembles the wire-level JSON envelope from the
// current cluster snapshot + persisted State. Exposed (capitalised)
// so `weft telemetry preview` can print it without sending.
//
// Returns ErrDisabled when the State.Enabled flag is unset — the
// CLI shadows that into a friendlier message ; the Sender.Send path
// treats it as a no-op success.
func (s *Sender) BuildPayload(ctx context.Context) (Payload, State, error) {
	st, err := s.store.LoadState(ctx)
	if err != nil {
		return Payload{}, State{}, fmt.Errorf("load telemetry state: %w", err)
	}
	if !st.Enabled {
		return Payload{}, st, ErrDisabled
	}
	snap, err := s.source.Snapshot(ctx)
	if err != nil {
		return Payload{}, st, fmt.Errorf("collect telemetry snapshot: %w", err)
	}
	// Defensive sort + nil-to-empty conversion. Two preview calls
	// in a row should be byte-identical for the de-dup hash test ;
	// nil-vs-[]string{} would break json.Marshal byte equality.
	drivers := append([]string(nil), snap.Drivers...)
	sort.Strings(drivers)
	if drivers == nil {
		drivers = []string{}
	}
	plugins := append([]string(nil), snap.PluginsInstalled...)
	sort.Strings(plugins)
	if plugins == nil {
		plugins = []string{}
	}

	uptime := int64(0)
	if !s.startTime.IsZero() {
		d := s.now().Sub(s.startTime)
		if d > 0 {
			uptime = int64(d.Seconds())
		}
	}

	p := Payload{
		AnonymousID:      AnonymousID(st.ClusterUUID, st.InstallDate),
		Version:          AgentVersion,
		HostCount:        snap.HostCount,
		VMCountRunning:   snap.VMCountRunning,
		Drivers:          drivers,
		PluginsInstalled: plugins,
		GoVersion:        runtime.Version(),
		OS:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		UptimeSeconds:    uptime,
	}
	return p, st, nil
}

// Payload is the exact wire-level JSON shape. Field order in the
// struct == field order in the encoded JSON for the default
// encoding/json output, which keeps the operator-visible preview
// output stable for diffing.
//
// EVERY field here was reviewed against the no-PII contract. The
// list in docs/operations/telemetry.md is the authoritative copy of
// what may live on this struct.
type Payload struct {
	AnonymousID      string   `json:"anonymous_id"`
	Version          string   `json:"version"`
	HostCount        int      `json:"host_count"`
	VMCountRunning   int      `json:"vm_count_running"`
	Drivers          []string `json:"drivers"`
	PluginsInstalled []string `json:"plugins_installed"`
	GoVersion        string   `json:"go_version"`
	OS               string   `json:"os"`
	Arch             string   `json:"arch"`
	UptimeSeconds    int64    `json:"uptime_seconds"`
}

// AnonymousID is sha256(cluster_uuid + install_date) truncated to
// 16 hex chars. Stable for the lifetime of a cluster ; opaque to
// the receiver — they can de-duplicate heartbeats from the same
// cluster but cannot reverse it back to identifying inputs.
//
// Exposed (capitalised) so the CLI tests + preview can verify
// stability across calls without re-implementing the hash.
func AnonymousID(clusterUUID, installDate string) string {
	if clusterUUID == "" && installDate == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(clusterUUID + installDate))
	return hex.EncodeToString(sum[:])[:16]
}

// ErrDisabled is returned by BuildPayload / Send when telemetry is
// opted-out. Send swallows it (logs nothing) ; CLI surfaces it as a
// friendly "telemetry is disabled" message.
var ErrDisabled = errors.New("telemetry disabled")

// Send is the public per-tick entry point. The 24h ticker in
// cmd/weft/main.go calls it ; everything else (retry, backoff,
// state load, payload build) lives inside.
//
// Behaviour matrix :
//
//   - Disabled        → no-op, returns nil. The ticker is allowed
//     to run even when disabled — Send is the
//     authoritative gate, so re-enabling at runtime
//     doesn't require a daemon restart.
//   - Endpoint empty  → no-op, returns nil. Treated as "operator
//     enabled the flag but hasn't pointed it at
//     a URL yet" ; logging a warning each tick
//     would be noisy.
//   - 5xx             → retried up to 3 times with exponential
//     backoff (1s, 2s, 4s). All retries share the
//     parent ctx so a shutdown cancels the loop.
//   - 4xx             → no retry. The receiver is telling us
//     "don't try again with this payload" ;
//     hammering them won't help.
//   - Network error   → retried as 5xx (transient transport).
//
// Failure is logged at INFO level only (per spec) — telemetry being
// down is a no-impact operational event.
func (s *Sender) Send(ctx context.Context) error {
	payload, st, err := s.BuildPayload(ctx)
	if errors.Is(err, ErrDisabled) {
		return nil
	}
	if err != nil {
		s.logger.Printf("telemetry: build payload: %v", err)
		return err
	}
	if st.Endpoint == "" {
		// Enabled but no endpoint — silent no-op. The CLI
		// banner on enable() already told the operator to set
		// one ; nothing useful happens at tick time.
		return nil
	}

	body, err := json.Marshal(payload)
	if err != nil {
		// json.Marshal failing on a fixed struct = programmer
		// error, not a runtime issue. Surface it loud so a
		// drift in the Payload shape is caught.
		return fmt.Errorf("marshal telemetry payload: %w", err)
	}

	backoffs := []time.Duration{0, 1 * time.Second, 2 * time.Second, 4 * time.Second}
	var lastErr error
	for attempt, wait := range backoffs {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		status, sendErr := s.post(ctx, st.Endpoint, body)
		if sendErr == nil && status >= 200 && status < 300 {
			st.LastSentAt = s.now().UTC()
			if err := s.store.SaveState(ctx, st); err != nil {
				s.logger.Printf("telemetry: save last-sent-at: %v", err)
			}
			return nil
		}
		if sendErr != nil {
			lastErr = sendErr
			s.logger.Printf("telemetry: send attempt %d: %v", attempt+1, sendErr)
			continue
		}
		// HTTP-level error. 4xx is terminal — receiver said
		// don't retry. 5xx and others fall through to the next
		// backoff slot.
		lastErr = fmt.Errorf("status %d", status)
		s.logger.Printf("telemetry: send attempt %d: status %d", attempt+1, status)
		if status >= 400 && status < 500 {
			return nil
		}
	}
	if lastErr != nil {
		s.logger.Printf("telemetry: giving up after %d attempts: %v", len(backoffs), lastErr)
	}
	// Telemetry being down is not an operational failure — the
	// caller (the 24h ticker) discards the error anyway, but
	// returning nil here makes that intent explicit.
	return nil
}

// post performs a single HTTP POST and returns (status, transport-error).
// Caller decides what to do with the status ; we don't peek at the
// body — the receiver is opaque to us.
func (s *Sender) post(ctx context.Context, endpoint string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "weft-telemetry/"+AgentVersion)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain the body so the underlying connection can be reused
	// across retries. 1 KiB cap — anything larger from a
	// telemetry receiver is suspicious and we don't want it.
	_, _ = io.CopyN(io.Discard, resp.Body, 1<<10)
	return resp.StatusCode, nil
}

// LoadState is a tiny convenience that mirrors Store.LoadState so
// the CLI doesn't have to wire a Sender just to read the flag.
func LoadState(ctx context.Context, store Store) (State, error) {
	return store.LoadState(ctx)
}

// SaveState mirror.
func SaveState(ctx context.Context, store Store, s State) error {
	return store.SaveState(ctx, s)
}

// --- JSON-blob Store over weft.Storage ------------------------------

// BlobStorage is the subset of weft.Storage we need. Mirrors the
// Load/Save contract so telemetry doesn't depend on the weft
// package directly (avoids the import cycle weft→telemetry→weft).
type BlobStorage interface {
	Load(ctx context.Context) ([]byte, error)
	Save(ctx context.Context, blob []byte) error
}

// NewBlobStore adapts a weft.Storage to telemetry.Store. JSON is the
// on-disk format because the State carries timestamps + UUIDs that
// would be awkward to hand-edit anyway — the operator drives this
// through `weft telemetry enable/disable` rather than vim.
func NewBlobStore(s BlobStorage) Store {
	return &blobStore{s: s}
}

type blobStore struct {
	s  BlobStorage
	mu sync.Mutex
}

func (b *blobStore) LoadState(ctx context.Context) (State, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, err := b.s.Load(ctx)
	if err != nil {
		return State{}, err
	}
	if len(raw) == 0 {
		return State{}, nil
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, fmt.Errorf("decode telemetry state: %w", err)
	}
	return st, nil
}

func (b *blobStore) SaveState(ctx context.Context, st State) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode telemetry state: %w", err)
	}
	raw = append(raw, '\n')
	return b.s.Save(ctx, raw)
}

// NewClusterIdentity mints the (cluster_uuid, install_date) pair an
// enable() call needs. Kept here (not in the CLI) so the hash
// recipe is reviewable in one file.
//
// cluster_uuid is a 128-bit random hex string (16 bytes) ; not a
// V4 UUID specifically because we don't need the version bits and
// the simpler 32-hex shape stays auditable. install_date is
// RFC3339-UTC truncated to the day — the receiver gets the cohort
// week, not the install hour.
func NewClusterIdentity(now time.Time, randHex string) (clusterUUID, installDate string) {
	return randHex, now.UTC().Format("2006-01-02")
}

// AssertLogger is a tiny adapter making the stdlib *log.Logger fit
// the Logger interface without an extra wrapper in cmd/weft.
var _ Logger = (*log.Logger)(nil)
