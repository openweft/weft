package infra

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHealthURL_Substitutes pins the $VM_IP substitution. The
// guest IP discovered at deploy time replaces the literal token
// wherever it appears in the plan's health.cmd URL.
func TestHealthURL_Substitutes(t *testing.T) {
	p := &Plan{
		Health: &HealthBlk{
			Type:   "http",
			Cmd:    "http://$VM_IP:8222/healthz",
			Period: "2s",
		},
	}
	got, err := HealthURL(p, "10.0.0.42")
	if err != nil {
		t.Fatalf("HealthURL: %v", err)
	}
	if got != "http://10.0.0.42:8222/healthz" {
		t.Errorf("got %q, want http://10.0.0.42:8222/healthz", got)
	}
}

// TestHealthURL_WordBoundary pins the regex-bounded matcher :
// $VM_IP doesn't bleed into longer identifiers. A plan that
// accidentally writes $VM_IP_V4 keeps the second token intact.
func TestHealthURL_WordBoundary(t *testing.T) {
	p := &Plan{Health: &HealthBlk{Type: "http", Cmd: "http://$VM_IP/$VM_IP_v4"}}
	got, _ := HealthURL(p, "10.0.0.1")
	if got != "http://10.0.0.1/$VM_IP_v4" {
		t.Errorf("got %q, want http://10.0.0.1/$VM_IP_v4", got)
	}
}

// TestHealthURL_RejectsUnsupportedType : only http probes are
// implemented today ; exec probes need ExecInVM plumbing and
// surface a clear error rather than silently skipping.
func TestHealthURL_RejectsUnsupportedType(t *testing.T) {
	p := &Plan{Health: &HealthBlk{Type: "exec", Cmd: "/usr/bin/healthcheck"}}
	_, err := HealthURL(p, "10.0.0.1")
	if err == nil {
		t.Fatal("expected error for exec probe (not implemented)")
	}
	if !strings.Contains(err.Error(), "exec") || !strings.Contains(err.Error(), "http") {
		t.Errorf("error should name the unsupported type + the supported alternative, got: %v", err)
	}
}

// TestHealthURL_RejectsNilBlock + EmptyCmd cover the loader-
// defaulted defensive paths : a plan without a health block,
// or with an empty Cmd, can't be probed and surfaces the
// reason.
func TestHealthURL_RejectsNilBlock(t *testing.T) {
	if _, err := HealthURL(&Plan{}, "10.0.0.1"); err == nil {
		t.Fatal("expected error for nil health block")
	}
	if _, err := HealthURL(&Plan{Health: &HealthBlk{Type: "http"}}, "10.0.0.1"); err == nil {
		t.Fatal("expected error for empty health.cmd")
	}
}

// TestHealthPeriod pins the period parsing + default. Bad
// strings, zero, and negative all collapse to the 5s default
// so the deployer never blocks on a nonsensical interval.
func TestHealthPeriod(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 5 * time.Second},
		{"2s", 2 * time.Second},
		{"1m", time.Minute},
		{"garbage", 5 * time.Second},
		{"-5s", 5 * time.Second},
		{"0s", 5 * time.Second},
	}
	for _, c := range cases {
		p := &Plan{Health: &HealthBlk{Type: "http", Cmd: "x", Period: c.in}}
		if got := HealthPeriod(p); got != c.want {
			t.Errorf("period=%q: got %v, want %v", c.in, got, c.want)
		}
	}
}

// TestWaitHealthy_SuccessAfterRetries spins a tiny HTTP server
// that returns 503 a couple of times then 200, and confirms
// WaitHealthy waits through the failures and returns nil once
// the service comes up.
func TestWaitHealthy_SuccessAfterRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := WaitHealthy(context.Background(), srv.URL, 3*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitHealthy: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got < 3 {
		t.Errorf("only %d probes — expected at least 3", got)
	}
}

// TestWaitHealthy_TimeoutSurfacesLastError exercises the
// budget-exhausted path : the server always returns 503, so
// WaitHealthy times out. The error should name the URL,
// elapsed budget, and the last underlying issue.
func TestWaitHealthy_TimeoutSurfacesLastError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := WaitHealthy(context.Background(), srv.URL, 200*time.Millisecond, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), srv.URL) || !strings.Contains(err.Error(), "503") {
		t.Errorf("error should name URL + last status: %v", err)
	}
}

// TestWaitHealthy_ContextCancel : a parent context cancel
// should abort the loop and surface the cancellation.
func TestWaitHealthy_ContextCancel(t *testing.T) {
	// Server that never responds in time — context cancel races
	// against the per-request timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := WaitHealthy(ctx, srv.URL, 5*time.Second, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected context-cancel error")
	}
}
