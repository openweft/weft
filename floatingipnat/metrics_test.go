package floatingipnat

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// counterValue + gaugeValue + histogramCount read the live value of
// the package-level collectors without going through /metrics.
// Tests are robust to other tests bumping the same collectors :
// assertions are written as deltas around the call-under-test, not
// absolute values.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("counter.Write: %v", err)
	}
	return m.Counter.GetValue()
}

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("gauge.Write: %v", err)
	}
	return m.Gauge.GetValue()
}

func histogramCount(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	var m dto.Metric
	if err := h.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("histogram.Write: %v", err)
	}
	return m.Histogram.GetSampleCount()
}

func TestApplyTotal_OKResult_OnSuccessfulApply(t *testing.T) {
	r := NewStubReconciler()
	before := counterValue(t, applyTotal.WithLabelValues("ok"))
	beforeCount := histogramCount(t, applyDuration)

	if err := r.Apply([]NATMapping{
		{PublicIP: "203.0.113.5", PrivateIP: "10.0.0.5", VMName: "metrics-ok"},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := counterValue(t, applyTotal.WithLabelValues("ok")); got != before+1 {
		t.Errorf("apply_total{result=ok} = %v, want %v", got, before+1)
	}
	if got := histogramCount(t, applyDuration); got != beforeCount+1 {
		t.Errorf("apply_duration_seconds count = %v, want %v", got, beforeCount+1)
	}
	if got := gaugeValue(t, rulesInstalled); got != 1 {
		t.Errorf("rules_installed = %v, want 1 (one mapping installed)", got)
	}
}

func TestApplyTotal_ErrResult_OnValidationFailure(t *testing.T) {
	r := NewStubReconciler()
	// Seed a successful Apply so the gauge is at a known non-zero value,
	// then prove a failed Apply does NOT reset it.
	if err := r.Apply([]NATMapping{
		{PublicIP: "203.0.113.42", PrivateIP: "10.0.0.42"},
	}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	gaugeBefore := gaugeValue(t, rulesInstalled)
	errBefore := counterValue(t, applyTotal.WithLabelValues("err"))

	// Duplicate PublicIP — ValidateMappings rejects.
	if err := r.Apply([]NATMapping{
		{PublicIP: "203.0.113.9", PrivateIP: "10.0.0.5"},
		{PublicIP: "203.0.113.9", PrivateIP: "10.0.0.6"},
	}); err == nil {
		t.Fatal("Apply should reject duplicate public_ip")
	}
	if got := counterValue(t, applyTotal.WithLabelValues("err")); got != errBefore+1 {
		t.Errorf("apply_total{result=err} = %v, want %v", got, errBefore+1)
	}
	if got := gaugeValue(t, rulesInstalled); got != gaugeBefore {
		t.Errorf("rules_installed must not move on failed Apply : got %v, want %v", got, gaugeBefore)
	}
}

func TestRulesInstalled_TracksLastSuccessfulMappingCount(t *testing.T) {
	r := NewStubReconciler()
	if err := r.Apply([]NATMapping{
		{PublicIP: "203.0.113.1", PrivateIP: "10.0.0.1"},
		{PublicIP: "203.0.113.2", PrivateIP: "10.0.0.2"},
		{PublicIP: "203.0.113.3", PrivateIP: "10.0.0.3"},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := gaugeValue(t, rulesInstalled); got != 3 {
		t.Errorf("rules_installed after 3-mapping Apply = %v, want 3", got)
	}
	// Converging to empty must update the gauge to 0 — an empty
	// successful Apply is a legitimate "drain" state, not a failure.
	if err := r.Apply(nil); err != nil {
		t.Fatalf("empty Apply: %v", err)
	}
	if got := gaugeValue(t, rulesInstalled); got != 0 {
		t.Errorf("rules_installed after drain = %v, want 0", got)
	}
}

// TestRegister_AcceptsCustomRegisterer pins the back-compat contract :
// once the package-level Once has fired (any one of the tests above
// triggered ensureRegistered) Register returns nil regardless of the
// passed registerer. Mirrors the firewallpub test of the same shape.
func TestRegister_AcceptsCustomRegisterer(t *testing.T) {
	if err := Register(prometheus.NewRegistry()); err != nil {
		t.Errorf("Register on consumed Once should be a no-op, got %v", err)
	}
	if err := Register(nil); err != nil {
		t.Errorf("Register(nil) should fall back to default and no-op, got %v", err)
	}
}
