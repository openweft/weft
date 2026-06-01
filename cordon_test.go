package weft

// cordon_test.go covers the per-host `cordoned` flag end-to-end :
// the registry-level toggle, idempotency, the scheduler's
// pre-filtering pass, and the multi-replica picker. The
// admin-gate test for the RPC side lives in
// cmd/weft/hosts_cordon_test.go.

import (
	"context"
	"strings"
	"testing"
)

// TestCordon_RegistryToggle exercises the registry's `setCordoned`
// helper : sets the flag, reads it back, clears it.
func TestCordon_RegistryToggle(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	h, _ := reg.register(RegisterHostSpec{Hostname: "h1"})
	if got, _ := reg.lookupByUUID(h.UUID); got.Cordoned {
		t.Errorf("freshly registered host should not be cordoned")
	}
	if err := reg.setCordoned(h.UUID, true); err != nil {
		t.Fatalf("setCordoned(true): %v", err)
	}
	got, _ := reg.lookupByUUID(h.UUID)
	if !got.Cordoned {
		t.Errorf("after setCordoned(true), Cordoned = false")
	}
	if err := reg.setCordoned(h.UUID, false); err != nil {
		t.Fatalf("setCordoned(false): %v", err)
	}
	got, _ = reg.lookupByUUID(h.UUID)
	if got.Cordoned {
		t.Errorf("after setCordoned(false), Cordoned = true")
	}
}

// TestCordon_Idempotent asserts that calling setCordoned with the
// host's current value is a no-op + nil — operator scripts re-run
// cordon/uncordon without crashing the call.
func TestCordon_Idempotent(t *testing.T) {
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	h, _ := reg.register(RegisterHostSpec{Hostname: "h1"})
	// Uncordon-on-already-uncordoned : no-op + nil.
	if err := reg.setCordoned(h.UUID, false); err != nil {
		t.Errorf("idempotent uncordon: %v", err)
	}
	_ = reg.setCordoned(h.UUID, true)
	// Cordon-on-already-cordoned : no-op + nil.
	if err := reg.setCordoned(h.UUID, true); err != nil {
		t.Errorf("idempotent cordon: %v", err)
	}
	// Unknown UUID still rejected.
	if err := reg.setCordoned("nope", true); err == nil {
		t.Errorf("unknown UUID should be rejected")
	}
}

// TestCordon_SchedulerSkips asserts that a cordoned host is
// removed from the candidate pool — the scheduler picks the next
// uncordoned host, not the cordoned one even if it appears first.
func TestCordon_SchedulerSkips(t *testing.T) {
	candidates := []Host{
		activeHost("a", func(h *Host) { h.Cordoned = true }),
		activeHost("b"),
	}
	got, err := FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{}, candidates)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if got.UUID != "b" {
		t.Errorf("cordoned host a should be skipped, got %q", got.UUID)
	}
}

// TestCordon_UncordonReschedules asserts the inverse : after
// uncordon, the previously cordoned host becomes a candidate
// again. Exercises the toggle by mutating the candidate slice
// between two Schedule() calls.
func TestCordon_UncordonReschedules(t *testing.T) {
	candidates := []Host{
		activeHost("a", func(h *Host) { h.Cordoned = true }),
	}
	if _, err := (FirstFitScheduler{}).Schedule(context.Background(), ScheduleRequest{}, candidates); err == nil {
		t.Errorf("only host is cordoned — schedule should fail")
	}
	candidates[0].Cordoned = false
	got, err := FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{}, candidates)
	if err != nil {
		t.Fatalf("schedule after uncordon: %v", err)
	}
	if got.UUID != "a" {
		t.Errorf("after uncordon, host a should be eligible, got %q", got.UUID)
	}
}

// TestCordon_AllCordonedSurfacesReason asserts the failure path
// names the cordon explicitly when every otherwise-active host
// has been cordoned — what a `--dry-run schedule` shows operators.
func TestCordon_AllCordonedSurfacesReason(t *testing.T) {
	candidates := []Host{
		activeHost("a", func(h *Host) { h.Cordoned = true }),
		activeHost("b", func(h *Host) { h.Cordoned = true }),
	}
	_, err := FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{}, candidates)
	if err == nil {
		t.Fatalf("all-cordoned cluster should not schedule")
	}
	if !strings.Contains(err.Error(), "cordoned") {
		t.Errorf("error should mention cordon, got: %v", err)
	}
}

// TestCordon_BeforeQuotaAndGPU asserts the cordon filter runs
// *before* GPU matching — a cordoned host with a matching GPU is
// still skipped, and the failure path doesn't masquerade as a
// GPU-capacity miss.
func TestCordon_BeforeQuotaAndGPU(t *testing.T) {
	candidates := []Host{
		activeHost("a", func(h *Host) {
			h.Cordoned = true
			h.GPUs = []GPU{{Vendor: "nvidia", Model: "H200"}}
		}),
	}
	_, err := FirstFitScheduler{}.Schedule(context.Background(),
		ScheduleRequest{GPU: "h200"}, candidates)
	if err == nil {
		t.Fatalf("cordoned-with-matching-GPU should not schedule")
	}
	// Error must be the cordon-shaped one, NOT a ResourceExhausted GPU error.
	if strings.Contains(err.Error(), "no host satisfies GPU constraint") {
		t.Errorf("cordon filter should run before GPU axis ; got GPU-shaped error: %v", err)
	}
	if !strings.Contains(err.Error(), "cordoned") {
		t.Errorf("error should mention cordon, got: %v", err)
	}
}

// TestCordon_GroupScheduleSkipsCordoned asserts the multi-replica
// picker honours cordon the same way Schedule does — a cordoned
// host is invisible to ScheduleGroup even if it satisfies every
// per-replica constraint.
func TestCordon_GroupScheduleSkipsCordoned(t *testing.T) {
	candidates := []Host{
		activeHost("a", func(h *Host) { h.Cordoned = true }),
		activeHost("b"),
		activeHost("c"),
	}
	hosts, err := FirstFitScheduler{}.ScheduleGroup(context.Background(),
		GroupScheduleRequest{Replicas: 2}, candidates)
	if err != nil {
		t.Fatalf("schedule-group: %v", err)
	}
	for _, h := range hosts {
		if h.UUID == "a" {
			t.Errorf("cordoned host a should not be picked")
		}
	}
}

// TestCordon_AdapterEmitsEvent asserts that toggling cordon on an
// Adapter publishes the corresponding PlatformEvent so audit
// dashboards see the transition. Same pattern the
// host.state_changed event uses.
func TestCordon_AdapterEmitsEvent(t *testing.T) {
	stateDir := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	a := NewWithStorage(stateDir, factory).(*Adapter)
	h, err := a.RegisterHost(RegisterHostSpec{Hostname: "h1"})
	if err != nil {
		t.Fatalf("RegisterHost: %v", err)
	}
	// Subscribe BEFORE the toggle so we see the event.
	sub, cancel := a.EventBus().Subscribe(EventFilter{})
	defer cancel()
	if err := a.SetHostCordoned(h.UUID, true); err != nil {
		t.Fatalf("SetHostCordoned: %v", err)
	}
	select {
	case ev := <-sub:
		if ev.Kind != "host.cordoned" {
			t.Errorf("event kind = %q, want host.cordoned", ev.Kind)
		}
		if ev.Subject != h.UUID {
			t.Errorf("event subject = %q, want %q", ev.Subject, h.UUID)
		}
	default:
		t.Errorf("no event published on cordon transition")
	}
	// Idempotent re-cordon : no second event.
	drainEventBus(sub)
	if err := a.SetHostCordoned(h.UUID, true); err != nil {
		t.Fatalf("idempotent SetHostCordoned: %v", err)
	}
	select {
	case ev := <-sub:
		t.Errorf("idempotent re-cordon emitted event %q — should be a no-op", ev.Kind)
	default:
		// expected
	}
}

// drainEventBus is a tiny helper that empties whatever's already
// buffered in the channel so the next select can assert "no new
// event". Best-effort ; the real test is the immediate-default arm
// of the second select.
func drainEventBus(sub <-chan PlatformEvent) {
	for {
		select {
		case <-sub:
		default:
			return
		}
	}
}
