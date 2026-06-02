package floatingipnat

import (
	"context"
	"sync"
	"testing"

	weft "github.com/openweft/weft"
)

// fakeScope is a hand-rolled Scope driven by literal slices.
type fakeScope struct {
	vmsByHost  map[string][]weft.VM
	fips       []weft.FloatingIP
	portsByVM  map[string][]weft.Port
	vmsByName  map[string]weft.VM
}

func (f *fakeScope) ListVMsForHost(h string) []weft.VM       { return f.vmsByHost[h] }
func (f *fakeScope) ListFloatingIPs() []weft.FloatingIP      { return f.fips }
func (f *fakeScope) ListPortsForVM(uuid string) []weft.Port  { return f.portsByVM[uuid] }
func (f *fakeScope) VMByName(name string) (weft.VM, bool)    { v, ok := f.vmsByName[name]; return v, ok }

// recorderReconciler captures every Apply payload for assertion.
type recorderReconciler struct {
	mu      sync.Mutex
	applied [][]NATMapping
}

func (r *recorderReconciler) Apply(m []NATMapping) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]NATMapping, len(m))
	copy(cp, m)
	r.applied = append(r.applied, cp)
	return nil
}

func (r *recorderReconciler) calls() [][]NATMapping {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]NATMapping, len(r.applied))
	for i, c := range r.applied {
		cp := make([]NATMapping, len(c))
		copy(cp, c)
		out[i] = cp
	}
	return out
}

func TestComputeLocalMappings_PicksOnlyLocalActiveVMFIPs(t *testing.T) {
	scope := &fakeScope{
		vmsByHost: map[string][]weft.VM{
			"host-1": {{UUID: "vm-a-uuid", Name: "vm-a"}, {UUID: "vm-b-uuid", Name: "vm-b"}},
		},
		fips: []weft.FloatingIP{
			// Active, local : included.
			{Address: "203.0.113.10", Status: weft.FIPStatusActive, TargetKind: weft.FIPTargetVM, MappedTo: "vm-a"},
			// Active, remote : excluded.
			{Address: "203.0.113.11", Status: weft.FIPStatusActive, TargetKind: weft.FIPTargetVM, MappedTo: "vm-remote"},
			// Available (unmapped) : excluded.
			{Address: "203.0.113.12", Status: weft.FIPStatusAvailable},
			// Active, mapped to local but target_kind = lb : excluded for v0.
			{Address: "203.0.113.13", Status: weft.FIPStatusActive, TargetKind: weft.FIPTargetLB, MappedTo: "vm-a"},
		},
		portsByVM: map[string][]weft.Port{
			"vm-a-uuid": {{UUID: "p1", IP: "10.0.0.5"}},
		},
	}
	got := ComputeLocalMappings(scope, "host-1")
	if len(got) != 1 {
		t.Fatalf("got %d mappings, want 1 : %+v", len(got), got)
	}
	if got[0].PublicIP != "203.0.113.10" || got[0].PrivateIP != "10.0.0.5" || got[0].VMName != "vm-a" {
		t.Errorf("got %+v", got[0])
	}
}

func TestComputeLocalMappings_SkipsVMWithoutPorts(t *testing.T) {
	scope := &fakeScope{
		vmsByHost: map[string][]weft.VM{
			"host-1": {{UUID: "vm-a-uuid", Name: "vm-a"}},
		},
		fips: []weft.FloatingIP{
			{Address: "203.0.113.10", Status: weft.FIPStatusActive, TargetKind: weft.FIPTargetVM, MappedTo: "vm-a"},
		},
		portsByVM: map[string][]weft.Port{}, // no port yet
	}
	if got := ComputeLocalMappings(scope, "host-1"); len(got) != 0 {
		t.Errorf("VM without port must be filtered out, got %+v", got)
	}
}

func TestComputeLocalMappings_SkipsPortWithEmptyIP(t *testing.T) {
	scope := &fakeScope{
		vmsByHost: map[string][]weft.VM{
			"host-1": {{UUID: "vm-a-uuid", Name: "vm-a"}},
		},
		fips: []weft.FloatingIP{
			{Address: "203.0.113.10", Status: weft.FIPStatusActive, TargetKind: weft.FIPTargetVM, MappedTo: "vm-a"},
		},
		portsByVM: map[string][]weft.Port{
			"vm-a-uuid": {{UUID: "p1", IP: ""}, {UUID: "p2", IP: "10.0.0.5"}},
		},
	}
	got := ComputeLocalMappings(scope, "host-1")
	if len(got) != 1 || got[0].PrivateIP != "10.0.0.5" {
		t.Errorf("expected p2's IP picked, got %+v", got)
	}
}

func TestComputeLocalMappings_EmptyHostNoOp(t *testing.T) {
	scope := &fakeScope{}
	if got := ComputeLocalMappings(scope, "host-1"); len(got) != 0 {
		t.Errorf("empty host should yield no mappings, got %+v", got)
	}
}

func TestWatcher_ShouldReact(t *testing.T) {
	w := New("host-1", &fakeScope{}, &recorderReconciler{}, nil)
	reactKinds := []string{
		"floating_ip.mapped", "floating_ip.unmapped", "floating_ip.released",
		"vm.created", "vm.deleted", "vm.migrated", "vm.state_changed",
		"port.created", "port.deleted",
	}
	for _, k := range reactKinds {
		if !w.shouldReact(weft.PlatformEvent{Kind: k}) {
			t.Errorf("kind %q should trigger reconcile", k)
		}
	}
	ignoreKinds := []string{
		"floating_ip.allocated",
		"vm.state.running", "vm.renamed", "vm.registered",
		"port.wireguard_key_rotated",
		"security_group.rules_updated",
		"host.registered",
		"",
	}
	for _, k := range ignoreKinds {
		if w.shouldReact(weft.PlatformEvent{Kind: k}) {
			t.Errorf("kind %q should NOT trigger reconcile", k)
		}
	}
}

func TestWatcher_RunInitialSyncThenEventDriven(t *testing.T) {
	scope := &fakeScope{
		vmsByHost: map[string][]weft.VM{
			"host-1": {{UUID: "vm-a-uuid", Name: "vm-a"}},
		},
		fips: []weft.FloatingIP{
			{Address: "203.0.113.10", Status: weft.FIPStatusActive, TargetKind: weft.FIPTargetVM, MappedTo: "vm-a"},
		},
		portsByVM: map[string][]weft.Port{
			"vm-a-uuid": {{UUID: "p1", IP: "10.0.0.5"}},
		},
	}
	rec := &recorderReconciler{}
	w := New("host-1", scope, rec, nil)

	events := make(chan weft.PlatformEvent, 4)
	events <- weft.PlatformEvent{Kind: "floating_ip.allocated"} // ignored
	events <- weft.PlatformEvent{Kind: "floating_ip.mapped"}    // triggers
	events <- weft.PlatformEvent{Kind: "vm.state_changed"}      // triggers
	close(events)

	if err := w.Run(context.Background(), events); err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := rec.calls()
	// 1 initial + 2 from non-ignored events = 3.
	if len(calls) != 3 {
		t.Errorf("expected 3 Apply calls, got %d", len(calls))
	}
	// All calls should carry the same single mapping.
	for i, c := range calls {
		if len(c) != 1 || c[0].PublicIP != "203.0.113.10" {
			t.Errorf("call[%d] = %+v", i, c)
		}
	}
}
