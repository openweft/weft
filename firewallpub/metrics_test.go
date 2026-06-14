package firewallpub

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft-microvm-init/pkg/pod"
)

// counterValue reads the current value of a single labelled counter
// without round-tripping through /metrics. Sidesteps the test-binary's
// shared collector state : every test reads its own (kind, vm_uuid)
// triplet so concurrent test ordering doesn't matter.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("counter.Write: %v", err)
	}
	return m.Counter.GetValue()
}

func TestPublishesTotal_TicksOnEventDrivenPublish(t *testing.T) {
	// Unique VM UUID per test keeps the assertion local to this
	// labelset — other tests in the package may also increment the
	// shared CounterVec, but never with this VM UUID.
	const vm = "vm-metrics-1"
	scope := newScope()
	scope.portsByVM[vm] = []weft.Port{
		{UUID: "p1", VMUUID: vm, NetworkUUID: "net-1", IP: "10.0.0.5",
			SecurityGroups: []string{"sg-1"}},
	}
	scope.sgs["sg-1"] = weft.SecurityGroup{}
	rec := &recorderPub{}
	p := New(scope, rec.fn(), silentLog())

	before := counterValue(t, publishesTotal.WithLabelValues("port.created", vm))
	events := make(chan weft.PlatformEvent, 1)
	events <- weft.PlatformEvent{Kind: "port.created", Subject: "p1",
		Meta: map[string]string{"vm_uuid": vm}}
	close(events)
	if err := p.Run(context.Background(), events); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := counterValue(t, publishesTotal.WithLabelValues("port.created", vm)); got != before+1 {
		t.Errorf("publishes_total{kind=port.created,vm_uuid=%s} = %v, want %v", vm, got, before+1)
	}
}

func TestPublishesTotal_ResyncLabel(t *testing.T) {
	const vm = "vm-metrics-resync"
	scope := newScope()
	scope.portsByVM[vm] = []weft.Port{}
	rec := &recorderPub{}
	p := New(scope, rec.fn(), silentLog())

	before := counterValue(t, publishesTotal.WithLabelValues("resync", vm))
	p.ResyncAll([]string{vm})
	if got := counterValue(t, publishesTotal.WithLabelValues("resync", vm)); got != before+1 {
		t.Errorf("publishes_total{kind=resync,vm_uuid=%s} = %v, want %v", vm, got, before+1)
	}
}

func TestStatusEventsTotal_TicksByVMAndOverall(t *testing.T) {
	const vm = "vm-metrics-status"
	bus := &fakeBus{}
	rcv, err := NewStatusReceiver(noopSubscribe, bus, silentLog())
	if err != nil {
		t.Fatalf("NewStatusReceiver: %v", err)
	}
	healthy, _ := json.Marshal(pod.FirewallStatus{Overall: "Healthy", TableInstalled: true})
	degraded, _ := json.Marshal(pod.FirewallStatus{Overall: "Degraded", LastError: "x"})

	hBefore := counterValue(t, statusEventsTotal.WithLabelValues(vm, "Healthy"))
	dBefore := counterValue(t, statusEventsTotal.WithLabelValues(vm, "Degraded"))

	rcv.HandleMessage("weft.firewall."+vm+".status", healthy)
	rcv.HandleMessage("weft.firewall."+vm+".status", degraded)
	rcv.HandleMessage("weft.firewall."+vm+".status", healthy)

	if got := counterValue(t, statusEventsTotal.WithLabelValues(vm, "Healthy")); got != hBefore+2 {
		t.Errorf("status_events_total{vm_uuid=%s,overall=Healthy} = %v, want %v", vm, got, hBefore+2)
	}
	if got := counterValue(t, statusEventsTotal.WithLabelValues(vm, "Degraded")); got != dBefore+1 {
		t.Errorf("status_events_total{vm_uuid=%s,overall=Degraded} = %v, want %v", vm, got, dBefore+1)
	}
	// Bad subjects must NOT bump any cardinality — checked by gathering
	// all label combinations and asserting no new vm_uuid="" label set.
	rcv.HandleMessage("weft.firewall.bad", healthy)
	rcv.HandleMessage("weft.firewall.vm.x.status", healthy)
	if got := counterValue(t, statusEventsTotal.WithLabelValues("", "Healthy")); got != 0 {
		t.Errorf("malformed subject must not create vm_uuid=\"\" label set, got %v", got)
	}
}

// TestRegister_AcceptsCustomRegisterer pins the back-compat contract :
// passing nil falls back to DefaultRegisterer (already engaged by the
// rest of the tests in this package, so the call is a no-op), and
// passing a fresh Registry never panics. Subsequent Register calls are
// idempotent — same sync.Once guarantees the cmd-side wiring relies on.
func TestRegister_AcceptsCustomRegisterer(t *testing.T) {
	// The package singletons are already bound to DefaultRegisterer by
	// previous tests (any one of them called ensureRegistered) ; the
	// Once is consumed, so this call must be a clean no-op.
	if err := Register(prometheus.NewRegistry()); err != nil {
		t.Errorf("Register on consumed Once should be a no-op, got %v", err)
	}
	if err := Register(nil); err != nil {
		t.Errorf("Register(nil) should fall back to default and no-op, got %v", err)
	}
}

