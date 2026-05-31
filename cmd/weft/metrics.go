package main

// metrics.go wires Prometheus observability into the weft-agent
// process : a fresh registry (not the prom global default — explicit
// registries are recommended best-practice so unrelated client_golang
// users don't leak metrics into ours), the process + Go runtime
// collectors, and an HTTP listener serving `/metrics`.
//
// The gRPC server-side interceptor (`grpc_server_handling_seconds`
// histogram + the per-call counters) is registered against the same
// registry by the caller — see `startMetricsServer` returning both
// the closer and the *prometheus.Registry.
//
// Disabled by default : the operator opts in by setting
// `--metrics-listen=:9101` (or `metrics_listen = ":9101"` in the HCL
// config). Empty listen = no listener, no registry, no overhead.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// startMetricsServer builds a dedicated Prometheus registry, registers
// the process + Go runtime collectors on it, and starts an HTTP listener
// that serves the standard `/metrics` endpoint. The returned registry is
// the seam the caller uses to add subsystem collectors (today: the gRPC
// server-side handling-time histogram via go-grpc-middleware).
//
// The closer returns from a context-bounded Shutdown so the listener
// flushes in-flight scrape responses before the process exits.
func startMetricsServer(addr string, logger *log.Logger) (*prometheus.Registry, func() error, error) {
	reg := prometheus.NewRegistry()
	// Process + Go runtime collectors — `process_cpu_seconds_total`,
	// `process_resident_memory_bytes`, `go_gc_duration_seconds`,
	// `go_goroutines`, etc. Equivalent to the `collectors`
	// subpackage's NewProcessCollector / NewGoCollector ; we use the
	// in-package functions (kept for backwards compat) so we don't
	// need an extra import path the vendored snapshot doesn't ship.
	if err := reg.Register(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{})); err != nil {
		return nil, nil, fmt.Errorf("register process collector: %w", err)
	}
	if err := reg.Register(prometheus.NewGoCollector()); err != nil {
		return nil, nil, fmt.Errorf("register go collector: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		Registry: reg,
	}))

	// Bind the listener before returning so the caller learns of bind
	// errors synchronously (a port-conflict on :9101 should be loud at
	// boot, not surface 10s later from a goroutine log line).
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("metrics listen %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if logger != nil {
				logger.Printf("metrics server stopped: %v", err)
			}
		}
	}()
	if logger != nil {
		logger.Printf("metrics endpoint listening on %s/metrics", lis.Addr().String())
	}

	closer := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
	return reg, closer, nil
}
