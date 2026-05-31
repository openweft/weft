package main

// rpc_metrics.go wires a `weft_rpc_total{method, code, driver_kind}`
// counter alongside the stock `grpc_server_handled_total` already
// registered by go-grpc-middleware. The point of the new counter is
// the `driver_kind` label : it reports which driver plugin handled
// the RPC ("vz" / "qemu" / future siblings) on multi-plugin hosts,
// the legacy hypervisor label ("apple-vz" / "qemu") on single-plugin
// hosts, or "" for RPCs that don't touch a driver (host registry,
// scheduling rules, projects, …).
//
// Operators alert on per-driver error rates with PromQL like :
//
//	rate(weft_rpc_total{driver_kind="qemu", code!="OK"}[5m]) > 0.1
//
// without false positives from VZ workloads (and symmetrically for VZ).
//
// Implementation seam : the unary + stream interceptors create a
// per-call placeholder (*string) in ctx ; handlers that route through
// the driver dispatch call RecordRPCKind(ctx, kind) to stamp the kind.
// The interceptor reads the slot AFTER the handler returns and emits
// one counter sample per RPC. Handlers that don't stamp leave the
// slot empty — the metric records `driver_kind=""` which keeps host-
// registry / projects / scheduling-rule traffic visible without
// labelling it as belonging to any driver.

import (
	"context"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// rpcKindCtxKey is the context-value key under which the per-call
// driver-kind slot lives. Unexported to keep the recorder API the
// only legal way to write it (no `ctx.Value("driver_kind")` foot-guns).
type rpcKindCtxKey struct{}

// withRPCKindSlot installs a fresh placeholder on ctx so handlers can
// later stamp the resolved driver kind via RecordRPCKind. The slot is
// a *string under a mutex — handlers and the interceptor read/write
// it across goroutines (stream RPCs in particular).
type rpcKindSlot struct {
	mu   sync.Mutex
	kind string
}

func (s *rpcKindSlot) set(k string) {
	s.mu.Lock()
	s.kind = k
	s.mu.Unlock()
}

func (s *rpcKindSlot) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kind
}

func withRPCKindSlot(ctx context.Context) (context.Context, *rpcKindSlot) {
	s := &rpcKindSlot{}
	return context.WithValue(ctx, rpcKindCtxKey{}, s), s
}

// RecordRPCKind stamps the driver kind that handled the current RPC
// onto the per-call slot installed by the interceptor. Safe to call
// from any goroutine ; safe to call from a handler that ran without
// the interceptor (no-op when the slot is absent).
//
// Call this AFTER dispatch resolution — typically right after
// hypervisorForVM / HostHandleOnArch picks a handle. Calling it
// multiple times overwrites ; the LAST stamp wins (mirrors the actual
// driver that fielded the call when a handler dispatches twice).
func RecordRPCKind(ctx context.Context, kind string) {
	if v := ctx.Value(rpcKindCtxKey{}); v != nil {
		if s, ok := v.(*rpcKindSlot); ok {
			s.set(kind)
		}
	}
}

// rpcKindFromContext is the interceptor-side read. Returns "" when no
// slot is present OR when the handler didn't stamp — both mean "this
// RPC did not route through a driver" and the empty label captures it.
func rpcKindFromContext(ctx context.Context) string {
	if v := ctx.Value(rpcKindCtxKey{}); v != nil {
		if s, ok := v.(*rpcKindSlot); ok {
			return s.get()
		}
	}
	return ""
}

// newRPCMetrics constructs the per-driver counter + the matching
// pair of interceptors. The counter is registered against the caller-
// supplied Registry (same `*prometheus.Registry` the gRPC handling
// histogram lives on, so a single scrape sees both).
//
// Returning the metric handle lets tests exercise the counter directly
// (gather + walk families) without standing up the HTTP server.
type rpcMetrics struct {
	total *prometheus.CounterVec
}

func newRPCMetrics(reg prometheus.Registerer) (*rpcMetrics, error) {
	m := &rpcMetrics{
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "weft_rpc_total",
			Help: "Total RPCs handled by weft-agent, labelled by gRPC method, gRPC status code, and the driver kind that handled the request (\"vz\" / \"qemu\" for multi-plugin hosts ; the legacy hypervisor label for single-plugin hosts ; empty for RPCs that did not route through a driver).",
		}, []string{"method", "code", "driver_kind"}),
	}
	if err := reg.Register(m.total); err != nil {
		return nil, err
	}
	return m, nil
}

// UnaryInterceptor returns the unary server-side interceptor that
// installs a kind slot on ctx, runs the handler, and records one
// counter sample once the handler returns. Status-code extraction
// mirrors go-grpc-middleware : errors stamped through grpc/status
// carry their canonical code, other errors fall through as "Unknown",
// and nil errors map to "OK".
func (m *rpcMetrics) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		ctx, slot := withRPCKindSlot(ctx)
		resp, err := handler(ctx, req)
		code := status.Code(err).String()
		m.total.WithLabelValues(info.FullMethod, code, slot.get()).Inc()
		return resp, err
	}
}

// StreamInterceptor mirrors UnaryInterceptor for stream RPCs. The
// kind slot is installed by wrapping the incoming ServerStream so
// handler-side ctx.Value lookups find it.
func (m *rpcMetrics) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, slot := withRPCKindSlot(ss.Context())
		err := handler(srv, &kindSlotStream{ServerStream: ss, ctx: ctx})
		code := status.Code(err).String()
		m.total.WithLabelValues(info.FullMethod, code, slot.get()).Inc()
		return err
	}
}

// kindSlotStream overrides Context() so the kind slot we put in ctx
// reaches the handler. Standard pattern for ServerStream wrapping —
// see go-grpc-middleware's wrappedServerStream for prior art.
type kindSlotStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (k *kindSlotStream) Context() context.Context { return k.ctx }
