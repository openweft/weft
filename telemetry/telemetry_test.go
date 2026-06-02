package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memStore is the in-memory test double for Store. Implements both
// load/save without touching disk.
type memStore struct {
	mu sync.Mutex
	s  State
}

func (m *memStore) LoadState(_ context.Context) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s, nil
}

func (m *memStore) SaveState(_ context.Context, s State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s = s
	return nil
}

// stubSource returns a fixed Snapshot.
type stubSource struct {
	snap Snapshot
	err  error
}

func (s stubSource) Snapshot(_ context.Context) (Snapshot, error) { return s.snap, s.err }

func newEnabledStore(endpoint string) *memStore {
	return &memStore{s: State{
		Enabled:     true,
		Endpoint:    endpoint,
		ClusterUUID: "abcd1234abcd1234abcd1234abcd1234",
		InstallDate: "2026-06-02",
	}}
}

// Test 1 — Disabled = no-op + nil error. No HTTP server stood up ;
// any attempt to dial would explode.
func TestSend_DisabledIsNoop(t *testing.T) {
	store := &memStore{} // zero State, Enabled = false
	src := stubSource{snap: Snapshot{HostCount: 3}}
	s := New(Options{Store: store, Source: src})
	if err := s.Send(context.Background()); err != nil {
		t.Fatalf("Send when disabled: want nil, got %v", err)
	}
	// LastSentAt must NOT advance when disabled.
	if !store.s.LastSentAt.IsZero() {
		t.Errorf("LastSentAt = %v, want zero (disabled)", store.s.LastSentAt)
	}
}

// Test 2 — Enabled + endpoint + httptest mock = successful POST with
// the expected payload shape.
func TestSend_EnabledPostsExpectedPayload(t *testing.T) {
	var (
		gotBody   []byte
		gotPath   string
		gotMethod string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newEnabledStore(srv.URL + "/v1/telemetry")
	src := stubSource{snap: Snapshot{
		HostCount:        3,
		VMCountRunning:   47,
		Drivers:          []string{"qemu", "vz"},
		PluginsInstalled: []string{"gitlab-runners-ha", "postgres-ha"},
	}}
	s := New(Options{Store: store, Source: src, Now: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }})
	s.MarkStart(time.Unix(1_699_996_400, 0).UTC()) // 1h before now

	if err := s.Send(context.Background()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/telemetry" {
		t.Errorf("path = %q, want /v1/telemetry", gotPath)
	}

	var p Payload
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatalf("body not JSON: %v\nbody=%s", err, gotBody)
	}
	if p.AnonymousID == "" || len(p.AnonymousID) != 16 {
		t.Errorf("AnonymousID = %q, want 16-hex string", p.AnonymousID)
	}
	if p.HostCount != 3 {
		t.Errorf("HostCount = %d, want 3", p.HostCount)
	}
	if p.VMCountRunning != 47 {
		t.Errorf("VMCountRunning = %d, want 47", p.VMCountRunning)
	}
	wantDrivers := []string{"qemu", "vz"}
	if !equalStringSlice(p.Drivers, wantDrivers) {
		t.Errorf("Drivers = %v, want %v", p.Drivers, wantDrivers)
	}
	wantPlugins := []string{"gitlab-runners-ha", "postgres-ha"}
	if !equalStringSlice(p.PluginsInstalled, wantPlugins) {
		t.Errorf("PluginsInstalled = %v, want %v", p.PluginsInstalled, wantPlugins)
	}
	if p.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", p.GoVersion, runtime.Version())
	}
	if p.OS != runtime.GOOS || p.Arch != runtime.GOARCH {
		t.Errorf("OS/Arch = %q/%q, want %q/%q", p.OS, p.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if p.UptimeSeconds != 3600 {
		t.Errorf("UptimeSeconds = %d, want 3600", p.UptimeSeconds)
	}
	if p.Version != AgentVersion {
		t.Errorf("Version = %q, want %q", p.Version, AgentVersion)
	}

	// LastSentAt must have advanced. memStore.s reflects the
	// post-Send state because Send synchronously calls SaveState.
	if store.s.LastSentAt.IsZero() {
		t.Errorf("LastSentAt not updated after successful send")
	}
}

// Test 3 — 5xx retried up to 3 times ; 4xx NOT retried.
func TestSend_RetryPolicy(t *testing.T) {
	t.Run("5xx_retries_then_succeeds", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			if n < 3 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		s := New(Options{
			Store:  newEnabledStore(srv.URL),
			Source: stubSource{},
		})
		if err := s.Send(context.Background()); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if got := calls.Load(); got != 3 {
			t.Errorf("calls = %d, want 3 (two 500s then a 200)", got)
		}
	})

	t.Run("4xx_not_retried", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		}))
		defer srv.Close()
		s := New(Options{Store: newEnabledStore(srv.URL), Source: stubSource{}})
		if err := s.Send(context.Background()); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("calls = %d, want 1 (4xx is terminal)", got)
		}
	})
}

// Test 4 — Payload shape never includes PII. Parse the body and
// assert no UUID-shaped fields slip in (canary against future
// additions to Snapshot leaking into Payload by accident).
func TestSend_PayloadNeverIncludesPII(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newEnabledStore(srv.URL)
	src := stubSource{snap: Snapshot{
		HostCount:        2,
		VMCountRunning:   1,
		Drivers:          []string{"qemu"},
		PluginsInstalled: []string{"postgres-ha"},
	}}
	s := New(Options{Store: store, Source: src})
	if err := s.Send(context.Background()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Parse into a generic map — any future fields show up here.
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	allowed := map[string]bool{
		"anonymous_id": true, "version": true,
		"host_count": true, "vm_count_running": true,
		"drivers": true, "plugins_installed": true,
		"go_version": true, "os": true, "arch": true,
		"uptime_seconds": true,
	}
	for k := range raw {
		if !allowed[k] {
			t.Errorf("payload contains unexpected field %q — review PII contract before adding", k)
		}
	}
	// UUID-shape canary. anonymous_id is 16 hex chars (passes), so
	// we look for 32-hex or the 8-4-4-4-12 dashed form, which are
	// the two UUID shapes used elsewhere in weft.
	uuidPat := regexp.MustCompile(`(?i)\b[0-9a-f]{32}\b|\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	if loc := uuidPat.FindIndex(gotBody); loc != nil {
		t.Errorf("payload contains a UUID-shaped substring at offset %d: %s", loc[0], string(gotBody[loc[0]:loc[1]]))
	}
	// Forbid-list canary — substrings that would indicate a leak.
	for _, banned := range []string{"@", "192.168.", "10.0.", "fe80:", "Bearer "} {
		if bytes.Contains(gotBody, []byte(banned)) {
			t.Errorf("payload contains forbidden substring %q", banned)
		}
	}
}

// Test 5 — anonymous_id is stable across two preview calls.
func TestBuildPayload_AnonymousIDStable(t *testing.T) {
	store := newEnabledStore("")
	src := stubSource{snap: Snapshot{}}
	s := New(Options{Store: store, Source: src})
	p1, _, err := s.BuildPayload(context.Background())
	if err != nil {
		t.Fatalf("BuildPayload #1: %v", err)
	}
	p2, _, err := s.BuildPayload(context.Background())
	if err != nil {
		t.Fatalf("BuildPayload #2: %v", err)
	}
	if p1.AnonymousID == "" {
		t.Fatalf("AnonymousID empty")
	}
	if p1.AnonymousID != p2.AnonymousID {
		t.Errorf("AnonymousID drifted: %q != %q", p1.AnonymousID, p2.AnonymousID)
	}
	// Direct hash check ; AnonymousID is sha256(cluster+install)[:16].
	want := AnonymousID(store.s.ClusterUUID, store.s.InstallDate)
	if p1.AnonymousID != want {
		t.Errorf("AnonymousID = %q, want %q", p1.AnonymousID, want)
	}
}

// Test 6 — Send is cancellable via ctx. The HTTPClient stub blocks
// forever on Do, watching for ctx cancellation. We don't use an
// httptest.Server here because its Close() waits on every in-flight
// handler, which would deadlock against a hung handler ; the
// HTTPClient interface gives us a cleaner injection point.
func TestSend_CancellableViaContext(t *testing.T) {
	hc := blockingHTTPClient{}
	ctx, cancel := context.WithCancel(context.Background())
	store := newEnabledStore("https://unused.example/")
	s := New(Options{Store: store, Source: stubSource{}, Client: hc})

	done := make(chan error, 1)
	go func() { done <- s.Send(ctx) }()

	// Give the goroutine a moment to enter Do, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		// Either nil (Send swallows transport errors as no-op
		// after retries) or context.Canceled — both are
		// acceptable. The contract we test is that Send returns ;
		// we don't deadlock.
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("Send returned non-ctx err on cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not return within 5s after ctx cancel")
	}
}

// blockingHTTPClient.Do blocks until the request context fires,
// then returns context.Canceled. Mirrors what a real *http.Client
// does for a hung connection without dragging in net/http server
// teardown semantics.
type blockingHTTPClient struct{}

func (blockingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// Test 7 — Endpoint empty (enabled but unconfigured) is a no-op.
// Sits alongside the six required tests because the empty-endpoint
// branch is the most likely place a "default endpoint = none" policy
// regression would land.
func TestSend_EmptyEndpointIsNoop(t *testing.T) {
	store := &memStore{s: State{Enabled: true}} // no endpoint
	src := stubSource{}
	s := New(Options{Store: store, Source: src})
	if err := s.Send(context.Background()); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// --- BlobStore round-trip -------------------------------------------

type fakeBlob struct {
	mu  sync.Mutex
	buf []byte
}

func (f *fakeBlob) Load(_ context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buf == nil {
		return nil, nil
	}
	out := make([]byte, len(f.buf))
	copy(out, f.buf)
	return out, nil
}

func (f *fakeBlob) Save(_ context.Context, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf = append([]byte(nil), b...)
	return nil
}

func TestBlobStore_RoundTrip(t *testing.T) {
	fb := &fakeBlob{}
	bs := NewBlobStore(fb)
	// Fresh-install path : Load on an empty backend returns the
	// zero State, no error.
	st, err := bs.LoadState(context.Background())
	if err != nil {
		t.Fatalf("LoadState fresh: %v", err)
	}
	if st.Enabled {
		t.Errorf("fresh state Enabled = true, want false")
	}
	st.Enabled = true
	st.Endpoint = "https://example/telemetry"
	st.ClusterUUID = "deadbeef"
	st.InstallDate = "2026-06-02"
	if err := bs.SaveState(context.Background(), st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := bs.LoadState(context.Background())
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Endpoint != st.Endpoint || !got.Enabled || got.ClusterUUID != "deadbeef" {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, st)
	}
	if !strings.Contains(string(fb.buf), `"enabled": true`) {
		t.Errorf("on-disk blob not human-readable JSON: %s", fb.buf)
	}
}

// equalStringSlice avoids reflect.DeepEqual for the slice-equality
// asserts (faster + clearer error message).
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
