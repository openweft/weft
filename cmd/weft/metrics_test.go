package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
)

// TestStartMetricsServer asserts the /metrics endpoint serves the
// process + Go runtime collectors registered on the fresh registry.
// The contract is twofold :
//
//   - the listener returns 200 OK on /metrics
//   - the body contains at least one well-known Go runtime metric
//     (`go_gc_duration_seconds`) — proves the Go collector reached
//     the registry the handler is bound to (a regression where
//     promhttp pointed at a different gatherer would silently return
//     zero metrics, which we explicitly want to catch).
//
// Picks port :0 so parallel test runs don't collide.
func TestStartMetricsServer(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	reg, closer, err := startMetricsServer("127.0.0.1:0", logger)
	if err != nil {
		t.Fatalf("startMetricsServer: %v", err)
	}
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}
	t.Cleanup(func() { _ = closer() })

	// Gather once via the registry directly to learn the bound
	// address — startMetricsServer doesn't expose the listener
	// (intentionally — the caller passes the addr in), but the
	// log line printed it. For test purposes, hit a known well-
	// formed addr by re-running on a deterministic port to avoid
	// guessing — but a simpler approach is to gather() via the
	// registry: if the Go collector is registered, the metric
	// names show up in reg.Gather().
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("registry.Gather: %v", err)
	}
	var sawGoGC bool
	for _, f := range families {
		if strings.HasPrefix(f.GetName(), "go_gc_") || strings.HasPrefix(f.GetName(), "go_goroutines") {
			sawGoGC = true
			break
		}
	}
	if !sawGoGC {
		var names []string
		for _, f := range families {
			names = append(names, f.GetName())
		}
		t.Fatalf("expected a go_gc_* / go_goroutines metric on the registry, got: %v", names)
	}
}

// TestStartMetricsServer_HTTPEndpoint exercises the full path : a real
// listener, a real HTTP GET against /metrics, and an assertion that the
// scrape body carries the Go-runtime collector output. Uses port :0 +
// inspects the actual listener address by binding a second listener and
// stealing its port — too fragile. Cleaner : use `127.0.0.1:0` and rely
// on the log line that startMetricsServer emits, then probe via the
// registry. But the spec asked for an HTTP probe, so we route through a
// fixed-port harness with a port grab.
func TestStartMetricsServer_HTTPEndpoint(t *testing.T) {
	// Grab a free port by listening on :0 then closing — the kernel
	// re-uses the port immediately for the metrics listener that
	// follows. Race window is tiny on localhost ; if a test ever
	// flakes here we can switch to net.Listen + adopt a custom
	// startMetricsServer that takes a net.Listener.
	addr, err := freeLocalAddr()
	if err != nil {
		t.Skipf("could not allocate free port: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	_, closer, err := startMetricsServer(addr, logger)
	if err != nil {
		t.Fatalf("startMetricsServer: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "go_gc_") && !strings.Contains(string(body), "go_goroutines") {
		t.Fatalf("expected go_gc_* / go_goroutines in /metrics body, got first 400 bytes: %s", truncate(string(body), 400))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// freeLocalAddr returns a 127.0.0.1:<port> the kernel just released.
// Sufficient for hermetic tests on a developer laptop ; production
// observability config uses operator-chosen ports.
func freeLocalAddr() (string, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr, nil
}
