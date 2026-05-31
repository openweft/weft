package main

// rpc_metrics_test.go pins the per-driver-kind metric pipeline :
// the interceptor installs a kind slot on ctx, the handler stamps the
// kind via RecordRPCKind, and the counter increments at the right
// label combination. The end-to-end test simulates two VM-lifecycle
// RPCs on a host that has both drivers (vz + qemu) and asserts that
// each driver_kind ticks once.
//
// The test deliberately uses a synthetic handler (not a real gRPC
// server) so it stays hermetic — no Unix socket, no proto-generated
// stubs — and runs in milliseconds.

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// counterValueByLabels gathers the counter family from `reg` and
// returns the value for the matching label set. Returns 0 when no
// sample is present — explicit so the assertion side can tell apart
// "metric absent" from "metric present at 0".
func counterValueByLabels(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("registry.Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !labelsMatch(m, labels) {
				continue
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
		}
	}
	return 0
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// TestRPCMetrics_PerKindLabel exercises two RPCs on a single host
// that runs both vz + qemu (Apple Silicon multi-plugin) :
//
//   - call /weft.WeftAgent/StartVM on an arm64 VM → stamps "vz"
//   - call /weft.WeftAgent/StartVM on an amd64 VM → stamps "qemu"
//
// Asserts the counter family `weft_rpc_total` carries one sample at
// each driver_kind, both with value 1. The label combination is the
// alert seam — operators run
// `rate(weft_rpc_total{driver_kind="qemu", code!="OK"}[5m])` against it.
func TestRPCMetrics_PerKindLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := newRPCMetrics(reg)
	if err != nil {
		t.Fatalf("newRPCMetrics: %v", err)
	}

	// Synthetic handlers that mimic StartVM's stamp-then-act pattern.
	// The interceptor wraps each — same chain the production server
	// builds in cmd/weft/main.go (auth → grpcprom → rpcByKind).
	interceptor := m.UnaryInterceptor()

	runRPC := func(method, kind string, returnErr error) error {
		info := &grpc.UnaryServerInfo{FullMethod: method}
		handler := func(ctx context.Context, _ interface{}) (interface{}, error) {
			// Stamp the kind as a real handler would after looking it
			// up via Adapter.LookupKindForVM.
			RecordRPCKind(ctx, kind)
			return struct{}{}, returnErr
		}
		_, err := interceptor(context.Background(), nil, info, handler)
		return err
	}

	// arm64 VM → vz
	if err := runRPC("/weft.WeftAgent/StartVM", "vz", nil); err != nil {
		t.Fatalf("vz RPC: %v", err)
	}
	// amd64 VM → qemu
	if err := runRPC("/weft.WeftAgent/StartVM", "qemu", nil); err != nil {
		t.Fatalf("qemu RPC: %v", err)
	}

	if got := counterValueByLabels(t, reg, "weft_rpc_total", map[string]string{
		"method":      "/weft.WeftAgent/StartVM",
		"code":        "OK",
		"driver_kind": "vz",
	}); got != 1 {
		t.Errorf("weft_rpc_total{vz, OK} = %v ; want 1", got)
	}
	if got := counterValueByLabels(t, reg, "weft_rpc_total", map[string]string{
		"method":      "/weft.WeftAgent/StartVM",
		"code":        "OK",
		"driver_kind": "qemu",
	}); got != 1 {
		t.Errorf("weft_rpc_total{qemu, OK} = %v ; want 1", got)
	}
}

// TestRPCMetrics_ErrorCode pins the error path : a failing handler
// labels the counter with the gRPC status code, so the alert
// `rate(weft_rpc_total{driver_kind="qemu", code!="OK"}[5m])` actually
// fires on QEMU-specific failures.
func TestRPCMetrics_ErrorCode(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := newRPCMetrics(reg)
	if err != nil {
		t.Fatalf("newRPCMetrics: %v", err)
	}
	interceptor := m.UnaryInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/weft.WeftAgent/StopVM"}
	handler := func(ctx context.Context, _ interface{}) (interface{}, error) {
		RecordRPCKind(ctx, "qemu")
		return nil, status.Error(codes.Internal, "qemu boom")
	}
	if _, err := interceptor(context.Background(), nil, info, handler); err == nil {
		t.Fatal("expected error")
	}
	if got := counterValueByLabels(t, reg, "weft_rpc_total", map[string]string{
		"method":      "/weft.WeftAgent/StopVM",
		"code":        "Internal",
		"driver_kind": "qemu",
	}); got != 1 {
		t.Errorf("weft_rpc_total{qemu, Internal} = %v ; want 1", got)
	}

	// Plain (non-status) errors map to "Unknown" — same convention as
	// google.golang.org/grpc/status.Code.
	handler2 := func(ctx context.Context, _ interface{}) (interface{}, error) {
		RecordRPCKind(ctx, "vz")
		return nil, errors.New("plain boom")
	}
	if _, err := interceptor(context.Background(), nil, info, handler2); err == nil {
		t.Fatal("expected error")
	}
	if got := counterValueByLabels(t, reg, "weft_rpc_total", map[string]string{
		"method":      "/weft.WeftAgent/StopVM",
		"code":        "Unknown",
		"driver_kind": "vz",
	}); got != 1 {
		t.Errorf("weft_rpc_total{vz, Unknown} = %v ; want 1", got)
	}
}

// TestRPCMetrics_NoStampEmptyLabel — handlers that don't route through
// a driver (host registry / scheduling rules / projects) leave the
// kind slot untouched. The counter records `driver_kind=""` so the
// non-routed traffic stays visible without being attributed to any
// driver (which would skew per-driver alert thresholds).
func TestRPCMetrics_NoStampEmptyLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := newRPCMetrics(reg)
	if err != nil {
		t.Fatalf("newRPCMetrics: %v", err)
	}
	interceptor := m.UnaryInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/weft.WeftAgent/ListProjects"}
	handler := func(ctx context.Context, _ interface{}) (interface{}, error) {
		// No RecordRPCKind call : registry RPC, not driver-routed.
		return struct{}{}, nil
	}
	if _, err := interceptor(context.Background(), nil, info, handler); err != nil {
		t.Fatalf("RPC: %v", err)
	}
	if got := counterValueByLabels(t, reg, "weft_rpc_total", map[string]string{
		"method":      "/weft.WeftAgent/ListProjects",
		"code":        "OK",
		"driver_kind": "",
	}); got != 1 {
		t.Errorf("weft_rpc_total{driver_kind=\"\"} = %v ; want 1", got)
	}
}

// TestRecordRPCKind_NoSlot — RecordRPCKind on a bare context (no
// interceptor) is a safe no-op. Pins the contract so handlers that
// happen to run outside the gRPC chain (in-process tests / direct
// invocation) don't panic.
func TestRecordRPCKind_NoSlot(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RecordRPCKind without slot panicked: %v", r)
		}
	}()
	RecordRPCKind(context.Background(), "vz")
}
