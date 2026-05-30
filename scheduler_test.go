package weft

import (
	"context"
	"strings"
	"testing"
	"time"
)

// helper: minimal active host with the given UUID + tweaks.
func activeHost(uuid string, opts ...func(*Host)) Host {
	h := Host{
		UUID:           uuid,
		Hostname:       "h-" + uuid,
		AZ:             "us-east-1a",
		Architecture:   "arm64",
		Hypervisor:     "apple-vz",
		NetworkTypes:   []string{"nat", "bridged"},
		VolumeBackends: []string{"file"},
		State:          HostStateActive,
		CreatedAt:      time.Now(),
		LastSeenAt:     time.Now(),
	}
	for _, opt := range opts {
		opt(&h)
	}
	return h
}

func TestFirstFitScheduler_NoCandidates(t *testing.T) {
	_, err := FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{}, nil)
	if err == nil {
		t.Errorf("empty candidate list should be rejected")
	}
	if !strings.Contains(err.Error(), "no hosts") {
		t.Errorf("error should mention empty cluster: %v", err)
	}
}

func TestFirstFitScheduler_PicksFirstMatch(t *testing.T) {
	candidates := []Host{
		activeHost("a"),
		activeHost("b"),
		activeHost("c"),
	}
	got, err := FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{}, candidates)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if got.UUID != "a" {
		t.Errorf("first-fit should pick the first candidate, got %q", got.UUID)
	}
}

func TestFirstFitScheduler_SkipsDrainingAndDown(t *testing.T) {
	candidates := []Host{
		activeHost("draining", func(h *Host) { h.State = HostStateDraining }),
		activeHost("down", func(h *Host) { h.State = HostStateDown }),
		activeHost("active"),
	}
	got, err := FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{}, candidates)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if got.UUID != "active" {
		t.Errorf("scheduler should skip draining + down hosts, got %q", got.UUID)
	}
}

func TestFirstFitScheduler_AllInactive(t *testing.T) {
	candidates := []Host{
		activeHost("d", func(h *Host) { h.State = HostStateDraining }),
		activeHost("down", func(h *Host) { h.State = HostStateDown }),
	}
	_, err := FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{}, candidates)
	if err == nil {
		t.Errorf("all-inactive cluster should produce error")
	}
	if !strings.Contains(err.Error(), "no active hosts") {
		t.Errorf("error should mention no active hosts: %v", err)
	}
}

func TestFirstFitScheduler_ArchitectureFilter(t *testing.T) {
	candidates := []Host{
		activeHost("intel", func(h *Host) { h.Architecture = "amd64" }),
		activeHost("apple", func(h *Host) { h.Architecture = "arm64" }),
	}
	got, _ := FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{Architecture: "arm64"}, candidates)
	if got.UUID != "apple" {
		t.Errorf("arch=arm64 should pick the arm64 host, got %q", got.UUID)
	}
	sched := FirstFitScheduler{}
	if _, err := sched.Schedule(context.Background(), ScheduleRequest{Architecture: "riscv64"}, candidates); err == nil {
		t.Errorf("unknown arch should yield no match")
	}
}

func TestFirstFitScheduler_HypervisorFilter(t *testing.T) {
	candidates := []Host{
		activeHost("kvm", func(h *Host) { h.Hypervisor = "qemu-kvm" }),
		activeHost("vz", func(h *Host) { h.Hypervisor = "apple-vz" }),
	}
	got, _ := FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{Hypervisor: "apple-vz"}, candidates)
	if got.UUID != "vz" {
		t.Errorf("hypervisor=apple-vz should pick the apple-vz host, got %q", got.UUID)
	}
}

func TestFirstFitScheduler_MultiDriverHost(t *testing.T) {
	// Apple Silicon cross-arch build host : VZ covers native arm64,
	// QEMU covers foreign archs. Same host, different driver per arch.
	dualHost := activeHost("mac-build", func(h *Host) {
		h.Hypervisor = ""    // Drivers is authoritative when non-empty
		h.Architecture = ""  // ditto
		h.Drivers = []HostDriver{
			{Kind: "vz", Arches: []string{"arm64"}},
			{Kind: "qemu", Arches: []string{"amd64", "riscv64", "loongarch64"}},
		}
	})
	candidates := []Host{dualHost}

	// arm64 request matches via VZ.
	got, err := FirstFitScheduler{}.Schedule(context.Background(),
		ScheduleRequest{Architecture: "arm64", Hypervisor: "vz"}, candidates)
	if err != nil || got.UUID != "mac-build" {
		t.Errorf("arm64+vz should match the dual-driver host, got %q err=%v", got.UUID, err)
	}

	// amd64 request matches via QEMU on the same host.
	got, err = FirstFitScheduler{}.Schedule(context.Background(),
		ScheduleRequest{Architecture: "amd64", Hypervisor: "qemu"}, candidates)
	if err != nil || got.UUID != "mac-build" {
		t.Errorf("amd64+qemu should match the dual-driver host, got %q err=%v", got.UUID, err)
	}

	// arm64 + qemu pairing on this host has no driver — QEMU's arch
	// set excludes arm64 here (VZ owns it natively).
	sched := FirstFitScheduler{}
	if _, err := sched.Schedule(context.Background(),
		ScheduleRequest{Architecture: "arm64", Hypervisor: "qemu"}, candidates); err == nil {
		t.Error("arm64+qemu shouldn't match a host where QEMU's arch list excludes arm64")
	}

	// Architecture-only request : works as long as ANY driver on the host
	// claims that arch.
	got, err = FirstFitScheduler{}.Schedule(context.Background(),
		ScheduleRequest{Architecture: "riscv64"}, candidates)
	if err != nil || got.UUID != "mac-build" {
		t.Errorf("riscv64 alone should match (QEMU covers it), got %q err=%v", got.UUID, err)
	}
}

func TestFirstFitScheduler_AZFilter(t *testing.T) {
	candidates := []Host{
		activeHost("east", func(h *Host) { h.AZ = "us-east-1a" }),
		activeHost("west", func(h *Host) { h.AZ = "us-west-2c" }),
	}
	got, _ := FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{AZ: "us-west-2c"}, candidates)
	if got.UUID != "west" {
		t.Errorf("AZ filter wrong: got %q", got.UUID)
	}
}

func TestFirstFitScheduler_NetworkTypesFilter(t *testing.T) {
	candidates := []Host{
		activeHost("nat-only", func(h *Host) { h.NetworkTypes = []string{"nat"} }),
		activeHost("mesh-capable", func(h *Host) { h.NetworkTypes = []string{"nat", "mesh"} }),
	}
	got, _ := FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{NetworkTypes: []string{"mesh"}}, candidates)
	if got.UUID != "mesh-capable" {
		t.Errorf("mesh requirement should pick mesh-capable host, got %q", got.UUID)
	}
	// Asking for ALL of multiple types — host must support every one.
	got, _ = FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{NetworkTypes: []string{"nat", "mesh"}}, candidates)
	if got.UUID != "mesh-capable" {
		t.Errorf("multi-type requirement should pick host supporting both, got %q", got.UUID)
	}
}

func TestFirstFitScheduler_VolumeBackendsFilter(t *testing.T) {
	candidates := []Host{
		activeHost("file-only", func(h *Host) { h.VolumeBackends = []string{"file"} }),
		activeHost("ceph", func(h *Host) { h.VolumeBackends = []string{"file", "ceph"} }),
	}
	got, _ := FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{VolumeBackends: []string{"ceph"}}, candidates)
	if got.UUID != "ceph" {
		t.Errorf("ceph requirement should pick ceph-capable host, got %q", got.UUID)
	}
}

func TestFirstFitScheduler_LabelSelectorsFilter(t *testing.T) {
	candidates := []Host{
		activeHost("nogpu", func(h *Host) { h.Labels = map[string]string{"gpu": "none"} }),
		activeHost("h100", func(h *Host) { h.Labels = map[string]string{"gpu": "h100"} }),
		activeHost("a100", func(h *Host) { h.Labels = map[string]string{"gpu": "a100"} }),
	}
	got, _ := FirstFitScheduler{}.Schedule(context.Background(), ScheduleRequest{
		LabelSelectors: map[string]string{"gpu": "h100"},
	}, candidates)
	if got.UUID != "h100" {
		t.Errorf("label selector gpu=h100 should pick that host, got %q", got.UUID)
	}
	// Missing label is not a match (label key absent → empty string, mismatch).
	sched := FirstFitScheduler{}
	if _, err := sched.Schedule(context.Background(), ScheduleRequest{
		LabelSelectors: map[string]string{"gpu": "h100", "ssd": "true"},
	}, candidates); err == nil {
		t.Errorf("compound selector with absent key should yield no match")
	}
}

// TestAdapter_ScheduleVM_RoutesThroughHostRegistry exercises the
// Adapter integration: ScheduleVM consults a.Hosts() (which
// includes the self-registered host) and returns the first
// match. Catches any drift between scheduler logic + Adapter
// wiring.
func TestAdapter_ScheduleVM_RoutesThroughHostRegistry(t *testing.T) {
	stateDir := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	a := NewWithStorage(stateDir, factory).(*Adapter)

	// The self-registered host is the only candidate today.
	got, err := a.ScheduleVM(context.Background(), ScheduleRequest{
		Hypervisor: "apple-vz",
	})
	if err != nil {
		t.Fatalf("ScheduleVM: %v", err)
	}
	if got.Hypervisor != "apple-vz" {
		t.Errorf("scheduled host hypervisor = %q, want apple-vz", got.Hypervisor)
	}
	// Requesting an architecture the self-registered host doesn't
	// have should fail.
	if _, err := a.ScheduleVM(context.Background(), ScheduleRequest{
		Architecture: "riscv64-no-such-host",
	}); err == nil {
		t.Errorf("impossible architecture should yield error")
	}
}

// TestAdapter_SetScheduler swaps the policy at runtime — useful
// for tests that want a forced outcome or for operators picking
// a different default policy via weft.hcl.
func TestAdapter_SetScheduler(t *testing.T) {
	stateDir := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	a := NewWithStorage(stateDir, factory).(*Adapter)

	called := false
	a.SetScheduler(funcScheduler(func(ctx context.Context, req ScheduleRequest, candidates []Host) (Host, error) {
		called = true
		if len(candidates) > 0 {
			return candidates[0], nil
		}
		return Host{}, nil
	}))
	if _, err := a.ScheduleVM(context.Background(), ScheduleRequest{}); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if !called {
		t.Errorf("custom scheduler not invoked")
	}
	// nil restores default (FirstFitScheduler).
	a.SetScheduler(nil)
	if _, ok := a.scheduler.(FirstFitScheduler); !ok {
		t.Errorf("SetScheduler(nil) should restore FirstFitScheduler, got %T", a.scheduler)
	}
}

// funcScheduler adapts a closure to the Scheduler interface —
// test-only helper. ScheduleGroup delegates to FirstFitScheduler
// since this helper is only used to exercise the single-VM
// Schedule path; tests that want custom group behaviour build a
// dedicated mock.
type funcScheduler func(ctx context.Context, req ScheduleRequest, candidates []Host) (Host, error)

func (f funcScheduler) Schedule(ctx context.Context, req ScheduleRequest, candidates []Host) (Host, error) {
	return f(ctx, req, candidates)
}

func (f funcScheduler) ScheduleGroup(ctx context.Context, req GroupScheduleRequest, candidates []Host) ([]Host, error) {
	return FirstFitScheduler{}.ScheduleGroup(ctx, req, candidates)
}

// TestScheduleGroup_AntiAffinityAZ confirms the canonical HA
// shape: 3 replicas, each forced onto a distinct AZ. The
// FirstFitScheduler scans candidates in order and picks one host
// per AZ that hasn't been used yet.
func TestScheduleGroup_AntiAffinityAZ(t *testing.T) {
	candidates := []Host{
		activeHost("a", func(h *Host) { h.AZ = "dc1" }),
		activeHost("b", func(h *Host) { h.AZ = "dc1" }), // same AZ as a — should be skipped
		activeHost("c", func(h *Host) { h.AZ = "dc2" }),
		activeHost("d", func(h *Host) { h.AZ = "dc3" }),
	}
	req := GroupScheduleRequest{
		Replicas:  3,
		Placement: PlacementRule{AZ: ProximityDifferent},
	}
	got, err := FirstFitScheduler{}.ScheduleGroup(context.Background(), req, candidates)
	if err != nil {
		t.Fatalf("ScheduleGroup: %v", err)
	}
	gotAZs := []string{got[0].AZ, got[1].AZ, got[2].AZ}
	want := []string{"dc1", "dc2", "dc3"}
	for i := range want {
		if gotAZs[i] != want[i] {
			t.Errorf("replica %d AZ = %q, want %q (full = %v)", i, gotAZs[i], want[i], gotAZs)
		}
	}
}

// TestScheduleGroup_NotEnoughDistinctAZs surfaces the operator
// error: 3 replicas across 3 AZs requested, only 2 distinct AZs
// in the cluster — schedule must fail rather than silently
// double-up.
func TestScheduleGroup_NotEnoughDistinctAZs(t *testing.T) {
	candidates := []Host{
		activeHost("a", func(h *Host) { h.AZ = "dc1" }),
		activeHost("b", func(h *Host) { h.AZ = "dc2" }),
		activeHost("c", func(h *Host) { h.AZ = "dc1" }),
	}
	req := GroupScheduleRequest{
		Replicas:  3,
		Placement: PlacementRule{AZ: ProximityDifferent},
	}
	_, err := FirstFitScheduler{}.ScheduleGroup(context.Background(), req, candidates)
	if err == nil {
		t.Fatal("expected schedule-group error, got nil")
	}
	if !strings.Contains(err.Error(), "replica 3") {
		t.Errorf("error should name the failing replica index: %v", err)
	}
}

// TestScheduleGroup_SameAZ pins the colocate-on-one-AZ shape:
// 3 replicas, AZ=same. The first picked host anchors the AZ;
// subsequent replicas must match it (so they fall on different
// hosts in the same AZ if Host=different, same host otherwise).
func TestScheduleGroup_SameAZ(t *testing.T) {
	candidates := []Host{
		activeHost("a", func(h *Host) { h.AZ = "dc1" }),
		activeHost("b", func(h *Host) { h.AZ = "dc2" }), // wrong AZ — skipped
		activeHost("c", func(h *Host) { h.AZ = "dc1" }),
		activeHost("d", func(h *Host) { h.AZ = "dc1" }),
	}
	req := GroupScheduleRequest{
		Replicas: 3,
		Placement: PlacementRule{
			AZ:   ProximitySame,
			Host: ProximityDifferent, // intra-AZ spread
		},
	}
	got, err := FirstFitScheduler{}.ScheduleGroup(context.Background(), req, candidates)
	if err != nil {
		t.Fatalf("ScheduleGroup: %v", err)
	}
	seen := map[string]bool{}
	for i, h := range got {
		if h.AZ != "dc1" {
			t.Errorf("replica %d landed in %q, want dc1", i, h.AZ)
		}
		if seen[h.UUID] {
			t.Errorf("replica %d collides on host %s (want intra-AZ spread)", i, h.UUID)
		}
		seen[h.UUID] = true
	}
}

// TestScheduleGroup_AntiAffinityHost pins the host-spread shape:
// 3 replicas, Host=different, AZ unconstrained. Each replica
// must land on a distinct host UUID. With only 2 hosts the
// scheduler errors out.
func TestScheduleGroup_AntiAffinityHost(t *testing.T) {
	candidates := []Host{activeHost("a"), activeHost("b")}
	req := GroupScheduleRequest{
		Replicas:  3,
		Placement: PlacementRule{Host: ProximityDifferent},
	}
	_, err := FirstFitScheduler{}.ScheduleGroup(context.Background(), req, candidates)
	if err == nil {
		t.Fatal("expected error: 3 distinct hosts requested but only 2 available")
	}
}

// TestScheduleGroup_NoPlacement_OneReplica pins the
// degenerate-case backward-compat: Replicas=1 with no rule
// behaves like a vanilla Schedule call.
func TestScheduleGroup_NoPlacement_OneReplica(t *testing.T) {
	candidates := []Host{activeHost("a"), activeHost("b")}
	req := GroupScheduleRequest{Replicas: 1}
	got, err := FirstFitScheduler{}.ScheduleGroup(context.Background(), req, candidates)
	if err != nil {
		t.Fatalf("ScheduleGroup: %v", err)
	}
	if len(got) != 1 || got[0].UUID != "a" {
		t.Errorf("got %+v, want exactly one host with UUID=a", got)
	}
}

// TestScheduleGroup_AntiAffinityRack pins the rack-spread shape:
// 3 replicas in one AZ but on distinct racks. Catches the
// single-rack failure-domain that AZ-anti-affinity alone misses.
func TestScheduleGroup_AntiAffinityRack(t *testing.T) {
	candidates := []Host{
		activeHost("a", func(h *Host) { h.AZ = "dc1"; h.Rack = "r1" }),
		activeHost("b", func(h *Host) { h.AZ = "dc1"; h.Rack = "r1" }), // same rack — skipped after a
		activeHost("c", func(h *Host) { h.AZ = "dc1"; h.Rack = "r2" }),
		activeHost("d", func(h *Host) { h.AZ = "dc1"; h.Rack = "r3" }),
	}
	req := GroupScheduleRequest{
		Replicas:  3,
		Placement: PlacementRule{AZ: ProximitySame, Rack: ProximityDifferent, Host: ProximityDifferent},
	}
	got, err := FirstFitScheduler{}.ScheduleGroup(context.Background(), req, candidates)
	if err != nil {
		t.Fatalf("ScheduleGroup: %v", err)
	}
	gotRacks := []string{got[0].Rack, got[1].Rack, got[2].Rack}
	want := []string{"r1", "r2", "r3"}
	for i := range want {
		if gotRacks[i] != want[i] {
			t.Errorf("replica %d rack = %q, want %q (full = %v)", i, gotRacks[i], want[i], gotRacks)
		}
	}
}

// TestScheduleGroup_RackDifferent_MissingRackRejects covers the
// fail-safe: a host with an empty Rack field can't satisfy
// "different rack" because we can't prove it's different from
// the already-picked hosts. Pins the proximityDimOK semantics.
func TestScheduleGroup_RackDifferent_MissingRackRejects(t *testing.T) {
	candidates := []Host{
		activeHost("a", func(h *Host) { h.Rack = "r1" }),
		activeHost("b"), // no rack tag — must NOT count as "different rack"
	}
	req := GroupScheduleRequest{
		Replicas:  2,
		Placement: PlacementRule{Rack: ProximityDifferent},
	}
	_, err := FirstFitScheduler{}.ScheduleGroup(context.Background(), req, candidates)
	if err == nil {
		t.Fatal("expected error: host without Rack can't satisfy 'different rack'")
	}
}

// TestScheduleGroup_InvalidReplicas pins the validation: Replicas
// must be >= 1. Zero or negative is a programming error in the
// caller, surfaced loudly.
func TestScheduleGroup_InvalidReplicas(t *testing.T) {
	_, err := FirstFitScheduler{}.ScheduleGroup(context.Background(), GroupScheduleRequest{Replicas: 0}, []Host{activeHost("a")})
	if err == nil {
		t.Fatal("expected error for Replicas=0")
	}
	if !strings.Contains(err.Error(), "replicas must be") {
		t.Errorf("error should mention the replicas-must-be constraint: %v", err)
	}
}
